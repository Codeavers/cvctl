// Package hostcfg reads and writes the host configuration
// (/etc/cvctl/host.env), the per-file secrets (/etc/cvctl/secrets/<name>) and
// defines the ordered questions `cvctl init` asks to fill them.
package hostcfg

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strconv"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const envPrefix = "CVCTL_"

// SecretNames are the files under <dir>/secrets, one per secret.
var SecretNames = []string{
	"telegram_token", "s3_access_key", "s3_secret_key",
	"route53_access_key", "route53_secret_key", "cloudflare_dns_token", "registry_token",
}

// Config mirrors host.env. vals is the source of truth (it distinguishes
// "unset" from a zero value); the typed fields are kept in sync by Set.
type Config struct {
	HostName       string
	EdgeMode       string
	ProxyPort      int
	AdminUser      string
	SSHManage      bool
	FirewallManage bool
	AcmeEmail      string
	AcmeChallenge  string
	Storage        string
	S3Endpoint     string
	S3Bucket       string
	S3Region       string
	Notify         string
	TelegramChatID string
	TelegramThread string
	Registries     string
	RegistryUser   string
	vals           map[string]string
	extra          map[string]string // host.env entries cvctl does not know; kept verbatim on Save
}

var dnsLabelRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

func oneOf(vals ...string) func(string) error {
	return func(v string) error {
		for _, x := range vals {
			if v == x {
				return nil
			}
		}
		return fmt.Errorf("must be one of: %s", strings.Join(vals, ", "))
	}
}

func validBool(v string) error { return oneOf("true", "false")(v) }

func validLabel(v string) error {
	if !dnsLabelRe.MatchString(v) {
		return errors.New("must be a DNS label (lowercase letters, digits and hyphens)")
	}
	return nil
}

func validPort(v string) error {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 || n > 65535 {
		return errors.New("must be a port between 1 and 65535")
	}
	return nil
}

// validators holds the checks for keys with a constrained format; other keys
// are free text.
var validators = map[string]func(string) error{
	"HOST_NAME":       validLabel,
	"EDGE_MODE":       oneOf("edge", "behind-proxy"),
	"PROXY_PORT":      validPort,
	"SSH_MANAGE":      validBool,
	"FIREWALL_MANAGE": validBool,
	"ACME_CHALLENGE":  oneOf("http", "route53", "cloudflare"),
	"STORAGE":         oneOf("local", "s3"),
	"NOTIFY":          oneOf("telegram", "none"),
}

var knownKeys = []string{
	"HOST_NAME", "EDGE_MODE", "PROXY_PORT", "ADMIN_USER", "SSH_MANAGE", "FIREWALL_MANAGE",
	"ACME_EMAIL", "ACME_CHALLENGE", "STORAGE", "S3_ENDPOINT", "S3_BUCKET", "S3_REGION",
	"NOTIFY", "TELEGRAM_CHAT_ID", "TELEGRAM_THREAD_ID", "REGISTRIES", "REGISTRY_USER",
}

func invalid(key string, err error) error {
	return &output.Error{
		Code:    "E_HOSTCFG_INVALID",
		Message: fmt.Sprintf("%s%s is invalid: %v", envPrefix, key, err),
		Hint:    "fix the value in /etc/cvctl/host.env or re-run `cvctl init`",
	}
}

func newConfig() *Config { return &Config{vals: map[string]string{}, extra: map[string]string{}} }

// Get returns the raw value of key (without the CVCTL_ prefix), "" if unset.
func (c *Config) Get(key string) string { return c.vals[key] }

// Set validates and stores key (without the CVCTL_ prefix). An empty value
// unsets it.
func (c *Config) Set(key, val string) error {
	known := false
	for _, k := range knownKeys {
		if k == key {
			known = true
		}
	}
	if !known {
		return invalid(key, errors.New("unknown key"))
	}
	if val != "" {
		if v := validators[key]; v != nil {
			if err := v(val); err != nil {
				return invalid(key, err)
			}
		}
		c.vals[key] = val
	} else {
		delete(c.vals, key)
	}
	c.sync(key, val)
	return nil
}

