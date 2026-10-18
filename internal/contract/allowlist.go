package contract

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/output"
)

var appAllowedFields = map[string]bool{
	"image":             true,
	"labels":            true,
	"environment":       true,
	"command":           true,
	"entrypoint":        true,
	"healthcheck":       true,
	"deploy":            true,
	"stop_grace_period": true,
	"depends_on":        true,
}

var dataAllowedFields = map[string]bool{
	"labels": true,
	"deploy": true,
}

// CheckFields validates raw compose YAML against the contract field allowlist
// before compose-go normalizes or expands includes/extends.
func CheckFields(raw []byte) []output.Issue {
	invalid := func(msg string) []output.Issue {
		return []output.Issue{{
			Code:    "E_CONTRACT_COMPOSE_INVALID",
			Message: msg,
			Hint:    "compose.yml must be a single YAML document with a top-level mapping",
		}}
	}
	// compose-go merges every document of the file, so the check must see
	// exactly the one it would load.
	var docs []*yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for {
		var n yaml.Node
		err := dec.Decode(&n)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return invalid("compose.yml is not valid YAML: " + err.Error())
		}
		if n.Kind == yaml.DocumentNode && len(n.Content) == 0 {
			continue
		}
		docs = append(docs, &n)
	}
	if len(docs) != 1 {
		return invalid(fmt.Sprintf("compose.yml has %d YAML documents", len(docs)))
	}
	root := docs[0]
	if root.Kind != yaml.DocumentNode || len(root.Content) == 0 || root.Content[0].Kind != yaml.MappingNode {
		return invalid("compose.yml has no top-level mapping")
	}
	doc := root.Content[0]

	var issues []output.Issue
	// Anchors and aliases would let a service reuse a mapping the walk below
	// never visits as a service; compose-go resolves them after this check.
	checkNoAliases(doc, "", &issues)
	add := func(svc, path string) {
		issues = append(issues, output.Issue{
			Code:    "E_CONTRACT_FIELD_FORBIDDEN",
			Service: svc,
			Message: path,
			Hint:    fmt.Sprintf("remove %s; cvctl renders networks, volumes and security options itself", path),
		})
	}

	for i := 0; i+1 < len(doc.Content); i += 2 {
		kNode := doc.Content[i]
		vNode := doc.Content[i+1]
		key := kNode.Value

		if key != "services" && key != "x-cvctl" {
			add("", key)
			continue
		}

		if key == "services" {
			if vNode.Kind != yaml.MappingNode {
				add("", "services")
				continue
			}
			for s := 0; s+1 < len(vNode.Content); s += 2 {
				svcNameNode := vNode.Content[s]
				svcNode := vNode.Content[s+1]
				svcName := svcNameNode.Value
				if svcNode.Kind != yaml.MappingNode {
					add(svcName, "services."+svcName)
					continue
				}

				checkServiceFields(svcName, svcNode, add, &issues)
			}
		}
	}

	output.SortIssues(issues)
	return issues
}

func checkServiceFields(svcName string, svcNode *yaml.Node, add func(svc, path string), portsIssues *[]output.Issue) {
	isData := isDataService(svcNode)
	allowed := appAllowedFields
	if isData {
		allowed = dataAllowedFields
	}

	for f := 0; f+1 < len(svcNode.Content); f += 2 {
		fNode := svcNode.Content[f]
		vNode := svcNode.Content[f+1]
		field := fNode.Value
		path := "services." + svcName + "." + field

		if field == "ports" {
			*portsIssues = append(*portsIssues, output.Issue{
				Code:    "E_CONTRACT_PORTS_PUBLISHED",
				Service: svcName,
				Message: "service publishes host ports",
				Hint:    "remove ports: and use cvctl.expose so the proxy reaches it over the docker network",
			})
			continue
		}
		if !allowed[field] {
			add(svcName, path)
			continue
		}

		if field == "labels" {
			checkLabels(svcName, path, vNode, add)
		} else if field == "deploy" {
			checkDeploy(svcName, path, vNode, add)
		}
	}
}

