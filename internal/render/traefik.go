package render

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
)

// Traefik renders the file-provider route for every exposed app and exposed
// local bucket service, or nil when none is exposed. Traefik has no Docker
// socket access, so these files are the only source of routes.
func Traefik(p *contract.Project, d *instance.Decisions, o Options) ([]byte, error) {
	if d.Instance == "" {
		return nil, missing("instance")
	}
	routers := map[string]any{}
	services := map[string]any{}
	for _, a := range p.Apps {
		if a.ExposePort <= 0 {
			continue
		}
		domain := d.Domains[a.Name]
		if domain == "" {
			return nil, missing("domain of " + a.Name)
		}
		name := routeName(d, a.Name)
		r := map[string]any{
			"rule":        fmt.Sprintf("Host(`%s`)", domain),
			"service":     name,
			"middlewares": []string{"cvctl-default@file"},
		}
		if o.Edge == "edge" {
			r["entryPoints"] = []string{"websecure"}
			r["tls"] = map[string]any{"certResolver": "le"}
		} else {
			r["entryPoints"] = []string{"web"}
		}
		routers[name] = r
		services[name] = map[string]any{"loadBalancer": map[string]any{
			"servers": []any{map[string]any{"url": fmt.Sprintf("http://%s:%d", name, a.ExposePort)}},
		}}
	}
	for _, ds := range p.Data {
		sd, ok := d.Services[ds.Name]
		if !ok || sd.Mode != "local" {
			continue
		}
		if ds.ExposePort <= 0 {
			continue
		}
		pub := p.PublicBuckets(ds.Name)
		if len(pub) == 0 {
			continue
		}
		domain := d.Domains[ds.Name]
		if domain == "" {
			return nil, missing("domain of " + ds.Name)
		}
		names := make([]string, len(pub))
		for i, b := range pub {
			names[i] = regexp.QuoteMeta(b.Name)
		}
		slices.Sort(names)
		pattern := strings.Join(names, "|")
		name := routeName(d, ds.Name)
		r := map[string]any{
			"rule":        fmt.Sprintf("Host(`%s`) && (Method(`GET`) || Method(`HEAD`)) && PathRegexp(`^/(%s)/.+`)", domain, pattern),
			"service":     name,
			"middlewares": []string{"cvctl-default@file"},
		}
		if o.Edge == "edge" {
			r["entryPoints"] = []string{"websecure"}
			r["tls"] = map[string]any{"certResolver": "le"}
		} else {
			r["entryPoints"] = []string{"web"}
		}
		routers[name] = r
		services[name] = map[string]any{"loadBalancer": map[string]any{
			"servers": []any{map[string]any{"url": fmt.Sprintf("http://%s:%d", name, ds.ExposePort)}},
		}}
	}
	if len(routers) == 0 {
		return nil, nil
	}
	return marshal(map[string]any{"http": map[string]any{"routers": routers, "services": services}})
}
