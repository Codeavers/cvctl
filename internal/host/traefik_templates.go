package host

import (
	"bytes"
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
)

type traefikStaticConfig struct {
	API                   traefikAPI                     `yaml:"api"`
	AccessLog             map[string]any                 `yaml:"accessLog"`
	Log                   traefikLog                     `yaml:"log"`
	Ping                  traefikPing                    `yaml:"ping"`
	EntryPoints           map[string]traefikEntryPoint   `yaml:"entryPoints"`
	Providers             traefikProviders               `yaml:"providers"`
	CertificatesResolvers map[string]traefikCertResolver `yaml:"certificatesResolvers,omitempty"`
}

type traefikAPI struct {
	Dashboard bool `yaml:"dashboard"`
}

type traefikLog struct {
	Level string `yaml:"level"`
}

type traefikPing struct {
	EntryPoint string `yaml:"entryPoint"`
}

type traefikHTTP3 struct{}

type traefikEntryPoint struct {
	Address string                 `yaml:"address"`
	HTTP    *traefikEntryPointHTTP `yaml:"http,omitempty"`
	HTTP3   *traefikHTTP3          `yaml:"http3,omitempty"`
}

type traefikEntryPointHTTP struct {
	Redirections traefikRedirections `yaml:"redirections"`
}

type traefikRedirections struct {
	EntryPoint traefikRedirectEntryPoint `yaml:"entryPoint"`
}

type traefikRedirectEntryPoint struct {
	To        string `yaml:"to"`
	Scheme    string `yaml:"scheme"`
	Permanent bool   `yaml:"permanent"`
}

type traefikProviders struct {
	File traefikFileProvider `yaml:"file"`
}

type traefikFileProvider struct {
	Directory string `yaml:"directory"`
	Watch     bool   `yaml:"watch"`
}

type traefikCertResolver struct {
	ACME traefikACME `yaml:"acme"`
}

type traefikACME struct {
	Email         string                `yaml:"email"`
	Storage       string                `yaml:"storage"`
	HTTPChallenge *traefikHTTPChallenge `yaml:"httpChallenge,omitempty"`
	DNSChallenge  *traefikDNSChallenge  `yaml:"dnsChallenge,omitempty"`
}

type traefikHTTPChallenge struct {
	EntryPoint string `yaml:"entryPoint"`
}

type traefikDNSChallenge struct {
	Provider string `yaml:"provider"`
}

type traefikDynamicConfig struct {
	HTTP traefikDynamicHTTP `yaml:"http"`
}

type traefikDynamicHTTP struct {
	Middlewares map[string]traefikMiddleware `yaml:"middlewares"`
}

type traefikMiddleware struct {
	Chain       *traefikChainMiddleware       `yaml:"chain,omitempty"`
	Headers     *traefikHeadersMiddleware     `yaml:"headers,omitempty"`
	RateLimit   *traefikRateLimitMiddleware   `yaml:"rateLimit,omitempty"`
	InFlightReq *traefikInFlightReqMiddleware `yaml:"inFlightReq,omitempty"`
}

type traefikChainMiddleware struct {
	Middlewares []string `yaml:"middlewares"`
}

type traefikHeadersMiddleware struct {
	STSSeconds           int    `yaml:"stsSeconds,omitempty"`
	STSIncludeSubdomains bool   `yaml:"stsIncludeSubdomains,omitempty"`
	ContentTypeNosniff   bool   `yaml:"contentTypeNosniff"`
	FrameDeny            bool   `yaml:"frameDeny"`
	ReferrerPolicy       string `yaml:"referrerPolicy"`
}

type traefikRateLimitMiddleware struct {
	Average int `yaml:"average"`
	Burst   int `yaml:"burst"`
}

type traefikInFlightReqMiddleware struct {
	Amount int `yaml:"amount"`
}

type traefikComposeConfig struct {
	Services map[string]traefikComposeService `yaml:"services"`
	Networks map[string]traefikComposeNetwork `yaml:"networks"`
	Volumes  map[string]map[string]any        `yaml:"volumes"`
}

type traefikComposeService struct {
	Image       string                    `yaml:"image"`
	Restart     string                    `yaml:"restart"`
	Ports       []string                  `yaml:"ports"`
	EnvFile     []string                  `yaml:"env_file,omitempty"`
	Volumes     []string                  `yaml:"volumes"`
	Networks    []string                  `yaml:"networks"`
	Healthcheck traefikComposeHealthcheck `yaml:"healthcheck"`
}

type traefikComposeHealthcheck struct {
	Test []string `yaml:"test"`
}

type traefikComposeNetwork struct {
	External bool `yaml:"external"`
}

