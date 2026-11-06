// Package kinds holds the catalog of data services cvctl can run locally or
// replace with an external provider.
package kinds

import (
	"embed"
	"fmt"
	"io/fs"
	"maps"
	"slices"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/output"
)

//go:embed *.yml
var embedded embed.FS

var backups = []string{"wal-g", "dump", "volume", "none"}

// ValidBackup reports whether b is an accepted backup strategy.
func ValidBackup(b string) bool { return slices.Contains(backups, b) }

type Kind struct {
	Name           string            `yaml:"name"`
	Image          string            `yaml:"image"`
	Versions       []string          `yaml:"versions"`
	DefaultVersion string            `yaml:"default_version"`
	Port           int               `yaml:"port"`
	VolumePath     string            `yaml:"volume_path"`
	ContainerEnv   map[string]string `yaml:"container_env"`
	Command        []string          `yaml:"command"`
	// BackupCommandArgs are appended to Command only when backups are wired.
	BackupCommandArgs []string          `yaml:"backup_command_args"`
	Env               map[string]string `yaml:"env"`
	Backup            string            `yaml:"backup"`
	External          []string          `yaml:"external"`
	Health            []string          `yaml:"health"`
	// RestoreCheck is SQL on the restored cluster for a wal-g kind. For a dump
	// kind it is a shell script run like DumpCommand; for both, the result is a
	// count that must be above zero.
	RestoreCheck string `yaml:"restore_check"`
	Buckets      bool   `yaml:"buckets"`
	// CapAdd are the capabilities a service needs back after cap_drop: [ALL].
	CapAdd []string `yaml:"cap_add"`
	// DumpCommand and RestoreCommand are joined with spaces into one sh -c
	// script run inside the service, so $VAR reads its credentials from the
	// container's own environment and they never reach docker's argv. Pass them
	// to the tool by environment too (MYSQL_PWD="$X" mysqldump), never as
	// -p"$X": the shell expands that into the tool's argv, visible in ps.
	DumpCommand    []string `yaml:"dump_command"`
	RestoreCommand []string `yaml:"restore_command"`
	// Helper names an auxiliary image that takes the dump instead of the
	// service; such a kind declares no commands.
	Helper string `yaml:"helper"`
	// NoContainer marks a kind that runs no service container of its own: a
	// volume mounted in one app under VolumePath/<service>. It needs a Helper and
	// has no external mode, so its env describes a local value only.
	NoContainer bool `yaml:"no_container"`
}

type Catalog map[string]Kind

type Credentials struct {
	User, Password, DB   string
	AppUser, AppPassword string
}

func Load() (Catalog, error) { return LoadFS(embedded) }

func LoadFS(fsys fs.FS) (Catalog, error) {
	files, err := fs.Glob(fsys, "*.yml")
	if err != nil {
		return nil, invalid("", err.Error())
	}
	cat := Catalog{}
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return nil, invalid(f, err.Error())
		}
		var k Kind
		if err := yaml.Unmarshal(data, &k); err != nil {
			return nil, invalid(f, err.Error())
		}
		if k.Name == "" {
			k.Name = strings.TrimSuffix(f, ".yml")
		}
		if err := validate(k); err != nil {
			return nil, err
		}
		cat[k.Name] = k
	}
	return cat, nil
}

func validate(k Kind) error {
	if !slices.Contains(backups, k.Backup) {
		return invalid(k.Name, fmt.Sprintf("backup %q must be one of %v", k.Backup, backups))
	}
	if k.Backup == "dump" && k.Helper == "" && (blank(k.DumpCommand) || blank(k.RestoreCommand)) {
		return invalid(k.Name, "backup dump requires dump_command and restore_command, or helper")
	}
	if !slices.Contains(k.Versions, k.DefaultVersion) {
		return invalid(k.Name, fmt.Sprintf("default_version %q is not in versions", k.DefaultVersion))
	}
	if !strings.Contains(k.Image, "{version}") {
		return invalid(k.Name, "image must contain {version}")
	}
	// Env keys and External fields describe the same variables for local and
	// external mode; a mismatch would make a var exist in only one mode.
	envKeys, extKeys := map[string]bool{}, map[string]bool{}
	for key := range k.Env {
		envKeys[strings.ToUpper(key)] = true
	}
	for _, f := range k.External {
		extKeys[strings.ToUpper(f)] = true
	}
	if k.NoContainer {
		if k.Helper == "" || len(k.External) > 0 {
			return invalid(k.Name, "no_container requires helper and no external fields")
		}
	} else if !maps.Equal(envKeys, extKeys) {
		return invalid(k.Name, "env keys and external fields must name the same variables")
	}
	// A password inside a CMD-SHELL line is parsed by a shell, so it would need
	// quoting; kinds with shell health checks must not put it there. A dump
	// kind's command and health run from docker argv during a restore.
	if k.Backup == "dump" || len(k.Health) > 0 && k.Health[0] == "CMD-SHELL" {
		for _, e := range append(slices.Clone(k.Health), k.Command...) {
			for _, placeholder := range []string{"{password}", "{app_password}"} {
				if strings.Contains(e, placeholder) {
					return invalid(k.Name, fmt.Sprintf("%s must not appear in command or health of a dump kind or when health uses CMD-SHELL", placeholder))
				}
			}
		}
	}
	return nil
}

