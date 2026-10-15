// Package render produces the compose override and Traefik route files for
// one instance. It is pure: callers decide where the bytes are written.
package render

import (
	"bytes"
	"fmt"
	"path/filepath"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

type Options struct {
	InstanceDir string
	Edge        string // edge | behind-proxy
}

func missing(what string) error {
	return &output.Error{
		Code:    "E_DECISION_MISSING",
		Message: fmt.Sprintf("no decision recorded for %s", what),
		Hint:    "run the instance check and fill in the missing decisions before rendering",
	}
}

func marshal(v any) ([]byte, error) {
	// yaml.v3 sorts map keys, which keeps the output deterministic.
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ServiceVars returns, per data service, the env vars it contributes to
// the app env resolution (envspec.Input.ServiceVars).
func ServiceVars(p *contract.Project, d *instance.Decisions, cat kinds.Catalog) (map[string]map[string]string, error) {
	out := map[string]map[string]string{}
	for _, ds := range p.Data {
		sd, ok := d.Services[ds.Name]
		if !ok || sd.Mode == "" {
			return nil, missing("service " + ds.Name)
		}
		k := cat[ds.Kind]
		if sd.Mode == "external" {
			v, err := k.ExternalVars(ds.EnvPrefix, sd.External)
			if err != nil {
				return nil, err
			}
			out[ds.Name] = v
			continue
		}
		out[ds.Name] = k.LocalVars(ds.EnvPrefix, ds.Name, sd.Creds)
	}
	return out, nil
}

func hasProjectMemoryLimit(p *contract.Project, name string) bool {
	svc, ok := p.Compose.Services[name]
	if !ok || svc.Deploy == nil || svc.Deploy.Resources.Limits == nil {
		return false
	}
	return svc.Deploy.Resources.Limits.MemoryBytes > 0
}

func Override(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o Options) ([]byte, error) {
	services := map[string]any{}
	volumes := map[string]any{}

	for _, a := range p.Apps {
		digest := d.Digests[a.Name]
		if digest == "" {
			return nil, missing("digest of " + a.Name)
		}
		nets := map[string]any{"app": map[string]any{}, "data": map[string]any{}}
		if a.ExposePort > 0 {
			nets["edge"] = map[string]any{"aliases": []string{routeName(d, a.Name)}}
		}
		services[a.Name] = map[string]any{
			"image":    a.Image + "@" + digest,
			"env_file": []string{filepath.Join(o.InstanceDir, "app.env")},
			"restart":  "unless-stopped",
			"networks": nets,
		}
	}

	for _, ds := range p.Data {
		sd, ok := d.Services[ds.Name]
		if !ok || sd.Mode == "" {
			return nil, missing("service " + ds.Name)
		}
		if sd.Mode == "external" {
			services[ds.Name] = map[string]any{"profiles": []string{"cvctl-disabled"}}
			continue
		}
		k := cat[ds.Kind]
		version := sd.Version
		if version == "" {
			version = ds.Version
		}
		image, err := k.ImageFor(version)
		if err != nil {
			return nil, err
		}
		svc := map[string]any{
			"image":    image,
			"restart":  "unless-stopped",
			"networks": []string{"data"},
			"volumes":  []string{ds.Name + "-data:" + k.VolumePath},
		}
		if env := k.ContainerEnvFor(sd.Creds); len(env) > 0 {
			svc["environment"] = env
		}
		if len(k.Command) > 0 {
			svc["command"] = substituteAll(k, k.Command, sd.Creds)
		}
		if len(k.Health) > 0 {
			svc["healthcheck"] = map[string]any{"test": substituteAll(k, k.Health, sd.Creds)}
		}
		if !hasProjectMemoryLimit(p, ds.Name) {
			svc["deploy"] = map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "512m"}}}
		}
		services[ds.Name] = svc
		volumes[ds.Name+"-data"] = map[string]any{}
	}

	doc := map[string]any{
		"services": services,
		"networks": map[string]any{
			"app":  map[string]any{},
			"data": map[string]any{"internal": true},
			"edge": map[string]any{"external": true, "name": "cvctl-edge"},
		},
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	return marshal(doc)
}

func substituteAll(k kinds.Kind, in []string, c kinds.Credentials) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = k.Substitute(s, c)
	}
	return out
}

func routeName(d *instance.Decisions, service string) string {
	return d.Instance + "-" + service
}
