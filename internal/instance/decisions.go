// Package instance reads, writes and checks the per-instance decisions file
// (cvctl.env): what cvctl decided for one <project>-<env> deployment.
package instance

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

type ServiceDecision struct {
	Mode    string // local | external
	Version string
	Volume  string // named docker volume; empty means <instance>_<service>-data
	// BackupGen selects the WAL archive generation (0 is the legacy prefix);
	// BackupGenMax is the highest generation ever used so it is never reused.
	BackupGen    int
	BackupGenMax int
	// BackupKeyGen is the first generation (WAL-G) or restic repository
	// index encrypted with the instance's own keys; older ones use the host's.
	BackupKeyGen int
	Creds        kinds.Credentials
	External     map[string]string
}

type Decisions struct {
	Instance    string
	Env         string // dev | prod
	InfraBranch string
	TrackTag    string // dev only
	InfraPath   string
	InfraSource string // git | oci; empty means git
	InfraRef    string // oci only: artifact repository host/repo, no tag
	Strategy    string // recreate | blue-green
	Services    map[string]ServiceDecision
	Domains     map[string]string
	Digests     map[string]string
	Backup      BackupDecision
}

// BackupDecision is the instance's own backup identity: S3 credentials on the
// host silo (user bk-<inst>) and encryption keys. Ext, when set, points the
// backups at the instance's own external bucket (fields bucket, endpoint,
// region, access_key, secret_key).
type BackupDecision struct {
	AccessKey      string
	SecretKey      string
	LibsodiumKey   string
	ResticPassword string
	Ext            map[string]string
}

// HasKeys reports whether the instance has its own encryption keys.
func (b BackupDecision) HasKeys() bool { return b.LibsodiumKey != "" && b.ResticPassword != "" }

const (
	keyInstance = "CVCTL_INSTANCE"
	keyEnv      = "CVCTL_ENV"
	keyBranch   = "CVCTL_INFRA_BRANCH"
	keyTrack    = "CVCTL_TRACK_TAG"
	keyPath     = "CVCTL_INFRA_PATH"
	keySource   = "CVCTL_INFRA_SOURCE"
	keyRef      = "CVCTL_INFRA_REF"
	keyStrategy = "CVCTL_STRATEGY"
	svcPrefix   = "CVCTL_SVC_"
	domPrefix   = "CVCTL_DOMAIN_"
	digPrefix   = "CVCTL_DIGEST_"
	extInfix    = "_EXT_"

	keyBackupAccess    = "CVCTL_BACKUP_ACCESS_KEY"
	keyBackupSecret    = "CVCTL_BACKUP_SECRET_KEY"
	keyBackupLibsodium = "CVCTL_BACKUP_LIBSODIUM_KEY"
	keyBackupRestic    = "CVCTL_BACKUP_RESTIC_PASSWORD"
	backupExtPrefix    = "CVCTL_BACKUP_EXT_"
)

// Infra sources.
const (
	SourceGit = "git"
	SourceOCI = "oci"
)

