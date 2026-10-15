// Package instance reads, writes and checks the per-instance decisions file
// (cvctl.env): what cvctl decided for one <project>-<env> deployment.
package instance

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

type ServiceDecision struct {
	Mode     string // local | external
	Version  string
	Creds    kinds.Credentials
	External map[string]string
}

type Decisions struct {
	Instance    string
	Env         string // dev | prod
	InfraBranch string
	TrackTag    string // dev only
	InfraPath   string
	Strategy    string // recreate | blue-green
	Services    map[string]ServiceDecision
	Domains     map[string]string
	Digests     map[string]string
}

const (
	keyInstance = "CVCTL_INSTANCE"
	keyEnv      = "CVCTL_ENV"
	keyBranch   = "CVCTL_INFRA_BRANCH"
	keyTrack    = "CVCTL_TRACK_TAG"
	keyPath     = "CVCTL_INFRA_PATH"
	keyStrategy = "CVCTL_STRATEGY"
	svcPrefix   = "CVCTL_SVC_"
	domPrefix   = "CVCTL_DOMAIN_"
	digPrefix   = "CVCTL_DIGEST_"
	extInfix    = "_EXT_"
)

func readErr(path string, err error) error {
	return &output.Error{
		Code:    "E_DECISION_READ",
		Message: fmt.Sprintf("cannot read %s: %v", path, err),
		Hint:    "check that the instance exists and the file is a valid dotenv file",
	}
}

// Read maps normalized keys back to service names using p. Keys for services
// that no longer exist in the contract are dropped on purpose.
func Read(path string, p *contract.Project) (*Decisions, error) {
	m, err := godotenv.Read(path)
	if err != nil {
		return nil, readErr(path, err)
	}
	d := &Decisions{
		Instance:    m[keyInstance],
		Env:         m[keyEnv],
		InfraBranch: m[keyBranch],
		TrackTag:    m[keyTrack],
		InfraPath:   m[keyPath],
		Strategy:    m[keyStrategy],
		Services:    map[string]ServiceDecision{},
		Domains:     map[string]string{},
		Digests:     map[string]string{},
	}
	for _, a := range p.Apps {
		n := contract.Normalize(a.Name)
		if v, ok := m[domPrefix+n]; ok {
			d.Domains[a.Name] = v
		}
		if v, ok := m[digPrefix+n]; ok {
			d.Digests[a.Name] = v
		}
	}
	for _, ds := range p.Data {
		pre := svcPrefix + contract.Normalize(ds.Name) + "_"
		var sd ServiceDecision
		found := false
		for k, v := range m {
			if !strings.HasPrefix(k, pre) {
				continue
			}
			rest := strings.TrimPrefix(k, pre)
			switch {
			case rest == "MODE":
				sd.Mode = v
			case rest == "VERSION":
				sd.Version = v
			case rest == "USER":
				sd.Creds.User = v
			case rest == "PASSWORD":
				sd.Creds.Password = v
			case rest == "DB":
				sd.Creds.DB = v
			case strings.HasPrefix(rest, extInfix[1:]):
				if sd.External == nil {
					sd.External = map[string]string{}
				}
				sd.External[strings.ToLower(strings.TrimPrefix(rest, extInfix[1:]))] = v
			default:
				continue
			}
			found = true
		}
		if found {
			d.Services[ds.Name] = sd
		}
	}
	return d, nil
}

func (d *Decisions) entries() map[string]string {
	m := map[string]string{}
	set := func(k, v string) {
		if v != "" {
			m[k] = v
		}
	}
	set(keyInstance, d.Instance)
	set(keyEnv, d.Env)
	set(keyBranch, d.InfraBranch)
	set(keyTrack, d.TrackTag)
	set(keyPath, d.InfraPath)
	set(keyStrategy, d.Strategy)
	for name, s := range d.Services {
		pre := svcPrefix + contract.Normalize(name) + "_"
		set(pre+"MODE", s.Mode)
		set(pre+"VERSION", s.Version)
		set(pre+"USER", s.Creds.User)
		set(pre+"PASSWORD", s.Creds.Password)
		set(pre+"DB", s.Creds.DB)
		for f, v := range s.External {
			set(pre+"EXT_"+strings.ToUpper(f), v)
		}
	}
	for name, v := range d.Domains {
		set(domPrefix+contract.Normalize(name), v)
	}
	for name, v := range d.Digests {
		set(digPrefix+contract.Normalize(name), v)
	}
	return m
}

func safeValue(v string) bool {
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case strings.ContainsRune("_-./:@%+=,", r):
		default:
			return false
		}
	}
	return true
}

// quote double-quotes with explicit escapes: godotenv expands $VAR inside
// double quotes and decodes \n, so both must be escaped to round-trip.
func quote(v string) string {
	if v != "" && safeValue(v) {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, `$`, `\$`, "\n", `\n`, "\r", `\r`)
	return `"` + r.Replace(v) + `"`
}

