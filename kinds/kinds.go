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

var backups = []string{"wal-g", "volume", "none"}

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
	RestoreCheck      string            `yaml:"restore_check"`
}

type Catalog map[string]Kind

type Credentials struct {
	User, Password, DB string
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
	if !maps.Equal(envKeys, extKeys) {
		return invalid(k.Name, "env keys and external fields must name the same variables")
	}
	// A password inside a CMD-SHELL line is parsed by a shell, so it would need
	// quoting; kinds with shell health checks must not put it there.
	if len(k.Health) > 0 && k.Health[0] == "CMD-SHELL" {
		for _, e := range append(slices.Clone(k.Health), k.Command...) {
			if strings.Contains(e, "{password}") {
				return invalid(k.Name, "{password} must not appear in command or health when health uses CMD-SHELL")
			}
		}
	}
	return nil
}

func invalid(name, msg string) *output.Error {
	return &output.Error{
		Code:    "E_KIND_INVALID",
		Message: fmt.Sprintf("kind %q: %s", name, msg),
		Hint:    "fix the kind definition under kinds/",
	}
}

// Substitute replaces placeholders literally, without escaping.
func (k Kind) Substitute(s string, c Credentials) string {
	return strings.NewReplacer("{user}", c.User, "{password}", c.Password, "{db}", c.DB).Replace(s)
}

func (k Kind) LocalVars(prefix, host string, c Credentials) map[string]string {
	out := make(map[string]string, len(k.Env))
	for key, tpl := range k.Env {
		user, pass := c.User, c.Password
		if strings.Contains(tpl, "://") {
			user, pass = escapeUserinfo(user), escapeUserinfo(pass)
		}
		v := strings.NewReplacer(
			"{user}", user, "{password}", pass, "{db}", c.DB,
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
