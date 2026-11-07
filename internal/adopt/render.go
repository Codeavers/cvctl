package adopt

import (
	"strings"

	"gopkg.in/yaml.v3"
)

const prefix = "adopted-"

// Render builds the Traefik dynamic config, or nil with no services. The target
// host is the gateway of Traefik's own network: its address on the host side.
func Render(svcs []Service, edgeMode, gateway string) ([]byte, error) {
	if len(svcs) == 0 {
		return nil, nil
	}
	routers := map[string]any{}
	services := map[string]any{}
	transports := map[string]any{}
	for _, s := range svcs {
		name := prefix + s.Name
		r := map[string]any{
			"rule":        "Host(`" + s.Domain + "`)",
			"service":     name,
			"middlewares": []string{"cvctl-default@file"},
		}
		if edgeMode == "edge" {
			r["entryPoints"] = []string{"websecure"}
			r["tls"] = map[string]any{"certResolver": "le"}
		} else {
			r["entryPoints"] = []string{"web"}
		}
		routers[name] = r
		lb := map[string]any{
			"servers": []any{map[string]any{"url": strings.Replace(s.Target, "://host:", "://"+gateway+":", 1)}},
		}
		if s.InsecureTLS {
			lb["serversTransport"] = name
			transports[name] = map[string]any{"insecureSkipVerify": true}
		}
		services[name] = map[string]any{"loadBalancer": lb}
	}
	doc := map[string]any{"routers": routers, "services": services}
	if len(transports) > 0 {
		doc["serversTransports"] = transports
	}
	return yaml.Marshal(map[string]any{"http": doc})
}
