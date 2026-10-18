// Package render produces the compose override and Traefik route files for
// one instance. It is pure: callers decide where the bytes are written.
package render

import (
	"bytes"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

type Options struct {
	InstanceDir string
	Edge        string // edge | behind-proxy
	AppEnvPath  string
	// BackupEnvDir is where backup-<svc>.env files are referenced from;
	// empty means InstanceDir. Staging renders point it at the staging dir.
	BackupEnvDir string
	// Backup nil means no backups configured: no backup env, no backup network.
	Backup *BackupTarget
}

// BackupTarget is the object storage destination and key material for wal-g.
type BackupTarget struct {
	Host, Endpoint, Bucket, Region, AccessKey, SecretKey, LibsodiumKey, ResticPassword string
	// Local is the host silo: per-instance users are provisioned in it.
	Local bool
	// Instance is set on a per-instance target (BackupTargetFor): AccessKey is
	// that instance's own silo user or external key.
	Instance string
	// HostLibsodiumKey and HostResticPassword decrypt what was written before
	// the instance got its own keys: WAL-G generations and restic repository
	// indexes below KeyGen[svc].
	HostLibsodiumKey, HostResticPassword string
	KeyGen                               map[string]int
}

// BackupTargetFor is the backup target of one instance: its own credentials
// and keys over the host's endpoint, bucket and region, or its own external
// bucket when Backup.Ext is set and the host is not the silo. An instance
// without its own keys keeps the host target, as before per-instance keys.
func BackupTargetFor(host *BackupTarget, d *instance.Decisions) *BackupTarget {
	if host == nil || d == nil {
		return host
	}
	bd := d.Backup
	if !bd.HasKeys() {
		return host
	}
	t := *host
	t.Instance = d.Instance
	t.HostLibsodiumKey, t.HostResticPassword = host.LibsodiumKey, host.ResticPassword
	t.LibsodiumKey, t.ResticPassword = bd.LibsodiumKey, bd.ResticPassword
	t.KeyGen = map[string]int{}
	for name, s := range d.Services {
		if s.BackupKeyGen > 0 {
			t.KeyGen[name] = s.BackupKeyGen
		}
	}
	switch {
	case !host.Local && bd.Ext["access_key"] != "" && bd.Ext["secret_key"] != "":
		t.AccessKey, t.SecretKey = bd.Ext["access_key"], bd.Ext["secret_key"]
		if v := bd.Ext["endpoint"]; v != "" {
			t.Endpoint = v
		}
		if v := bd.Ext["bucket"]; v != "" {
			t.Bucket = v
		}
		if v := bd.Ext["region"]; v != "" {
			t.Region = v
		}
	case host.Local && bd.AccessKey != "" && bd.SecretKey != "":
		t.AccessKey, t.SecretKey = bd.AccessKey, bd.SecretKey
	default:
		// External storage without per-instance credentials: the host's
		// credentials (status warns), but still the instance's keys.
		t.Instance = ""
	}
	return &t
}

// UserPrefix is the part of the bucket the instance's silo user may touch.
func (b *BackupTarget) UserPrefix(inst string) string { return b.Host + "/" + inst }

// libsodiumKey is the key of WAL-G generation gen of svc.
func (b *BackupTarget) libsodiumKey(svc string, gen int) string {
	if b.HostLibsodiumKey != "" && gen < b.KeyGen[svc] {
		return b.HostLibsodiumKey
	}
	return b.LibsodiumKey
}

// ResticRepoIndex is the restic repository svc backs up into: a service that
// got per-instance keys after its first backups moves to a new repository.
func (b *BackupTarget) ResticRepoIndex(svc string) int { return b.KeyGen[svc] }

// ResticEnv is the restic environment of repository index idx of svc.
func (b *BackupTarget) ResticEnv(inst, svc string, idx int) map[string]string {
	repo := "s3:" + b.Endpoint + "/" + b.Bucket + "/" + b.Host + "/" + inst + "/" + svc
	if idx > 0 {
		repo += "/r" + strconv.Itoa(idx)
	}
	pass := b.ResticPassword
	if b.HostResticPassword != "" && idx < b.KeyGen[svc] {
		pass = b.HostResticPassword
	}
	return map[string]string{
		"RESTIC_REPOSITORY":     repo,
		"RESTIC_PASSWORD":       pass,
		"AWS_ACCESS_KEY_ID":     b.AccessKey,
		"AWS_SECRET_ACCESS_KEY": b.SecretKey,
		"AWS_DEFAULT_REGION":    b.Region,
	}
}

// NetworkEdge is the per-instance network shared with Traefik.
func NetworkEdge(inst string) string { return inst + "_edge" }

// NetworkBackup is the per-instance internal network shared with the silo.
func NetworkBackup(inst string) string { return inst + "_backup" }

const backupNetKey = "backup"

// BackupEnvPath is the wal-g env file of one service. WALG_S3_PREFIX differs
// per service, so each local wal-g service gets its own file.
func BackupEnvPath(dir, service string) string {
	return filepath.Join(dir, "backup-"+service+".env")
}

// Prefix is the WAL-G archive of one generation. A restored cluster starts a
// new timeline, so it archives into its own generation instead of mixing
// timelines in one prefix; generation 0 is the legacy location.
func (b *BackupTarget) Prefix(inst, svc string, gen int) string {
	p := "s3://" + b.Bucket + "/" + b.Host + "/" + inst + "/" + svc
	if gen > 0 {
		p += "/g" + strconv.Itoa(gen)
	}
	return p
}

// Env returns the wal-g environment of service svc of instance inst.
func (b *BackupTarget) Env(inst, svc string, gen int) map[string]string {
	return map[string]string{
		"WALG_S3_PREFIX":               b.Prefix(inst, svc, gen),
		"WALG_PREVENT_WAL_OVERWRITE":   "true",
		"AWS_ACCESS_KEY_ID":            b.AccessKey,
		"AWS_SECRET_ACCESS_KEY":        b.SecretKey,
		"AWS_ENDPOINT":                 b.Endpoint,
		"AWS_REGION":                   b.Region,
		"AWS_S3_FORCE_PATH_STYLE":      "true",
		"WALG_LIBSODIUM_KEY":           b.libsodiumKey(svc, gen),
		"WALG_LIBSODIUM_KEY_TRANSFORM": "hex",
		"WALG_COMPRESSION_METHOD":      "zstd",
	}
}

// BackupEnvs returns the backup env contents per service name, only for local
// wal-g services and only when a backup target is configured.
func BackupEnvs(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o Options) map[string]map[string]string {
	out := map[string]map[string]string{}
	if o.Backup == nil {
		return out
	}
	for _, ds := range p.Data {
		if d.Services[ds.Name].Mode == "local" && ds.Backup == "wal-g" {
			out[ds.Name] = o.Backup.Env(d.Instance, ds.Name, d.Services[ds.Name].BackupGen)
		}
	}
	return out
}

// VolumeName is the docker volume of a local data service.
func VolumeName(d *instance.Decisions, service string) string {
	if v := d.Services[service].Volume; v != "" {
		return v
	}
	return d.Instance + "_" + service + "-data"
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
		buckets := p.Buckets(ds.Name)
		if len(buckets) == 0 {
			if sd.Mode == "external" {
				v, err := k.ExternalVars(ds.EnvPrefix, sd.External)
				if err != nil {
					return nil, err
				}
				out[ds.Name] = v
				continue
			}
			out[ds.Name] = k.LocalVars(ds.EnvPrefix, ds.Name, sd.Creds)
			continue
		}

		if sd.Mode == "external" {
			v := make(map[string]string, len(k.External)+len(buckets))
			for _, f := range k.External {
				if f == "bucket" {
					continue
				}
				val, ok := sd.External[f]
				if !ok || val == "" {
					return nil, &output.Error{
						Code:    "E_KIND_EXTERNAL_MISSING",
						Message: fmt.Sprintf("external %s requires field %q", k.Name, f),
						Hint:    fmt.Sprintf("provide all of: %s", strings.Join(k.External, ", ")),
					}
				}
				v[ds.EnvPrefix+"_"+strings.ToUpper(f)] = val
			}
			for _, b := range buckets {
				norm := contract.Normalize(b.Name)
				low := strings.ToLower(b.Name)
				bKey := "bucket_" + low
				val, ok := sd.External[bKey]
				if !ok || val == "" {
					return nil, &output.Error{
						Code:    "E_KIND_EXTERNAL_MISSING",
						Message: fmt.Sprintf("external %s requires field %q", k.Name, bKey),
						Hint:    "provide field " + bKey,
					}
				}
				v[ds.EnvPrefix+"_BUCKET_"+norm] = val
				if b.Public {
					pKey := "public_url_" + low
					pval, ok := sd.External[pKey]
					if !ok || pval == "" {
						return nil, &output.Error{
							Code:    "E_KIND_EXTERNAL_MISSING",
							Message: fmt.Sprintf("external %s requires field %q", k.Name, pKey),
							Hint:    "provide field " + pKey,
						}
					}
					v[ds.EnvPrefix+"_PUBLIC_URL_"+norm] = pval
				}
			}
			out[ds.Name] = v
			continue
		}

		// Local mode with declared buckets: drop <P>_BUCKET, set <P>_BUCKET_<B>
		// and, for public buckets, <P>_PUBLIC_URL_<B>.
		v := k.LocalVars(ds.EnvPrefix, ds.Name, sd.Creds)
		delete(v, ds.EnvPrefix+"_BUCKET")
		for _, b := range buckets {
			norm := contract.Normalize(b.Name)
			v[ds.EnvPrefix+"_BUCKET_"+norm] = b.Name
			if b.Public {
				domain := d.Domains[ds.Name]
				if domain == "" {
					return nil, missing("domain of " + ds.Name)
				}
				v[ds.EnvPrefix+"_PUBLIC_URL_"+norm] = "https://" + domain + "/" + b.Name
			}
		}
		out[ds.Name] = v
	}
	return out, nil
}