func writeErr(path string, err error) error {
	return &output.Error{
		Code:    "E_DECISION_WRITE",
		Message: fmt.Sprintf("cannot write %s: %v", path, err),
		Hint:    "check permissions and free space of the instance directory",
	}
}

// Write is atomic (temp file in the same dir + rename) so a crash never
// leaves a half-written decisions file.
func Write(path string, d *Decisions) error {
	m := d.entries()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + quote(m[k]) + "\n")
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cvctl.env.*")
	if err != nil {
		return writeErr(path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return writeErr(path, err)
	}
	if _, err := f.WriteString(b.String()); err != nil {
		return fail(err)
	}
	if err := f.Chmod(0o640); err != nil {
		return fail(err)
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return writeErr(path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return writeErr(path, err)
	}
	return nil
}

// kindUses reports whether any template of the kind references placeholder;
// only referenced credential fields are required for a local service.
func kindUses(k kinds.Kind, placeholder string) bool {
	has := func(v string) bool { return strings.Contains(v, placeholder) }
	for _, v := range k.ContainerEnv {
		if has(v) {
			return true
		}
	}
	for _, v := range k.Env {
		if has(v) {
			return true
		}
	}
	for _, v := range k.Command {
		if has(v) {
			return true
		}
	}
	for _, v := range k.Health {
		if has(v) {
			return true
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Check reports every gap or invalid value; issues are sorted so output is
// deterministic.
func Check(d *Decisions, p *contract.Project, cat kinds.Catalog) []output.Issue {
	var is []output.Issue
	add := func(code, svc, msg, hint string) {
		is = append(is, output.Issue{Code: code, Service: svc, Message: msg, Hint: hint})
	}
	switch d.Env {
	case "":
		add("E_DECISION_MISSING", "", "env is not set", "set CVCTL_ENV to dev or prod")
	case "dev", "prod":
	default:
		add("E_DECISION_INVALID", "", fmt.Sprintf("env %q is not valid", d.Env), "use dev or prod")
	}
	switch d.Strategy {
	case "":
		add("E_DECISION_MISSING", "", "strategy is not set", "set CVCTL_STRATEGY to recreate or blue-green")
	case "recreate", "blue-green":
	default:
		add("E_DECISION_INVALID", "", fmt.Sprintf("strategy %q is not valid", d.Strategy), "use recreate or blue-green")
	}
	for _, a := range p.Apps {
		if a.ExposePort != 0 && d.Domains[a.Name] == "" {
			add("E_DECISION_MISSING", a.Name, "exposed app has no domain", "set CVCTL_DOMAIN_"+contract.Normalize(a.Name))
		}
		if d.Digests[a.Name] == "" {
			add("E_DECISION_MISSING", a.Name, "app has no image digest", "set CVCTL_DIGEST_"+contract.Normalize(a.Name))
		}
	}
	for _, ds := range p.Data {
		s, ok := d.Services[ds.Name]
		pre := svcPrefix + contract.Normalize(ds.Name) + "_"
		if !ok || s.Mode == "" {
			add("E_DECISION_MISSING", ds.Name, "data service has no mode", "set "+pre+"MODE to local or external")
			continue
		}
		kind := cat[ds.Kind]
		switch s.Mode {
		case "local":
			for _, f := range []struct{ name, ph, val string }{
				{"USER", "{user}", s.Creds.User},
				{"PASSWORD", "{password}", s.Creds.Password},
				{"DB", "{db}", s.Creds.DB},
			} {
				if f.val == "" && kindUses(kind, f.ph) {
					add("E_DECISION_MISSING", ds.Name, "local service credential "+strings.ToLower(f.name)+" is not set", "set "+pre+f.name)
				}
			}
			if s.Version != "" && len(kind.Versions) > 0 && !contains(kind.Versions, s.Version) {
				add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("version %q is not available for kind %s", s.Version, ds.Kind),
					"use one of: "+strings.Join(kind.Versions, ", "))
			}
		case "external":
			if !ds.ExternalAllowed {
				add("E_DECISION_EXTERNAL_NOT_ALLOWED", ds.Name, "external mode is not allowed for this service",
					"add label cvctl.external: allowed to the service or use local mode")
				continue
			}
			for _, f := range kind.External {
				if s.External[f] == "" {
					add("E_DECISION_MISSING", ds.Name, fmt.Sprintf("external field %q is not set", f),
						"set "+pre+"EXT_"+strings.ToUpper(f))
				}
			}
		default:
			add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("mode %q is not valid", s.Mode), "use local or external")
		}
	}
	sort.SliceStable(is, func(i, j int) bool {
		a, b := is[i], is[j]
		if a.Code != b.Code {
			return a.Code < b.Code
		}
		if a.Service != b.Service {
			return a.Service < b.Service
		}
		return a.Message < b.Message
	})
	return is
}
