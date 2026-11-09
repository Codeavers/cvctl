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
// socket access, so these files are the only source of routes. A service in
// o.Pin routes to that URL instead of its network alias.
//
// Each alias and redirect source gets a router of its own, so a domain whose
// DNS does not point here yet only fails its own certificate.
func Traefik(p *contract.Project, d *instance.Decisions, o Options) ([]byte, error) {
	if d.Instance == "" {
		return nil, missing("instance")
	}
	routers := map[string]any{}
	services := map[string]any{}
	middlewares := map[string]any{}
	addRoutes := func(svc string, port int, rule func(host string) string) {
		name := routeName(d, svc)
		routers[name] = router(rule(d.Domains[svc]), name, []string{"cvctl-default@file"}, o)
		for i, h := range instance.SortedAliases(d.Aliases[svc]) {
			routers[fmt.Sprintf("%s--alias-%d", name, i)] = router(rule(h), name, []string{"cvctl-default@file"}, o)
		}
		for i, r := range instance.SortedRedirects(d.Redirects[svc]) {
			rn := fmt.Sprintf("%s--redirect-%d", name, i)
			routers[rn] = router(hostRule(r.From), "noop@internal", []string{"cvctl-default@file", rn}, o)
			middlewares[rn] = map[string]any{"redirectRegex": map[string]any{
				"regex":       `^https?://[^/]+(.*)$`,
				"replacement": "https://" + r.To + "${1}",
				"permanent":   true,
			}}
		}
		services[name] = map[string]any{"loadBalancer": map[string]any{
			"servers": []any{map[string]any{"url": backendURL(o, svc, name, port)}},
		}}
	}
	for _, a := range p.Apps {
		if a.ExposePort <= 0 {
			continue
		}
		if d.Domains[a.Name] == "" {
			return nil, missing("domain of " + a.Name)
		}
		addRoutes(a.Name, a.ExposePort, hostRule)
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
		if d.Domains[ds.Name] == "" {
			return nil, missing("domain of " + ds.Name)
		}
		names := make([]string, len(pub))
		for i, b := range pub {
			names[i] = regexp.QuoteMeta(b.Name)
		}
		slices.Sort(names)
		pattern := strings.Join(names, "|")
		addRoutes(ds.Name, ds.ExposePort, func(host string) string {
			return fmt.Sprintf("Host(`%s`) && (Method(`GET`) || Method(`HEAD`)) && PathRegexp(`^/(%s)/.+`)", host, pattern)
		})
	}
	if len(routers) == 0 {
		return nil, nil
	}
	doc := map[string]any{"routers": routers, "services": services}
	if len(middlewares) > 0 {
		doc["middlewares"] = middlewares
	}
	return marshal(map[string]any{"http": doc})
}

func hostRule(host string) string { return fmt.Sprintf("Host(`%s`)", host) }

func router(rule, service string, mws []string, o Options) map[string]any {
	r := map[string]any{
		"rule":        rule,
		"service":     service,
		"middlewares": mws,
	}
	if o.Edge == "edge" {
		r["entryPoints"] = []string{"websecure"}
		r["tls"] = map[string]any{"certResolver": "le"}
	} else {
		r["entryPoints"] = []string{"web"}
	}
	return r
}

func backendURL(o Options, svc, alias string, port int) string {
	if u := o.Pin[svc]; u != "" {
		return u
	}
	return fmt.Sprintf("http://%s:%d", alias, port)
}