func (c *Config) sync(key, val string) {
	switch key {
	case "HOST_NAME":
		c.HostName = val
	case "EDGE_MODE":
		c.EdgeMode = val
	case "PROXY_PORT":
		c.ProxyPort, _ = strconv.Atoi(val)
	case "ADMIN_USER":
		c.AdminUser = val
	case "SSH_MANAGE":
		c.SSHManage = val == "true"
	case "FIREWALL_MANAGE":
		c.FirewallManage = val == "true"
	case "ACME_EMAIL":
		c.AcmeEmail = val
	case "ACME_CHALLENGE":
		c.AcmeChallenge = val
	case "STORAGE":
		c.Storage = val
	case "S3_ENDPOINT":
		c.S3Endpoint = val
	case "S3_BUCKET":
		c.S3Bucket = val
	case "S3_REGION":
		c.S3Region = val
	case "NOTIFY":
		c.Notify = val
	case "TELEGRAM_CHAT_ID":
		c.TelegramChatID = val
	case "TELEGRAM_THREAD_ID":
		c.TelegramThread = val
	case "REGISTRIES":
		c.Registries = val
	case "REGISTRY_USER":
		c.RegistryUser = val
	}
}

// Secrets holds secret values by file name. Values never appear in errors.
type Secrets struct{ m map[string]string }

func newSecrets() *Secrets { return &Secrets{m: map[string]string{}} }

func (s *Secrets) Get(name string) (string, bool) {
	v, ok := s.m[name]
	return v, ok && v != ""
}

func (s *Secrets) Set(name, value string) {
	if value == "" {
		delete(s.m, name)
		return
	}
	s.m[name] = value
}

func readErr(p string, err error) error {
	return &output.Error{
		Code:    "E_HOSTCFG_READ",
		Message: fmt.Sprintf("cannot read %s: %v", p, err),
		Hint:    "check that the file exists and is readable by the current user",
	}
}

// Load reads <dir>/host.env and <dir>/secrets/*. Missing files give empty values.
func Load(fsys sys.FS, dir string) (*Config, *Secrets, error) {
	cfg, sec := newConfig(), newSecrets()
	envPath := path.Join(dir, "host.env")
	b, err := fsys.ReadFile(envPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, nil, readErr(envPath, err)
	default:
		m, perr := godotenv.Unmarshal(string(b))
		if perr != nil {
			return nil, nil, readErr(envPath, perr)
		}
		known := map[string]bool{}
		for _, k := range knownKeys {
			known[envPrefix+k] = true
			if v, ok := m[envPrefix+k]; ok {
				if err := cfg.Set(k, v); err != nil {
					return nil, nil, err
				}
			}
		}
		for k, v := range m {
			if !known[k] {
				cfg.extra[k] = v
			}
		}
	}
	for _, n := range SecretNames {
		p := path.Join(dir, "secrets", n)
		b, err := fsys.ReadFile(p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, nil, readErr(p, err)
		}
		sec.Set(n, strings.TrimRight(string(b), "\r\n"))
	}
	return cfg, sec, nil
}

// owner is root:root because the deploy group may not exist yet when init
// first saves; the accounts step later sets root:deploy.
var owner = sys.Owner{User: "root", Group: "root"}

func writeErr(p string, err error) error {
	return &output.Error{
		Code:    "E_HOSTCFG_WRITE",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    "run as root and check free space under /etc/cvctl",
	}
}

// Save writes host.env and one file per set secret, everything root:root
// (dir 0700 when created, secrets dir 0750, files 0640). Unknown host.env
// entries read by Load are preserved; secrets cleared with Set(name, "") have
// their file removed; secrets are never written to host.env.
func Save(fsys sys.FS, dir string, cfg *Config, sec *Secrets) error {
	if _, err := fsys.Stat(dir); err != nil {
		if err := fsys.MkdirAll(dir, 0o700, owner); err != nil {
			return writeErr(dir, err)
		}
	}
	values := map[string]string{}
	for k, v := range cfg.extra {
		values[k] = v
	}
	for k, v := range cfg.vals {
		values[envPrefix+k] = v
	}
	envPath := path.Join(dir, "host.env")
	if err := fsys.WriteFile(envPath, instance.FormatEnv(values), 0o640, owner); err != nil {
		return writeErr(envPath, err)
	}
	sdir := path.Join(dir, "secrets")
	if err := fsys.MkdirAll(sdir, 0o750, owner); err != nil {
		return writeErr(sdir, err)
	}
	for _, n := range SecretNames {
		v, ok := sec.Get(n)
		p := path.Join(sdir, n)
		if !ok {
			if err := fsys.Remove(p); err != nil {
				return writeErr(p, err)
			}
			continue
		}
		if err := fsys.WriteFile(p, []byte(v), 0o640, owner); err != nil {
			return writeErr(p, err)
		}
	}
	return nil
}
