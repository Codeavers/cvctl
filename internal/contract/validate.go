package contract

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

var bucketNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,30}$`)

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
	dataByName := map[string]DataService{}
	for _, d := range p.Data {
		isData[d.Name] = true
		dataByName[d.Name] = d
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
			svc := p.Compose.Services[d.Name]
			if k.Buckets {
				if rawExpose, ok := svc.Labels[LabelExpose]; ok {
					if d.ExposePort != k.Port {
						add("E_CONTRACT_EXPOSE_DATA", d.Name,
							fmt.Sprintf("data service %s exposes port %q, but kind %s uses port %d", d.Name, rawExpose, d.Kind, k.Port),
							fmt.Sprintf("set cvctl.expose to %q", strconv.Itoa(k.Port)))
					} else if len(p.PublicBuckets(d.Name)) == 0 {
						add("E_CONTRACT_S3_ROUTE_NO_PUBLIC", d.Name,
							fmt.Sprintf("exposed bucket service %s has no public buckets", d.Name),
							"declare at least one public bucket under x-cvctl.s3 or remove cvctl.expose")
					}
				} else {
					if len(p.PublicBuckets(d.Name)) > 0 {
						add("E_CONTRACT_S3_PUBLIC_NO_ROUTE", d.Name,
							fmt.Sprintf("service %s declares public buckets but has no cvctl.expose label", d.Name),
							"add cvctl.expose to the service or remove public: true from its buckets")
					}
				}
			} else {
				if _, ok := svc.Labels[LabelExpose]; ok {
					add("E_CONTRACT_EXPOSE_DATA", d.Name,
						fmt.Sprintf("data service %s (kind %s) does not support cvctl.expose", d.Name, d.Kind),
						"remove cvctl.expose; only bucket-capable data services can be exposed")
				}
			}
		}
		if d.EnvPrefix == "" {
			add("E_CONTRACT_NO_ENV_PREFIX", d.Name, "data service lacks cvctl.env",
				fmt.Sprintf("add label %s: <PREFIX> for the generated env vars", LabelEnv))
		}
	}

	var s3Services []string
	for svcName := range p.X.S3 {
		s3Services = append(s3Services, svcName)
	}
	sort.Strings(s3Services)

	for _, svcName := range s3Services {
		ds, isDataSvc := dataByName[svcName]
		if !isDataSvc {
			add("E_CONTRACT_S3_NOT_S3", svcName, fmt.Sprintf("service %q in x-cvctl.s3 is not a data service", svcName),
				"point x-cvctl.s3 at a data service whose kind supports buckets")
			continue
		}
		k, hasKind := cat[ds.Kind]
		if !hasKind || !k.Buckets {
			add("E_CONTRACT_S3_NOT_S3", svcName, fmt.Sprintf("service %q has kind %q which does not support buckets", svcName, ds.Kind),
				"use x-cvctl.s3 only with bucket-capable data services")
			continue
		}
		for _, b := range p.X.S3[svcName] {
			if !bucketNameRe.MatchString(b.Name) {
				add("E_CONTRACT_S3_BUCKET_NAME", svcName, fmt.Sprintf("bucket %q name must match ^[a-z][a-z0-9-]{1,30}$", b.Name),
					"use 2 to 31 lowercase alphanumeric characters or hyphens, starting with a letter")
			}
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
				bks := p.Buckets(ds.Name)
				if len(bks) > 0 {
					for key := range k.Env {
						if strings.ToUpper(key) == "BUCKET" {
							continue
						}
						provided = append(provided, ds.EnvPrefix+"_"+strings.ToUpper(key))
					}
					for _, b := range bks {
						provided = append(provided, ds.EnvPrefix+"_BUCKET_"+Normalize(b.Name))
						if b.Public {
							provided = append(provided, ds.EnvPrefix+"_PUBLIC_URL_"+Normalize(b.Name))
						}
					}
				} else {
					for key := range k.Env {
						provided = append(provided, ds.EnvPrefix+"_"+strings.ToUpper(key))
					}
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
