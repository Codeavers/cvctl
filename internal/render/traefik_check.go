package render

import (
	"fmt"
	"regexp"
	"slices"
	"sort"

	"gopkg.in/yaml.v3"
)

// RouteRouter is one router of a route file written by Traefik.
type RouteRouter struct {
	Name string
	// Domain is set only for a plain Host(`...`) rule, the one a GET / probe
	// can reach; bucket routers match a path and method too.
	Domain string
}

// RouteCheck is the result of comparing a route file with the host edge mode.
type RouteCheck struct {
	Routers []RouteRouter
	// Problems lists the routers whose entry points or TLS do not match edge.
	Problems []string
}

var hostRuleRe = regexp.MustCompile("^Host\\(`([^`]+)`\\)$")

// CheckRoute parses a route file rendered by Traefik and reports the routers
// that would not serve on the host's current edge mode. A route rendered for
// behind-proxy on an edge host listens only on :80, which redirects to :443
// where no router matches.
func CheckRoute(b []byte, edge string) (RouteCheck, error) {
	var doc struct {
		HTTP struct {
			Routers map[string]struct {
				Rule        string         `yaml:"rule"`
				EntryPoints []string       `yaml:"entryPoints"`
				TLS         map[string]any `yaml:"tls"`
			} `yaml:"routers"`
		} `yaml:"http"`
	}
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return RouteCheck{}, err
	}
	names := make([]string, 0, len(doc.HTTP.Routers))
	for n := range doc.HTTP.Routers {
		names = append(names, n)
	}
	sort.Strings(names)

	var rc RouteCheck
	for _, n := range names {
		r := doc.HTTP.Routers[n]
		rr := RouteRouter{Name: n}
		if m := hostRuleRe.FindStringSubmatch(r.Rule); m != nil {
			rr.Domain = m[1]
		}
		rc.Routers = append(rc.Routers, rr)
		if edge == "edge" {
			if !slices.Contains(r.EntryPoints, "websecure") || r.TLS == nil {
				rc.Problems = append(rc.Problems, fmt.Sprintf("router %s has entryPoints %v and tls %v, but the host is in edge mode (needs websecure with tls)", n, r.EntryPoints, r.TLS != nil))
			}
		} else if !slices.Contains(r.EntryPoints, "web") {
			rc.Problems = append(rc.Problems, fmt.Sprintf("router %s has entryPoints %v, but the host is behind a proxy (needs web)", n, r.EntryPoints))
		}
	}
	return rc, nil
}
