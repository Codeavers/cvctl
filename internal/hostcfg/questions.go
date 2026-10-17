package hostcfg

import (
	"sort"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
)

// Facts are values detected on the machine that feed defaults.
type Facts struct {
	Hostname, SudoUser string
	Adopt              bool
}

// Question describes one value init needs. Secret questions fill Secrets
// (Key is the secret file name); the rest fill Config (Key has no CVCTL_ prefix).
type Question struct {
	Key      string
	Secret   bool
	Optional bool // empty answer is fine and never reported missing
	Prompt   string
	Help     string
	Options  []string
	Default  func(*Config, Facts) string
	When     func(*Config) bool
	Validate func(string) error
}

// Prompter asks one question. Callers may pass ad-hoc questions that are not
// in Questions().
type Prompter interface {
	Ask(q Question, def string) (string, error)
}

func lit(s string) func(*Config, Facts) string { return func(*Config, Facts) string { return s } }

func is(key, val string) func(*Config) bool {
	return func(c *Config) bool { return c.Get(key) == val }
}

func and(fs ...func(*Config) bool) func(*Config) bool {
	return func(c *Config) bool {
		for _, f := range fs {
			if !f(c) {
				return false
			}
		}
		return true
	}
}

func q(key, prompt string, opts ...func(*Question)) Question {
	x := Question{Key: key, Prompt: prompt, Validate: validators[key]}
	for _, o := range opts {
		o(&x)
	}
	return x
}

func withOpts(o ...string) func(*Question) { return func(x *Question) { x.Options = o } }
func withDef(f func(*Config, Facts) string) func(*Question) {
	return func(x *Question) { x.Default = f }
}
func withWhen(f func(*Config) bool) func(*Question) { return func(x *Question) { x.When = f } }
func withHelp(h string) func(*Question)             { return func(x *Question) { x.Help = h } }
func optional(x *Question)                          { x.Optional = true }
func secret(x *Question)                            { x.Secret = true }

// Questions returns the init questions in the order they are asked. When is
// evaluated against the config as answers accumulate.
func Questions() []Question {
	edge := is("EDGE_MODE", "edge")
	s3 := is("STORAGE", "s3")
	tg := is("NOTIFY", "telegram")
	route53 := and(edge, is("ACME_CHALLENGE", "route53"))
	cf := and(edge, is("ACME_CHALLENGE", "cloudflare"))
	return []Question{
		q("HOST_NAME", "Host name (DNS label)", withDef(func(_ *Config, f Facts) string { return f.Hostname })),
		q("EDGE_MODE", "Edge mode", withOpts("edge", "behind-proxy"),
			withHelp("edge: Traefik terminates TLS on 80/443; behind-proxy: another proxy forwards to Traefik"),
			withDef(func(_ *Config, f Facts) string {
				if f.Adopt {
					return "behind-proxy"
				}
				return "edge"
			})),
		q("PROXY_PORT", "Local port Traefik listens on", withWhen(is("EDGE_MODE", "behind-proxy")), withDef(lit("8081"))),
		q("ADMIN_USER", "Admin system user", withDef(func(_ *Config, f Facts) string { return f.SudoUser })),
		q("SSH_MANAGE", "Let cvctl manage SSH hardening?", withOpts("true", "false"), withDef(lit("true"))),
		q("FIREWALL_MANAGE", "Let cvctl manage the firewall?", withOpts("true", "false"),
			withDef(func(_ *Config, f Facts) string {
				if f.Adopt {
					return "false"
				}
				return "true"
			})),
		q("ACME_EMAIL", "ACME (Let's Encrypt) email", withWhen(edge)),
		q("ACME_CHALLENGE", "ACME challenge", withOpts("http", "route53", "cloudflare"), withWhen(edge), withDef(lit("http"))),
		q("route53_access_key", "Route53 access key", secret, withWhen(route53)),
		q("route53_secret_key", "Route53 secret key", secret, withWhen(route53)),
		q("cloudflare_dns_token", "Cloudflare DNS API token", secret, withWhen(cf)),
		q("STORAGE", "Backup storage", withOpts("local", "s3"), withDef(lit("local"))),
		q("S3_ENDPOINT", "S3 endpoint URL", withWhen(s3)),
		q("S3_BUCKET", "S3 bucket", withWhen(s3), withDef(lit("cvctl"))),
		q("S3_REGION", "S3 region", withWhen(s3), withDef(lit("auto"))),
		q("s3_access_key", "S3 access key", secret, withWhen(s3)),
		q("s3_secret_key", "S3 secret key", secret, withWhen(s3)),
		q("NOTIFY", "Notifications", withOpts("telegram", "none"), withDef(lit("none"))),
		q("telegram_token", "Telegram bot token", secret, withWhen(tg)),
		q("TELEGRAM_CHAT_ID", "Telegram chat id", withWhen(tg)),
		q("TELEGRAM_THREAD_ID", "Telegram thread id (optional)", optional, withWhen(tg)),
		q("REGISTRIES", "Image registries, comma-separated (empty = none)", optional, withHelp("e.g. ghcr.io/codeavers")),
		q("REGISTRY_USER", "Registry user (empty = public images)", optional),
		q("registry_token", "Registry token (empty = public images)", secret, optional),
	}
}

func missingErr(keys []string) error {
	return &output.Error{
		Code:    "E_INIT_MISSING",
		Message: "missing: " + strings.Join(keys, ", "),
		Hint:    "add them to /etc/cvctl/host.env (secrets in /etc/cvctl/secrets/<name>) or run interactively",
	}
}

// Resolve fills what cfg and s lack. A key that already has a value is never
// asked. Non-interactive runs take defaults and report the rest as
// E_INIT_MISSING; optional keys are left empty.
func Resolve(cfg *Config, s *Secrets, f Facts, p Prompter, interactive bool) error {
	var missing []string
	for _, qu := range Questions() {
		if qu.When != nil && !qu.When(cfg) {
			continue
		}
		if qu.Secret {
			if _, ok := s.Get(qu.Key); ok {
				continue
			}
		} else if cfg.Get(qu.Key) != "" {
			continue
		}
		def := ""
		if qu.Default != nil {
			def = qu.Default(cfg, f)
		}
		ans := def
		if interactive {
			a, err := p.Ask(qu, def)
			if err != nil {
				return err
			}
			ans = a
		} else if def == "" {
			if !qu.Optional {
				missing = append(missing, qu.Key)
			}
			continue
		}
		if ans == "" {
			if interactive && !qu.Optional {
				return &output.Error{
					Code:    "E_INIT_MISSING",
					Message: "no value given for " + qu.Key,
					Hint:    "answer the question or add the value to /etc/cvctl/host.env",
				}
			}
			continue
		}
		if qu.Validate != nil {
			if err := qu.Validate(ans); err != nil {
				return invalid(qu.Key, err)
			}
		}
		if qu.Secret {
			s.Set(qu.Key, ans)
		} else if err := cfg.Set(qu.Key, ans); err != nil {
			return err
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		return missingErr(missing)
	}
	return nil
}
