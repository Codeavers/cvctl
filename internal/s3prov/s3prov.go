// Package s3prov builds the script that provisions buckets, the anonymous
// read policy of public buckets and the scoped app user inside a local silo
// container, and parses the bucket listing that script prints.
package s3prov

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/codeavers/cvctl/internal/contract"
)

// PolicyName is the silo policy attached to every instance's app user.
const PolicyName = "cvctl-app"

// listMarker separates the provisioning output from the bucket listing.
const listMarker = "@@buckets"

// Script returns the POSIX sh script that provisions buckets and the app user.
// It runs inside the silo container on stdin: the root keys come from the
// container's own MINIO_ROOT_USER/MINIO_ROOT_PASSWORD, so they never leave it,
// and the app password travels in the script, never in argv. Every step is
// idempotent, so the script runs on every deploy.
func Script(buckets []contract.Bucket, appUser, appPassword, policyName string) string {
	bs := append([]contract.Bucket(nil), buckets...)
	sort.Slice(bs, func(i, j int) bool { return bs[i].Name < bs[j].Name })

	var b strings.Builder
	b.WriteString("set -eu\n")
	// The root keys reach mc through the environment and the app secret through
	// stdin: container processes show up in the host's process table, so
	// neither may be an mc argument.
	b.WriteString(`export MC_HOST_local="http://${MINIO_ROOT_USER}:${MINIO_ROOT_PASSWORD}@localhost:9000"` + "\n")
	for _, bk := range bs {
		fmt.Fprintf(&b, "mc mb --ignore-existing local/%s\n", bk.Name)
	}
	for _, bk := range bs {
		if bk.Public {
			fmt.Fprintf(&b, "cat > /tmp/cvctl-anon-%s.json <<'CVCTL_EOF'\n%s\nCVCTL_EOF\n", bk.Name, anonPolicy(bk.Name))
			fmt.Fprintf(&b, "mc anonymous set-json /tmp/cvctl-anon-%s.json local/%s\n", bk.Name, bk.Name)
		} else {
			fmt.Fprintf(&b, "mc anonymous set none local/%s\n", bk.Name)
		}
	}
	fmt.Fprintf(&b, "cat > /tmp/cvctl-app-policy.json <<'CVCTL_EOF'\n%s\nCVCTL_EOF\n", appPolicy(bs))
	fmt.Fprintf(&b, "mc admin policy create local %s /tmp/cvctl-app-policy.json\n", policyName)
	fmt.Fprintf(&b, "printf '%%s\\n' %s | mc admin user add local %s\n", shQuote(appPassword), shQuote(appUser))
	fmt.Fprintf(&b, "case \"$(mc admin user info local %s --json)\" in\n", shQuote(appUser))
	// policyName lists every attached policy, comma-separated.
	fmt.Fprintf(&b, "  *'\"policyName\":\"%[1]s\"'* | *'\"policyName\":\"%[1]s,'* | *',%[1]s\"'* | *',%[1]s,'*) ;;\n", policyName)
	fmt.Fprintf(&b, "  *) mc admin policy attach local %s --user %s ;;\n", policyName, shQuote(appUser))
	b.WriteString("esac\n")
	b.WriteString("rm -f /tmp/cvctl-app-policy.json /tmp/cvctl-anon-*.json\n")
	fmt.Fprintf(&b, "echo %s\n", listMarker)
	b.WriteString("mc ls --json local\n")
	return b.String()
}

// Orphans returns the buckets listed after the script's marker that are not
// declared, sorted. Lines that are not bucket entries are ignored.
func Orphans(listOutput []byte, declared []contract.Bucket) []string {
	want := map[string]bool{}
	for _, b := range declared {
		want[b.Name] = true
	}
	text := string(listOutput)
	i := strings.LastIndex(text, listMarker)
	if i < 0 {
		return nil
	}
	var out []string
	for _, line := range strings.Split(text[i+len(listMarker):], "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var e struct {
			Status string `json:"status"`
			Type   string `json:"type"`
			Key    string `json:"key"`
		}
		if json.Unmarshal([]byte(line), &e) != nil || e.Status != "success" || e.Type != "folder" {
			continue
		}
		name := strings.TrimSuffix(e.Key, "/")
		if name != "" && !want[name] {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

func anonPolicy(bucket string) string {
	return mustJSON(policyDoc{Version: "2012-10-17", Statement: []statement{{
		Effect:    "Allow",
		Principal: &principal{AWS: []string{"*"}},
		Action:    []string{"s3:GetObject"},
		Resource:  []string{"arn:aws:s3:::" + bucket + "/*"},
	}}})
}

func appPolicy(buckets []contract.Bucket) string {
	var objects, roots []string
	for _, b := range buckets {
		objects = append(objects, "arn:aws:s3:::"+b.Name+"/*")
		roots = append(roots, "arn:aws:s3:::"+b.Name)
	}
	return mustJSON(policyDoc{Version: "2012-10-17", Statement: []statement{
		{Effect: "Allow", Action: []string{"s3:GetObject", "s3:PutObject", "s3:DeleteObject"}, Resource: objects},
		{Effect: "Allow", Action: []string{"s3:ListBucket"}, Resource: roots},
	}})
}

type policyDoc struct {
	Version   string      `json:"Version"`
	Statement []statement `json:"Statement"`
}

type statement struct {
	Effect    string                         `json:"Effect"`
	Principal *principal                     `json:"Principal,omitempty"`
	Action    []string                       `json:"Action"`
	Resource  []string                       `json:"Resource"`
	Condition map[string]map[string][]string `json:"Condition,omitempty"`
}

type principal struct {
	AWS []string `json:"AWS"`
}

func mustJSON(v any) string {
	out, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(out)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