// appRawFields are copied verbatim from the project's compose.yml into an
// app service; their ${VAR} references stay for compose to interpolate.
var appRawFields = []string{"environment", "command", "entrypoint", "healthcheck", "labels", "stop_grace_period", "depends_on"}

// copyRaw copies the named fields of the raw project service into svc and
// records them as project-owned so their "$" is not doubled.
func copyRaw(svc map[string]any, raw map[string]any, fields []string, owned map[string]bool) {
	for _, f := range fields {
		if v, ok := raw[f]; ok {
			svc[f] = v
			owned[f] = true
		}
	}
}

// rawResources returns the project's deploy.resources of a service.
func rawResources(raw map[string]any) map[string]any {
	deploy, _ := raw["deploy"].(map[string]any)
	res, _ := deploy["resources"].(map[string]any)
	return res
}

// dataResources is the project's deploy.resources, with the 512m memory limit
// added when the project sets none.
func dataResources(raw map[string]any) (map[string]any, bool) {
	res := rawResources(raw)
	if res == nil {
		return map[string]any{"limits": map[string]any{"memory": "512m"}}, false
	}
	limits, _ := res["limits"].(map[string]any)
	if _, ok := limits["memory"]; ok {
		return res, true
	}
	out := maps.Clone(res)
	nl := maps.Clone(limits)
	if nl == nil {
		nl = map[string]any{}
	}
	nl["memory"] = "512m"
	out["limits"] = nl
	return out, true
}