func marshalYAML(v any) ([]byte, error) {
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

func effectiveEdgeMode(cfg *hostcfg.Config) string {
	if cfg != nil && cfg.EdgeMode != "" {
		return cfg.EdgeMode
	}
	return "edge"
}

func isDNSChallenge(cfg *hostcfg.Config) bool {
	if effectiveEdgeMode(cfg) != "edge" || cfg == nil {
		return false
	}
	return cfg.AcmeChallenge == "route53" || cfg.AcmeChallenge == "cloudflare"
}

func renderTraefikStatic(cfg *hostcfg.Config) ([]byte, error) {
	mode := effectiveEdgeMode(cfg)
	eps := map[string]traefikEntryPoint{
		"ping": {Address: ":8082"},
	}

	var resolvers map[string]traefikCertResolver
	if mode == "edge" {
		eps[EntryWeb] = traefikEntryPoint{
			Address: ":80",
			HTTP: &traefikEntryPointHTTP{
				Redirections: traefikRedirections{
					EntryPoint: traefikRedirectEntryPoint{
						To:        EntryWebSecure,
						Scheme:    "https",
						Permanent: true,
					},
				},
			},
		}
		eps[EntryWebSecure] = traefikEntryPoint{
			Address: ":443",
			HTTP3:   &traefikHTTP3{},
		}

		email := ""
		challenge := "http"
		if cfg != nil {
			email = cfg.AcmeEmail
			if cfg.AcmeChallenge != "" {
				challenge = cfg.AcmeChallenge
			}
		}
		acme := traefikACME{
			Email:   email,
			Storage: "/acme/acme.json",
		}
		if challenge == "route53" || challenge == "cloudflare" {
			acme.DNSChallenge = &traefikDNSChallenge{Provider: challenge}
		} else {
			acme.HTTPChallenge = &traefikHTTPChallenge{EntryPoint: EntryWeb}
		}
		resolvers = map[string]traefikCertResolver{
			CertResolver: {ACME: acme},
		}
	} else {
		eps[EntryWeb] = traefikEntryPoint{Address: ":80"}
	}

	doc := traefikStaticConfig{
		API:         traefikAPI{Dashboard: false},
		AccessLog:   map[string]any{},
		Log:         traefikLog{Level: "INFO"},
		Ping:        traefikPing{EntryPoint: "ping"},
		EntryPoints: eps,
		Providers: traefikProviders{
			File: traefikFileProvider{
				Directory: "/etc/traefik/dynamic",
				Watch:     true,
			},
		},
		CertificatesResolvers: resolvers,
	}
	return marshalYAML(doc)
}

func renderTraefikDynamic(cfg *hostcfg.Config) ([]byte, error) {
	mode := effectiveEdgeMode(cfg)
	hdrs := &traefikHeadersMiddleware{
		ContentTypeNosniff: true,
		FrameDeny:          true,
		ReferrerPolicy:     "strict-origin-when-cross-origin",
	}
	if mode == "edge" {
		hdrs.STSSeconds = 31536000
		hdrs.STSIncludeSubdomains = true
	}

	defaultName := strings.TrimSuffix(DefaultMiddleware, "@file")
	doc := traefikDynamicConfig{
		HTTP: traefikDynamicHTTP{
			Middlewares: map[string]traefikMiddleware{
				defaultName: {
					Chain: &traefikChainMiddleware{
						Middlewares: []string{"headers", "rateLimit", "inFlightReq"},
					},
				},
				"headers": {
					Headers: hdrs,
				},
				"rateLimit": {
					RateLimit: &traefikRateLimitMiddleware{
						Average: 50,
						Burst:   100,
					},
				},
				"inFlightReq": {
					InFlightReq: &traefikInFlightReqMiddleware{
						Amount: 100,
					},
				},
			},
		},
	}
	return marshalYAML(doc)
}

func renderTraefikCompose(cfg *hostcfg.Config) ([]byte, error) {
	mode := effectiveEdgeMode(cfg)
	var ports []string
	var envFile []string

	if mode == "edge" {
		ports = []string{"80:80", "443:443", "443:443/udp"}
		if isDNSChallenge(cfg) {
			envFile = []string{traefikSecretsPath}
		}
	} else {
		port := 8081
		if cfg != nil && cfg.ProxyPort > 0 {
			port = cfg.ProxyPort
		}
		ports = []string{fmt.Sprintf("127.0.0.1:%d:80", port)}
	}

	doc := traefikComposeConfig{
		Services: map[string]traefikComposeService{
			"traefik": {
				Image:   traefikImage,
				Restart: "unless-stopped",
				Ports:   ports,
				EnvFile: envFile,
				Volumes: []string{
					traefikStaticPath + ":/etc/traefik/traefik.yml:ro",
					"/etc/traefik/dynamic:/etc/traefik/dynamic:ro",
					"cvctl-traefik-acme:/acme",
				},
				Networks: []string{EdgeNetwork},
				Healthcheck: traefikComposeHealthcheck{
					Test: []string{"CMD", "traefik", "healthcheck", "--ping"},
				},
			},
		},
		Networks: map[string]traefikComposeNetwork{
			EdgeNetwork: {External: true},
		},
		Volumes: map[string]map[string]any{
			"cvctl-traefik-acme": {},
		},
	}
	return marshalYAML(doc)
}

func renderTraefikEnv(cfg *hostcfg.Config, sec *hostcfg.Secrets) ([]byte, bool, error) {
	if !isDNSChallenge(cfg) {
		return nil, false, nil
	}
	switch cfg.AcmeChallenge {
	case "route53":
		var ak, sk string
		if sec != nil {
			ak, _ = sec.Get("route53_access_key")
			sk, _ = sec.Get("route53_secret_key")
		}
		if ak == "" || sk == "" {
			return nil, false, &output.Error{
				Code:    "E_TRAEFIK_FAILED",
				Message: "missing route53 credentials for DNS challenge",
				Hint:    "set route53_access_key and route53_secret_key in /etc/cvctl/secrets",
			}
		}
		return instance.FormatEnv(map[string]string{
			"AWS_ACCESS_KEY_ID":     ak,
			"AWS_REGION":            "us-east-1",
			"AWS_SECRET_ACCESS_KEY": sk,
		}), true, nil
	case "cloudflare":
		var tok string
		if sec != nil {
			tok, _ = sec.Get("cloudflare_dns_token")
		}
		if tok == "" {
			return nil, false, &output.Error{
				Code:    "E_TRAEFIK_FAILED",
				Message: "missing cloudflare_dns_token for DNS challenge",
				Hint:    "set cloudflare_dns_token in /etc/cvctl/secrets",
			}
		}
		return instance.FormatEnv(map[string]string{
			"CF_DNS_API_TOKEN": tok,
		}), true, nil
	default:
		return nil, false, nil
	}
}