func isDataService(svcNode *yaml.Node) bool {
	for f := 0; f+1 < len(svcNode.Content); f += 2 {
		if svcNode.Content[f].Value == "labels" {
			lNode := svcNode.Content[f+1]
			if lNode.Kind == yaml.MappingNode {
				for i := 0; i+1 < len(lNode.Content); i += 2 {
					if lNode.Content[i].Value == LabelKind {
						return true
					}
				}
			} else if lNode.Kind == yaml.SequenceNode {
				for _, item := range lNode.Content {
					k := strings.TrimSpace(strings.SplitN(item.Value, "=", 2)[0])
					k = strings.TrimSpace(strings.SplitN(k, ":", 2)[0])
					if k == LabelKind {
						return true
					}
				}
			}
		}
	}
	return false
}

func checkLabels(svcName, path string, lNode *yaml.Node, add func(svc, path string)) {
	if lNode.Kind != yaml.MappingNode && lNode.Kind != yaml.SequenceNode {
		add(svcName, path)
		return
	}
	if lNode.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(lNode.Content); i += 2 {
			lk := lNode.Content[i].Value
			if strings.HasPrefix(lk, "traefik.") {
				add(svcName, path+"."+lk)
			}
		}
	} else if lNode.Kind == yaml.SequenceNode {
		for _, item := range lNode.Content {
			k := strings.TrimSpace(strings.SplitN(item.Value, "=", 2)[0])
			k = strings.TrimSpace(strings.SplitN(k, ":", 2)[0])
			if strings.HasPrefix(k, "traefik.") {
				add(svcName, path+"."+k)
			}
		}
	}
}

func checkDeploy(svcName, path string, dNode *yaml.Node, add func(svc, path string)) {
	if dNode.Kind != yaml.MappingNode {
		add(svcName, path)
		return
	}
	for i := 0; i+1 < len(dNode.Content); i += 2 {
		dk := dNode.Content[i].Value
		dv := dNode.Content[i+1]
		dPath := path + "." + dk
		if dk != "resources" {
			add(svcName, dPath)
			continue
		}
		if dv.Kind != yaml.MappingNode {
			add(svcName, dPath)
			continue
		}
		for r := 0; r+1 < len(dv.Content); r += 2 {
			rk := dv.Content[r].Value
			rv := dv.Content[r+1]
			rPath := dPath + "." + rk
			if rk != "limits" && rk != "reservations" {
				add(svcName, rPath)
				continue
			}
			if rv.Kind != yaml.MappingNode {
				add(svcName, rPath)
				continue
			}
			for lr := 0; lr+1 < len(rv.Content); lr += 2 {
				lrk := rv.Content[lr].Value
				lrPath := rPath + "." + lrk
				if lrk != "memory" && lrk != "cpus" {
					add(svcName, lrPath)
				}
			}
		}
	}
}

func checkNoAliases(n *yaml.Node, path string, issues *[]output.Issue) {
	if n.Anchor != "" || n.Kind == yaml.AliasNode {
		p := path
		if p == "" {
			p = "(document)"
		}
		*issues = append(*issues, output.Issue{
			Code:    "E_CONTRACT_FIELD_FORBIDDEN",
			Message: p + ": YAML anchors and aliases are not allowed",
			Hint:    "write the values out in full; cvctl does not resolve anchors, aliases or merge keys",
		})
		return
	}
	if n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			k := n.Content[i]
			child := path + "." + k.Value
			if path == "" {
				child = k.Value
			}
			if k.Value == "<<" || k.Tag == "!!merge" {
				*issues = append(*issues, output.Issue{
					Code:    "E_CONTRACT_FIELD_FORBIDDEN",
					Message: child + ": YAML merge keys are not allowed",
					Hint:    "write the values out in full; cvctl does not resolve anchors, aliases or merge keys",
				})
				continue
			}
			checkNoAliases(n.Content[i+1], child, issues)
		}
		return
	}
	for i, c := range n.Content {
		checkNoAliases(c, fmt.Sprintf("%s[%d]", path, i), issues)
	}
}
