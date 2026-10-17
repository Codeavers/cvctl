package contract

import (
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

func exposedApp(p *Project, name string) bool {
	for _, a := range p.Apps {
		if a.Name == name && a.ExposePort > 0 {
			return true
		}
	}
	return false
}

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

		if role := svc.Labels[LabelRole]; role != "" && role != "app" && role != "worker" {
			add("E_CONTRACT_ROLE_INVALID", name, fmt.Sprintf("%s %q is not app or worker", LabelRole, role),
				fmt.Sprintf("set %s to app or worker", LabelRole))
		}
		if b, ok := svc.Labels[LabelBackup]; ok && !kinds.ValidBackup(b) {
			add("E_CONTRACT_BACKUP_INVALID", name, fmt.Sprintf("%s %q is not valid", LabelBackup, b),
				fmt.Sprintf("set %s to wal-g, volume or none", LabelBackup))
		}
		if svc.Labels[LabelRole] == "" && svc.Labels[LabelKind] == "" {
			add("E_CONTRACT_NO_ROLE", name, "service has neither cvctl.role nor cvctl.kind",
				fmt.Sprintf("add label %s: app|worker, or %s: <kind>", LabelRole, LabelKind))
		}
		if len(svc.Ports) > 0 {
			add("E_CONTRACT_PORTS_PUBLISHED", name, "service publishes host ports",
				"remove ports: and use expose: so the proxy reaches it over the docker network")
		}
		if isApp[name] {
			if svc.Image == "" {
				add("E_CONTRACT_NO_IMAGE", name, "app service has no image",
					"set image: to the registry image; cvctl deploys by digest and never builds")
			}
			if svc.Deploy == nil || svc.Deploy.Resources.Limits == nil || svc.Deploy.Resources.Limits.MemoryBytes == 0 {
				add("E_CONTRACT_NO_MEMORY_LIMIT", name, "app service has no memory limit",
					"set deploy.resources.limits.memory, e.g. 256M")
			}
			if v, ok := svc.Labels[LabelExpose]; ok {
				if n, err := strconv.Atoi(v); err != nil || n < 1 || n > 65535 {
					add("E_CONTRACT_EXPOSE_INVALID", name, fmt.Sprintf("%s %q is not a port number", LabelExpose, v),
						fmt.Sprintf("set %s to the container port, e.g. \"3000\"", LabelExpose))
				}
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
		} else {
			if _, err := k.ImageFor(d.Version); err != nil {
				add("E_CONTRACT_KIND_VERSION", d.Name, fmt.Sprintf("kind %s has no version %q", d.Kind, d.Version),
					"use one of: "+strings.Join(k.Versions, ", "))
			}
			// wal-g runs postgres tools inside the service; a file-level copy
			// of a live postgres volume is torn.
			switch {
			case d.Backup == "wal-g" && k.Backup != "wal-g":
				add("E_CONTRACT_BACKUP_UNSUPPORTED", d.Name, fmt.Sprintf("kind %s does not support %s wal-g", d.Kind, LabelBackup),
					fmt.Sprintf("set %s to volume or none for this service", LabelBackup))
			case d.Backup == "volume" && k.Backup == "wal-g":
				add("E_CONTRACT_BACKUP_UNSUPPORTED", d.Name, fmt.Sprintf("kind %s cannot be backed up as a volume while it runs", d.Kind),
					fmt.Sprintf("set %s to wal-g or none for this service", LabelBackup))
			}
		}
		if d.EnvPrefix == "" {
			add("E_CONTRACT_NO_ENV_PREFIX", d.Name, "data service lacks cvctl.env",
				fmt.Sprintf("add label %s: <PREFIX> for the generated env vars", LabelEnv))
		}
	}

	if !isApp[p.X.Health.Service] || p.X.Health.HTTP == "" {
		add("E_CONTRACT_NO_HEALTH", "", "x-cvctl.health is missing, empty http, or names a service that is not an app service",
			"set x-cvctl.health.service to an app service and health.http to its health path")
	} else {
		var healthApp *AppService
		for i := range p.Apps {
			if p.Apps[i].Name == p.X.Health.Service {
				healthApp = &p.Apps[i]
				break
			}
		}
		if healthApp != nil && p.X.Health.Port == 0 && healthApp.ExposePort == 0 {
			add("E_CONTRACT_HEALTH_PORT", p.X.Health.Service,
				fmt.Sprintf("health service %q has neither cvctl.expose nor x-cvctl.health.port", p.X.Health.Service),
				fmt.Sprintf("add cvctl.expose to %s or x-cvctl.health.port", p.X.Health.Service))
		}
	}

	for _, hooks := range [][]Hook{p.X.Hooks.PreDeploy, p.X.Hooks.PostDeploy} {
		for _, h := range hooks {
			if !isApp[h.Service] {
				add("E_CONTRACT_HOOK_SERVICE", h.Service, fmt.Sprintf("hook service %q is not an app service", h.Service),
					"point the hook at a service labelled cvctl.role")
			}
		}
	}

	if p.X.Verify != nil && !isData[p.X.Verify.Service] {
		add("E_CONTRACT_VERIFY_SERVICE", p.X.Verify.Service, fmt.Sprintf("x-cvctl.verify.service %q is not a data service", p.X.Verify.Service),
			"point verify.service at a service labelled cvctl.kind")
	}

	if spec != nil {
		dataByName := map[string]DataService{}
		for _, d := range p.Data {
			dataByName[d.Name] = d
		}
		for name, v := range spec.Vars {
			switch v.Source {
			case "service":
				ds, ok := dataByName[v.Service]
				if !ok {
					add("E_CONTRACT_SPEC_SERVICE", v.Service, fmt.Sprintf("var %s: service %q is not a data service", name, v.Service),
						"set service: to a service labelled cvctl.kind")
					continue
				}
				k, ok := cat[ds.Kind]
				if !ok || ds.EnvPrefix == "" {
					continue // reported as unknown kind / missing prefix
				}
				var provided []string
				for key := range k.Env {
					provided = append(provided, ds.EnvPrefix+"_"+strings.ToUpper(key))
				}
				slices.Sort(provided)
				if !slices.Contains(provided, name) {
					add("E_CONTRACT_SPEC_SERVICE_VAR", ds.Name, fmt.Sprintf("var %s is not provided by service %s", name, ds.Name),
						"use one of: "+strings.Join(provided, ", "))
				}
			case "template":
				raw, _ := v.Value.(string)
				for _, ref := range envspec.TemplateRefs(raw) {
					switch ref.Kind {
					case "domain":
						if !exposedApp(p, ref.Name) {
							add("E_CONTRACT_SPEC_TEMPLATE_REF", "", fmt.Sprintf("var %s: {domain:%s} is not an exposed app", name, ref.Name),
								"reference an app service that sets cvctl.expose")
						}
					case "var":
						if _, ok := spec.Vars[ref.Name]; !ok {
							add("E_CONTRACT_SPEC_TEMPLATE_REF", "", fmt.Sprintf("var %s: {var:%s} is not declared in the spec", name, ref.Name),
								fmt.Sprintf("declare %s under vars: or fix the reference", ref.Name))
						}
					}
				}
			}
		}
		for _, ref := range EnvRefs(p.RawCompose) {
			if _, ok := spec.Vars[ref]; !ok {
				add("E_CONTRACT_ENV_UNDECLARED", "", fmt.Sprintf("compose references %s which env.spec.yml does not declare", ref),
					fmt.Sprintf("add %s under vars: in env.spec.yml", ref))
			}
		}
	}

	output.SortIssues(out)
	return out
}