// IsOCI reports whether the contract comes from an OCI artifact.
func (d *Decisions) IsOCI() bool { return d != nil && d.InfraSource == SourceOCI }

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
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, readErr(path, err)
	}
	m, err := godotenv.Unmarshal(string(raw))
	if err != nil {
		// godotenv parse errors echo the offending value, and this file holds secrets.
		return nil, &output.Error{
			Code:    "E_DECISION_READ",
			Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", path),
			Hint:    "fix the KEY=value syntax in cvctl.env",
		}
	}
	d := &Decisions{
		Instance:    m[keyInstance],
		Env:         m[keyEnv],
		InfraBranch: m[keyBranch],
		TrackTag:    m[keyTrack],
		InfraPath:   m[keyPath],
		InfraSource: m[keySource],
		InfraRef:    m[keyRef],
		Strategy:    m[keyStrategy],
		Services:    map[string]ServiceDecision{},
		Domains:     map[string]string{},
		Digests:     map[string]string{},
		Backup: BackupDecision{
			AccessKey:      m[keyBackupAccess],
			SecretKey:      m[keyBackupSecret],
			LibsodiumKey:   m[keyBackupLibsodium],
			ResticPassword: m[keyBackupRestic],
		},
	}
	for k, v := range m {
		if f, ok := strings.CutPrefix(k, backupExtPrefix); ok && f != "" {
			if d.Backup.Ext == nil {
				d.Backup.Ext = map[string]string{}
			}
			d.Backup.Ext[strings.ToLower(f)] = v
		}
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
		if ds.ExposePort != 0 {
			n := contract.Normalize(ds.Name)
			if v, ok := m[domPrefix+n]; ok {
				d.Domains[ds.Name] = v
			}
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
			case rest == "VOLUME":
				sd.Volume = v
			case rest == "BACKUP_GEN":
				sd.BackupGen = AtoiNonNeg(v)
			case rest == "BACKUP_GEN_MAX":
				sd.BackupGenMax = AtoiNonNeg(v)
			case rest == "BACKUP_KEY_GEN":
				sd.BackupKeyGen = AtoiNonNeg(v)
			case rest == "USER":
				sd.Creds.User = v
			case rest == "PASSWORD":
				sd.Creds.Password = v
			case rest == "DB":
				sd.Creds.DB = v
			case rest == "APP_USER":
				sd.Creds.AppUser = v
			case rest == "APP_PASSWORD":
				sd.Creds.AppPassword = v
			case strings.HasPrefix(rest, extInfix[1:]):
				if sd.External == nil {
					sd.External = map[string]string{}
				}
				fieldUpper := strings.TrimPrefix(rest, extInfix[1:])
				matched := false
				for _, b := range p.Buckets(ds.Name) {
					norm := contract.Normalize(b.Name)
					low := strings.ToLower(b.Name)
					if fieldUpper == "BUCKET_"+norm {
						sd.External["bucket_"+low] = v
						matched = true
						break
					}
					if fieldUpper == "PUBLIC_URL_"+norm {
						sd.External["public_url_"+low] = v
						matched = true
						break
					}
				}
				if !matched {
					sd.External[strings.ToLower(fieldUpper)] = v
				}
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

// AtoiNonNeg parses a non-negative integer; anything else is 0.
func AtoiNonNeg(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (d *Decisions) Entries() map[string]string {
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
	// git is the default, so git instances keep the file they had.
	if d.InfraSource != SourceGit {
		set(keySource, d.InfraSource)
	}
	set(keyRef, d.InfraRef)
	set(keyStrategy, d.Strategy)
	for name, s := range d.Services {
		pre := svcPrefix + contract.Normalize(name) + "_"
		set(pre+"MODE", s.Mode)
		set(pre+"VERSION", s.Version)
		set(pre+"VOLUME", s.Volume)
		if s.BackupGen > 0 {
			set(pre+"BACKUP_GEN", strconv.Itoa(s.BackupGen))
		}
		if s.BackupGenMax > 0 {
			set(pre+"BACKUP_GEN_MAX", strconv.Itoa(s.BackupGenMax))
		}
		if s.BackupKeyGen > 0 {
			set(pre+"BACKUP_KEY_GEN", strconv.Itoa(s.BackupKeyGen))
		}
		set(pre+"USER", s.Creds.User)
		set(pre+"PASSWORD", s.Creds.Password)
		set(pre+"DB", s.Creds.DB)
		set(pre+"APP_USER", s.Creds.AppUser)
		set(pre+"APP_PASSWORD", s.Creds.AppPassword)
		for f, v := range s.External {
			set(pre+"EXT_"+contract.Normalize(f), v)
		}
	}
	set(keyBackupAccess, d.Backup.AccessKey)
	set(keyBackupSecret, d.Backup.SecretKey)
	set(keyBackupLibsodium, d.Backup.LibsodiumKey)
	set(keyBackupRestic, d.Backup.ResticPassword)
	for f, v := range d.Backup.Ext {
		set(backupExtPrefix+contract.Normalize(f), v)
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
		Code:    "E_FILE_WRITE",
		Message: fmt.Sprintf("cannot write %s: %v", path, err),
		Hint:    "check permissions and free space of the instance directory",
	}
}

// WriteFileAtomic writes via a temp file in the same dir + rename, so a crash
// never leaves a half-written file.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return writeErr(path, err)
	}
	tmp := f.Name()
	fail := func(err error) error {
		f.Close()
		os.Remove(tmp)
		return writeErr(path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fail(err)
	}
	if err := f.Chmod(mode); err != nil {
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

// FormatEnv renders values as dotenv text (sorted keys, values quoted so
// godotenv reads them back unchanged).
func FormatEnv(values map[string]string) []byte {
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + quote(values[k]) + "\n")
	}
	return []byte(b.String())
}

// WriteEnvFile writes values as a dotenv file (mode 0640).
func WriteEnvFile(path string, values map[string]string) error {
	return WriteFileAtomic(path, FormatEnv(values), 0o640)
}

func Write(path string, d *Decisions) error {
	return WriteEnvFile(path, d.Entries())
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

var (
	dnsLabelRe     = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
	instanceNameRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	digestRe       = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	// identRe limits user and db names: they end up inside shell
	// healthchecks (postgres uses CMD-SHELL with user and db).
	identRe   = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
	appUserRe = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	// S3 bucket naming without dots: what the provisioning script creates and
	// what S3_BUCKET hands the app.
	bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	// volumeRe is docker's own volume-name rule.
	volumeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
)

// validHostname accepts lowercase RFC 1123 hostnames. Domains are embedded in
// a Traefik Host(`...`) rule, so anything else could inject rule syntax.
func validHostname(h string) bool {
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(h, ".") {
		if len(l) > 63 || !dnsLabelRe.MatchString(l) {
			return false
		}
	}
	return true
}

func validPublicURL(raw string) bool {
	if strings.HasSuffix(raw, "/") {
		return false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	if u.Hostname() == "" {
		return false
	}
	// The value is a base the app and the probe append "/<key>" to.
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return false
	}
	return true
}

func instName(d *Decisions) string {
	if d.Instance == "" {
		return "<inst>"
	}
	return d.Instance
}

// Check reports every gap or invalid value; issues are sorted so output is
// deterministic.
func Check(d *Decisions, p *contract.Project, cat kinds.Catalog) []output.Issue {
	var is []output.Issue
	add := func(code, svc, msg, hint string) {
		is = append(is, output.Issue{Code: code, Service: svc, Message: msg, Hint: hint})
	}
	switch {
	case d.Instance == "":
		add("E_DECISION_MISSING", "", "instance is not set", "set CVCTL_INSTANCE to <project>-<env>")
	case !instanceNameRe.MatchString(d.Instance):
		add("E_INSTANCE_NAME", "", fmt.Sprintf("instance %q is not a DNS label without '--'", d.Instance),
			"use lowercase letters, digits and hyphens without '--', e.g. myproj-dev")
	case (d.Env == "dev" || d.Env == "prod") && !strings.HasSuffix(d.Instance, "-"+d.Env):
		add("E_INSTANCE_NAME", "", fmt.Sprintf("instance %q does not end with -%s", d.Instance, d.Env),
			"name the instance <project>-"+d.Env)
	}
	switch d.Env {
	case "":
		add("E_DECISION_MISSING", "", "env is not set", "set CVCTL_ENV to dev or prod")
	case "dev", "prod":
	default:
		add("E_DECISION_INVALID", "", fmt.Sprintf("env %q is not valid", d.Env), "use dev or prod")
	}
	switch d.InfraSource {
	case "", SourceGit:
	case SourceOCI:
		if d.InfraRef == "" {
			add("E_DECISION_MISSING", "", "oci infra source has no artifact reference", "set CVCTL_INFRA_REF to host/repository")
		}
	default:
		add("E_DECISION_INVALID", "", fmt.Sprintf("infra source %q is not valid", d.InfraSource), "use git or oci")
	}
	switch d.Strategy {
	case "":
		add("E_DECISION_MISSING", "", "strategy is not set", "set CVCTL_STRATEGY to recreate or blue-green")
	case "recreate", "blue-green":
	default:
		add("E_DECISION_INVALID", "", fmt.Sprintf("strategy %q is not valid", d.Strategy), "use recreate or blue-green")
	}
	for _, a := range p.Apps {
		n := contract.Normalize(a.Name)
		if a.ExposePort != 0 {
			if dom := d.Domains[a.Name]; dom == "" {
				add("E_DECISION_MISSING", a.Name, "exposed app has no domain", "set CVCTL_DOMAIN_"+n)
			} else if !validHostname(dom) {
				add("E_DECISION_INVALID", a.Name, fmt.Sprintf("domain %q is not a valid hostname", dom),
					"set CVCTL_DOMAIN_"+n+" to lowercase labels of letters, digits and hyphens separated by dots")
			}
		}
		if dg := d.Digests[a.Name]; dg == "" {
			add("E_DECISION_MISSING", a.Name, "app has no image digest", "set CVCTL_DIGEST_"+n)
		} else if !digestRe.MatchString(dg) {
			add("E_DECISION_INVALID", a.Name, fmt.Sprintf("digest %q is not sha256:<64 hex>", dg),
				"set CVCTL_DIGEST_"+n+" to the lowercase sha256 image digest")
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
			if d.Env == "prod" && len(p.PublicBuckets(ds.Name)) > 0 {
				inst := d.Instance
				if inst == "" {
					inst = "<inst>"
				}
				add("E_S3_PUBLIC_LOCAL_PROD", ds.Name, fmt.Sprintf("public bucket on local %s service is not allowed in prod", ds.Name),
					fmt.Sprintf("switch %s to external storage: cvctl project update %s --ext %s.endpoint=<url> (the service needs cvctl.external: allowed)", ds.Name, inst, ds.Name))
			}
			if kind.Buckets && len(p.Buckets(ds.Name)) == 0 && s.Creds.DB != "" && !bucketNameRe.MatchString(s.Creds.DB) {
				add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("bucket name %q (from %sDB) is not a valid S3 bucket name", s.Creds.DB, pre),
					"declare the buckets in x-cvctl.s3."+ds.Name+".buckets, or set "+pre+"DB to 3-63 lowercase letters, digits and hyphens")
			}
			if ds.ExposePort != 0 {
				n := contract.Normalize(ds.Name)
				if dom := d.Domains[ds.Name]; dom == "" {
					add("E_DECISION_MISSING", ds.Name, "exposed service has no domain", "cvctl project update "+instName(d)+" (or set CVCTL_DOMAIN_"+n+")")
				} else if !validHostname(dom) {
					add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("domain %q is not a valid hostname", dom),
						"set CVCTL_DOMAIN_"+n+" to lowercase labels of letters, digits and hyphens separated by dots")
				}
			}
			for _, f := range []struct{ name, ph, val string }{
				{"USER", "{user}", s.Creds.User},
				{"PASSWORD", "{password}", s.Creds.Password},
				{"DB", "{db}", s.Creds.DB},
			} {
				if f.val == "" && kindUses(kind, f.ph) {
					add("E_DECISION_MISSING", ds.Name, "local service credential "+strings.ToLower(f.name)+" is not set", "set "+pre+f.name)
				}
			}
			if kind.UsesApp() {
				if s.Creds.AppUser == "" || s.Creds.AppPassword == "" {
					inst := d.Instance
					if inst == "" {
						inst = "<inst>"
					}
					add("E_DECISION_MISSING", ds.Name, "local service app credentials are not set",
						fmt.Sprintf("cvctl project update %s", inst))
				}
				if s.Creds.AppUser != "" && !appUserRe.MatchString(s.Creds.AppUser) {
					add("E_DECISION_INVALID", ds.Name, "app_user must match [A-Za-z0-9_-]+",
						"set "+pre+"APP_USER to letters, digits, underscores and hyphens only")
				}
			}
			for _, f := range []struct{ name, val string }{{"USER", s.Creds.User}, {"DB", s.Creds.DB}} {
				if f.val != "" && !identRe.MatchString(f.val) {
					add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("%s must match [A-Za-z0-9_]+", strings.ToLower(f.name)),
						"set "+pre+f.name+" to letters, digits and underscores only")
				}
			}
			if s.Volume != "" && !volumeRe.MatchString(s.Volume) {
				add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("volume %q is not a valid docker volume name", s.Volume),
					"set "+pre+"VOLUME to letters, digits, underscores, dots and hyphens, starting with a letter or digit")
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
			buckets := p.Buckets(ds.Name)
			if len(buckets) > 0 {
				for _, f := range kind.External {
					if f == "bucket" {
						continue
					}
					if s.External[f] == "" {
						add("E_DECISION_MISSING", ds.Name, fmt.Sprintf("external field %q is not set", f),
							"set "+pre+"EXT_"+contract.Normalize(f))
					}
				}
				for _, b := range buckets {
					bKey := "bucket_" + strings.ToLower(b.Name)
					if s.External[bKey] == "" {
						add("E_DECISION_MISSING", ds.Name, fmt.Sprintf("external field %q is not set", bKey),
							"set "+pre+"EXT_"+contract.Normalize(bKey))
					}
					if b.Public {
						pKey := "public_url_" + strings.ToLower(b.Name)
						if s.External[pKey] == "" {
							add("E_DECISION_MISSING", ds.Name, fmt.Sprintf("external field %q is not set", pKey),
								"set "+pre+"EXT_"+contract.Normalize(pKey))
						}
					}
				}
			} else {
				for _, f := range kind.External {
					if s.External[f] == "" {
						add("E_DECISION_MISSING", ds.Name, fmt.Sprintf("external field %q is not set", f),
							"set "+pre+"EXT_"+contract.Normalize(f))
					}
				}
			}
			var extKeys []string
			for k := range s.External {
				if strings.HasPrefix(k, "public_url_") {
					extKeys = append(extKeys, k)
				}
			}
			sort.Strings(extKeys)
			for _, k := range extKeys {
				val := s.External[k]
				if val != "" && !validPublicURL(val) {
					add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("external field %q must be an absolute http(s) URL without a trailing slash", k),
						"set "+pre+"EXT_"+contract.Normalize(k)+" to an absolute https:// or http:// URL without a trailing slash")
				}
			}
		default:
			add("E_DECISION_INVALID", ds.Name, fmt.Sprintf("mode %q is not valid", s.Mode), "use local or external")
		}
	}
	output.SortIssues(is)
	return is
}

// CheckDir verifies that the instance name in Decisions matches the directory name of cvctl.env.
func CheckDir(d *Decisions, cvctlEnvPath string) *output.Error {
	dir := filepath.Base(filepath.Dir(cvctlEnvPath))
	if d == nil || d.Instance != dir {
		inst := ""
		if d != nil {
			inst = d.Instance
		}
		return &output.Error{
			Code:    "E_INSTANCE_MISMATCH",
			Message: fmt.Sprintf("instance name %q in %s does not match directory %q", inst, cvctlEnvPath, dir),
			Hint:    "ensure CVCTL_INSTANCE matches the directory containing cvctl.env",
		}
	}
	return nil
}
