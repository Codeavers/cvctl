package contract

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

// Validate checks the contract rules. A nil spec skips the env checks (the
// caller reports the missing or invalid spec itself). The result is sorted by
// code, service, message so output is deterministic.
func Validate(p *Project, cat kinds.Catalog, spec *envspec.Spec) []output.Issue {
	var out []output.Issue
	add := func(code, svc, msg, hint string) {
		out = append(out, output.Issue{Code: code, Service: svc, Message: msg, Hint: hint})
	}

	isApp := map[string]bool{}
	for _, a := range p.Apps {
		isApp[a.Name] = true
	}
	isData := map[string]bool{}
	for _, d := range p.Data {
		isData[d.Name] = true
	}

	seen := map[string]string{}
	for name, svc := range p.Compose.Services {
		if prev, ok := seen[Normalize(name)]; ok {
			a, b := prev, name
			if a > b {
				a, b = b, a
			}
			add("E_CONTRACT_NAME_COLLISION", b, fmt.Sprintf("services %s and %s normalize to the same name %s", a, b, Normalize(name)),
				"rename one of the services so their names differ after upper-casing and mapping - and . to _")
		}
		seen[Normalize(name)] = name

		if svc.Labels[LabelRole] == "" && svc.Labels[LabelKind] == "" {
			add("E_CONTRACT_NO_ROLE", name, "service has neither cvctl.role nor cvctl.kind",
				fmt.Sprintf("add label %s: app|worker, or %s: <kind>", LabelRole, LabelKind))
		}
		if len(svc.Ports) > 0 {
			add("E_CONTRACT_PORTS_PUBLISHED", name, "service publishes host ports",
				"remove ports: and use expose: so the proxy reaches it over the docker network")
		}
		if isApp[name] {
			if svc.Deploy == nil || svc.Deploy.Resources.Limits == nil || svc.Deploy.Resources.Limits.MemoryBytes == 0 {
				add("E_CONTRACT_NO_MEMORY_LIMIT", name, "app service has no memory limit",
					"set deploy.resources.limits.memory, e.g. 256M")
			}
			for dep := range svc.DependsOn {
				if isData[dep] {
					add("E_CONTRACT_DEPENDS_ON_DATA", name, fmt.Sprintf("depends_on targets data service %s", dep),
						"remove depends_on; cvctl orders data services before apps")
				}
			}
		}
	}

	for _, d := range p.Data {
		k, ok := cat[d.Kind]
		if !ok {
			names := make([]string, 0, len(cat))
			for n := range cat {
				names = append(names, n)
			}
			sort.Strings(names)
			add("E_CONTRACT_UNKNOWN_KIND", d.Name, fmt.Sprintf("unknown kind %q", d.Kind),
				"use one of: "+strings.Join(names, ", "))
		} else if _, err := k.ImageFor(d.Version); err != nil {
			add("E_CONTRACT_KIND_VERSION", d.Name, fmt.Sprintf("kind %s has no version %q", d.Kind, d.Version),
				"use one of: "+strings.Join(k.Versions, ", "))
		}
		if d.EnvPrefix == "" {
			add("E_CONTRACT_NO_ENV_PREFIX", d.Name, "data service lacks cvctl.env",
				fmt.Sprintf("add label %s: <PREFIX> for the generated env vars", LabelEnv))
		}
	}

	if !isApp[p.X.Health.Service] {
		add("E_CONTRACT_NO_HEALTH", "", "x-cvctl.health is missing or names a service that is not an app service",
			"set x-cvctl.health.service to an app service and health.http to its health path")
	}
	for _, hooks := range [][]Hook{p.X.Hooks.PreDeploy, p.X.Hooks.PostDeploy} {
		for _, h := range hooks {
			if !isApp[h.Service] {
				add("E_CONTRACT_HOOK_SERVICE", h.Service, fmt.Sprintf("hook service %q is not an app service", h.Service),
					"point the hook at a service labelled cvctl.role")
			}
		}
	}

	if spec != nil {
		for _, ref := range EnvRefs(p.RawCompose) {
			if _, ok := spec.Vars[ref]; !ok {
				add("E_CONTRACT_ENV_UNDECLARED", "", fmt.Sprintf("compose references %s which env.spec.yml does not declare", ref),
					fmt.Sprintf("add %s under vars: in env.spec.yml", ref))
			}
		}
	}

	slices.SortFunc(out, func(a, b output.Issue) int {
		return strings.Compare(a.Code+"\x00"+a.Service+"\x00"+a.Message, b.Code+"\x00"+b.Service+"\x00"+b.Message)
	})
	return out
}