func blank(cmd []string) bool { return strings.TrimSpace(strings.Join(cmd, "")) == "" }

func invalid(name, msg string) *output.Error {
	return &output.Error{
		Code:    "E_KIND_INVALID",
		Message: fmt.Sprintf("kind %q: %s", name, msg),
		Hint:    "fix the kind definition under kinds/",
	}
}

// Substitute replaces placeholders literally, without escaping.
func (k Kind) Substitute(s string, c Credentials) string {
	return strings.NewReplacer(
		"{user}", c.User, "{password}", c.Password, "{db}", c.DB,
		"{app_user}", c.AppUser, "{app_password}", c.AppPassword,
	).Replace(s)
}

func (k Kind) LocalVars(prefix, host string, c Credentials) map[string]string {
	out := make(map[string]string, len(k.Env))
	for key, tpl := range k.Env {
		user, pass := c.User, c.Password
		appUser, appPass := c.AppUser, c.AppPassword
		if strings.Contains(tpl, "://") {
			user, pass = escapeUserinfo(user), escapeUserinfo(pass)
			appUser, appPass = escapeUserinfo(appUser), escapeUserinfo(appPass)
		}
		v := strings.NewReplacer(
			"{user}", user, "{password}", pass, "{db}", c.DB,
			"{app_user}", appUser, "{app_password}", appPass,
			"{host}", host, "{port}", strconv.Itoa(k.Port),
		).Replace(tpl)
		out[prefix+"_"+strings.ToUpper(key)] = v
	}
	return out
}

func (k Kind) ExternalVars(prefix string, values map[string]string) (map[string]string, error) {
	out := make(map[string]string, len(k.External))
	for _, f := range k.External {
		v, ok := values[f]
		if !ok || v == "" {
			return nil, &output.Error{
				Code:    "E_KIND_EXTERNAL_MISSING",
				Message: fmt.Sprintf("external %s requires field %q", k.Name, f),
				Hint:    fmt.Sprintf("provide all of: %s", strings.Join(k.External, ", ")),
			}
		}
		out[prefix+"_"+strings.ToUpper(f)] = v
	}
	return out, nil
}

func (k Kind) ContainerEnvFor(c Credentials) map[string]string {
	out := make(map[string]string, len(k.ContainerEnv))
	for key, tpl := range k.ContainerEnv {
		out[key] = k.Substitute(tpl, c)
	}
	return out
}

func (k Kind) UsesApp() bool {
	for _, tpl := range k.Env {
		if strings.Contains(tpl, "{app_user}") || strings.Contains(tpl, "{app_password}") {
			return true
		}
	}
	return false
}

func (k Kind) ImageFor(version string) (string, error) {
	if !slices.Contains(k.Versions, version) {
		return "", &output.Error{
			Code:    "E_CONTRACT_KIND_VERSION",
			Message: fmt.Sprintf("kind %s has no version %q", k.Name, version),
			Hint:    fmt.Sprintf("use one of: %s", strings.Join(k.Versions, ", ")),
		}
	}
	return strings.ReplaceAll(k.Image, "{version}", version), nil
}

// escapeUserinfo percent-encodes everything outside the RFC 3986 unreserved
// set; url.UserPassword leaves "/" and others unescaped, which breaks some
// URL parsers in consuming apps.
func escapeUserinfo(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' || c == '_' || c == '~' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}