func hardened(svc map[string]any) {
	svc["security_opt"] = []string{"no-new-privileges:true"}
	svc["cap_drop"] = []string{"ALL"}
}

func Override(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o Options) ([]byte, error) {
	if d.Instance == "" {
		return nil, missing("instance")
	}
	services := map[string]any{}
	owned := map[string]map[string]bool{}
	volumes := map[string]any{}
	usesBackupNet := false

	for _, a := range p.Apps {
		digest := d.Digests[a.Name]
		if digest == "" {
			return nil, missing("digest of " + a.Name)
		}
		nets := map[string]any{"app": map[string]any{}, "data": map[string]any{}}
		if a.ExposePort > 0 {
			nets["edge"] = map[string]any{"aliases": []string{routeName(d, a.Name)}}
		}
		appEnvPath := o.AppEnvPath
		if appEnvPath == "" {
			appEnvPath = filepath.Join(o.InstanceDir, "app.env")
		}
		svc := map[string]any{
			"image":    a.Image + "@" + digest,
			"env_file": []string{appEnvPath},
			"restart":  "unless-stopped",
			"networks": nets,
		}
		own := map[string]bool{}
		raw := contract.RawService(p, a.Name)
		copyRaw(svc, raw, appRawFields, own)
		if res := rawResources(raw); res != nil {
			svc["deploy"] = map[string]any{"resources": res}
			own["deploy"] = true
		}
		hardened(svc)
		services[a.Name] = svc
		owned[a.Name] = own
	}

	for _, ds := range p.Data {
		sd, ok := d.Services[ds.Name]
		if !ok || sd.Mode == "" {
			return nil, missing("service " + ds.Name)
		}
		if sd.Mode == "external" {
			continue
		}
		if err := validateCreds(ds.Name, sd.Creds); err != nil {
			return nil, err
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
		isExposedBucket := k.Buckets && ds.ExposePort > 0
		isBackedUp := o.Backup != nil && (ds.Backup == "wal-g" || ds.Backup == "volume")
		if isBackedUp {
			usesBackupNet = true
		}
		var nets any
		if isExposedBucket {
			m := map[string]any{
				"data": map[string]any{},
				"edge": map[string]any{"aliases": []string{routeName(d, ds.Name)}},
			}
			if isBackedUp {
				m[backupNetKey] = map[string]any{}
			}
			nets = m
		} else if isBackedUp {
			nets = []string{"data", backupNetKey}
		} else {
			nets = []string{"data"}
		}
		svc := map[string]any{
			"image":    image,
			"restart":  "unless-stopped",
			"networks": nets,
			"volumes":  []string{ds.Name + "-data:" + k.VolumePath},
		}
		if o.Backup != nil && ds.Backup == "wal-g" {
			dir := o.BackupEnvDir
			if dir == "" {
				dir = o.InstanceDir
			}
			svc["env_file"] = []string{BackupEnvPath(dir, ds.Name)}
		}
		if env := k.ContainerEnvFor(sd.Creds); len(env) > 0 {
			svc["environment"] = env
		}
		if len(k.Command) > 0 {
			cmd := k.Command
			if o.Backup != nil && ds.Backup == "wal-g" {
				cmd = append(slices.Clone(cmd), k.BackupCommandArgs...)
			}
			svc["command"] = substituteAll(k, cmd, sd.Creds)
		}
		if len(k.Health) > 0 {
			svc["healthcheck"] = map[string]any{"test": substituteAll(k, k.Health, sd.Creds)}
		}
		raw := contract.RawService(p, ds.Name)
		own := map[string]bool{}
		copyRaw(svc, raw, []string{"labels"}, own)
		res, fromProject := dataResources(raw)
		svc["deploy"] = map[string]any{"resources": res}
		own["deploy"] = fromProject
		hardened(svc)
		svc["cap_add"] = k.CapAdd
		services[ds.Name] = svc
		owned[ds.Name] = own
		volumes[ds.Name+"-data"] = map[string]any{"name": VolumeName(d, ds.Name)}
	}

	doc := map[string]any{
		"services": services,
		"networks": map[string]any{
			"app":  map[string]any{},
			"data": map[string]any{"internal": true},
			"edge": map[string]any{"external": true, "name": NetworkEdge(d.Instance)},
		},
	}
	if usesBackupNet {
		doc["networks"].(map[string]any)[backupNetKey] = map[string]any{"external": true, "name": NetworkBackup(d.Instance)}
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	return marshalEscaped(doc, owned)
}

var safeIdent = regexp.MustCompile(`^[A-Za-z0-9_]+$`)

// validateCreds guards the values that end up inside a shell healthcheck
// (postgres uses CMD-SHELL with user and db). Passwords are not restricted:
// they are never interpolated into a shell command line by a kind.
func validateCreds(name string, c kinds.Credentials) error {
	for _, f := range []struct{ field, val string }{{"USER", c.User}, {"DB", c.DB}} {
		if f.val != "" && !safeIdent.MatchString(f.val) {
			key := "CVCTL_SVC_" + contract.Normalize(name) + "_" + f.field
			return &output.Error{
				Code:    "E_DECISION_INVALID",
				Message: fmt.Sprintf("%s of service %s must match [A-Za-z0-9_]+", strings.ToLower(f.field), name),
				Hint:    "set " + key + " to letters, digits and underscores only",
			}
		}
	}
	return nil
}

// marshalEscaped doubles every "$" in cvctl-generated string values: docker
// compose interpolates the file at deploy time, unlike env_file contents.
// owned lists, per service, the fields copied from the project, which keep
// their ${VAR} references for compose to resolve from --env-file.
func marshalEscaped(v map[string]any, owned map[string]map[string]bool) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	top := &n
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		top = n.Content[0]
	}
	for i := 0; i+1 < len(top.Content); i += 2 {
		if top.Content[i].Value != "services" {
			escapeValues(top.Content[i+1])
			continue
		}
		svcs := top.Content[i+1]
		for j := 0; j+1 < len(svcs.Content); j += 2 {
			svc := svcs.Content[j+1]
			skip := owned[svcs.Content[j].Value]
			for k := 0; k+1 < len(svc.Content); k += 2 {
				if !skip[svc.Content[k].Value] {
					escapeValues(svc.Content[k+1])
				}
			}
		}
	}
	return marshal(&n)
}

func escapeValues(n *yaml.Node) {
	switch n.Kind {
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			escapeValues(n.Content[i])
		}
	case yaml.ScalarNode:
		if n.Tag == "!!str" {
			n.Value = strings.ReplaceAll(n.Value, "$", "$$")
		}
	default:
		for _, c := range n.Content {
			escapeValues(c)
		}
	}
}

func substituteAll(k kinds.Kind, in []string, c kinds.Credentials) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = k.Substitute(s, c)
	}
	return out
}

func routeName(d *instance.Decisions, service string) string {
	return d.Instance + "--" + service
}
