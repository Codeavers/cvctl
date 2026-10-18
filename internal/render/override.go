// Package render produces the compose override and Traefik route files for
// one instance. It is pure: callers decide where the bytes are written.
package render

import (
	"bytes"
	"fmt"
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
}

const backupNetwork = "cvctl-backup"

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
		"WALG_LIBSODIUM_KEY":           b.LibsodiumKey,
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

func hasProjectMemoryLimit(p *contract.Project, name string) bool {
	svc, ok := p.Compose.Services[name]
	if !ok || svc.Deploy == nil || svc.Deploy.Resources.Limits == nil {
		return false
	}
	return svc.Deploy.Resources.Limits.MemoryBytes > 0
}

func Override(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o Options) ([]byte, error) {
	if d.Instance == "" {
		return nil, missing("instance")
	}
	services := map[string]any{}
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
		services[a.Name] = map[string]any{
			"image":    a.Image + "@" + digest,
			"env_file": []string{appEnvPath},
			"restart":  "unless-stopped",
			"networks": nets,
		}
	}

	for _, ds := range p.Data {
		sd, ok := d.Services[ds.Name]
		if !ok || sd.Mode == "" {
			return nil, missing("service " + ds.Name)
		}
		if sd.Mode == "external" {
			services[ds.Name] = map[string]any{"profiles": []string{"cvctl-disabled"}}
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
				m[backupNetwork] = map[string]any{}
			}
			nets = m
		} else if isBackedUp {
			nets = []string{"data", backupNetwork}
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
		if !hasProjectMemoryLimit(p, ds.Name) {
			svc["deploy"] = map[string]any{"resources": map[string]any{"limits": map[string]any{"memory": "512m"}}}
		}
		services[ds.Name] = svc
		volumes[ds.Name+"-data"] = map[string]any{"name": VolumeName(d, ds.Name)}
	}

	doc := map[string]any{
		"services": services,
		"networks": map[string]any{
			"app":  map[string]any{},
			"data": map[string]any{"internal": true},
			"edge": map[string]any{"external": true, "name": "cvctl-edge"},
		},
	}
	if usesBackupNet {
		doc["networks"].(map[string]any)[backupNetwork] = map[string]any{"external": true, "name": backupNetwork}
	}
	if len(volumes) > 0 {
		doc["volumes"] = volumes
	}
	return marshalEscaped(doc)
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

// marshalEscaped doubles every "$" in string values: docker compose
// interpolates the override at deploy time, unlike env_file contents.
func marshalEscaped(v any) ([]byte, error) {
	var n yaml.Node
	if err := n.Encode(v); err != nil {
		return nil, err
	}
	escapeValues(&n)
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
	return d.Instance + "-" + service
}
