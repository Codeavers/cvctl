package adopt

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func errCode(t *testing.T, err error) string {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("not an output.Error: %v", err)
	}
	return oe.Code
}

func TestValidate(t *testing.T) {
	valid := Service{Name: "cockpit", Domain: "server.example.com", Target: "https://host:9090"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid service rejected: %v", err)
	}
	cases := []struct {
		name string
		edit func(*Service)
		code string
	}{
		{"uppercase name", func(s *Service) { s.Name = "Cockpit" }, "E_ADOPT_NAME"},
		{"double dash name", func(s *Service) { s.Name = "a--b" }, "E_ADOPT_NAME"},
		{"leading dash name", func(s *Service) { s.Name = "-a" }, "E_ADOPT_NAME"},
		{"empty name", func(s *Service) { s.Name = "" }, "E_ADOPT_NAME"},
		{"empty domain", func(s *Service) { s.Domain = "" }, "E_USAGE_DOMAIN"},
		{"wildcard domain", func(s *Service) { s.Domain = "*.example.com" }, "E_USAGE_DOMAIN"},
		{"space in domain", func(s *Service) { s.Domain = "a b" }, "E_USAGE_DOMAIN"},
		{"ip target", func(s *Service) { s.Target = "http://10.0.0.1:80" }, "E_ADOPT_TARGET"},
		{"ftp target", func(s *Service) { s.Target = "ftp://host:21" }, "E_ADOPT_TARGET"},
		{"no port", func(s *Service) { s.Target = "https://host" }, "E_ADOPT_TARGET"},
		{"port zero", func(s *Service) { s.Target = "https://host:0" }, "E_ADOPT_TARGET"},
		{"port too high", func(s *Service) { s.Target = "https://host:70000" }, "E_ADOPT_TARGET"},
		{"path", func(s *Service) { s.Target = "https://host:9090/path" }, "E_ADOPT_TARGET"},
		{"insecure over http", func(s *Service) { s.Target = "http://host:9090"; s.InsecureTLS = true }, "E_ADOPT_TARGET"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := valid
			c.edit(&s)
			if got := errCode(t, s.Validate()); got != c.code {
				t.Fatalf("code %s, want %s", got, c.code)
			}
		})
	}
}

func TestPort(t *testing.T) {
	if got := (Service{Target: "https://host:9090"}).Port(); got != 9090 {
		t.Fatalf("port %d", got)
	}
}

type dynamic struct {
	HTTP struct {
		Routers map[string]struct {
			Rule        string         `yaml:"rule"`
			Service     string         `yaml:"service"`
			EntryPoints []string       `yaml:"entryPoints"`
			Middlewares []string       `yaml:"middlewares"`
			TLS         map[string]any `yaml:"tls"`
		} `yaml:"routers"`
		Middlewares map[string]struct {
			Headers map[string]any `yaml:"headers"`
		} `yaml:"middlewares"`
		Services map[string]struct {
			LoadBalancer struct {
				Servers []struct {
					URL string `yaml:"url"`
				} `yaml:"servers"`
				ServersTransport string `yaml:"serversTransport"`
			} `yaml:"loadBalancer"`
		} `yaml:"services"`
		ServersTransports map[string]struct {
			InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
		} `yaml:"serversTransports"`
	} `yaml:"http"`
}

func renderDoc(t *testing.T, svcs []Service, mode string) dynamic {
	t.Helper()
	b, err := Render(svcs, mode, "10.203.0.1")
	if err != nil {
		t.Fatal(err)
	}
	var d dynamic
	if err := yaml.Unmarshal(b, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRenderEdge(t *testing.T) {
	d := renderDoc(t, []Service{{Name: "cockpit", Domain: "server.example.com", Target: "https://host:9090"}}, "edge")
	r := d.HTTP.Routers["adopted-cockpit"]
	if r.Rule != "Host(`server.example.com`)" || r.Service != "adopted-cockpit" {
		t.Fatalf("router %+v", r)
	}
	if len(r.EntryPoints) != 1 || r.EntryPoints[0] != "websecure" {
		t.Fatalf("entrypoints %v", r.EntryPoints)
	}
	if r.TLS["certResolver"] != "le" {
		t.Fatalf("tls %v", r.TLS)
	}
	want := []string{"adopted-headers@file", "cvctl-ratelimit@file", "cvctl-inflight@file"}
	if !slices.Equal(r.Middlewares, want) {
		t.Fatalf("middlewares %v", r.Middlewares)
	}
	if got := d.HTTP.Services["adopted-cockpit"].LoadBalancer.Servers[0].URL; got != "https://10.203.0.1:9090" {
		t.Fatalf("url %s", got)
	}
	if len(d.HTTP.ServersTransports) != 0 {
		t.Fatalf("unexpected transports %v", d.HTTP.ServersTransports)
	}
}

// Panels such as Cockpit load their pages in a same-origin iframe, which the
// default chain's X-Frame-Options: DENY would blank out.
func TestRenderHeadersAllowSameOriginFrames(t *testing.T) {
	svcs := []Service{{Name: "app", Domain: "a.example.com", Target: "http://host:3000"}}
	edge := renderDoc(t, svcs, "edge").HTTP.Middlewares["adopted-headers"].Headers
	if edge["customFrameOptionsValue"] != "SAMEORIGIN" || edge["contentTypeNosniff"] != true {
		t.Fatalf("headers %v", edge)
	}
	if edge["frameDeny"] != nil || edge["stsSeconds"] != 31536000 || edge["stsIncludeSubdomains"] != true {
		t.Fatalf("headers %v", edge)
	}
	proxied := renderDoc(t, svcs, "behind-proxy").HTTP.Middlewares["adopted-headers"].Headers
	if proxied["stsSeconds"] != nil {
		t.Fatalf("headers %v", proxied)
	}
}

func TestRenderBehindProxy(t *testing.T) {
	d := renderDoc(t, []Service{{Name: "app", Domain: "a.example.com", Target: "http://host:3000"}}, "behind-proxy")
	r := d.HTTP.Routers["adopted-app"]
	if len(r.EntryPoints) != 1 || r.EntryPoints[0] != "web" || r.TLS != nil {
		t.Fatalf("router %+v", r)
	}
}

func TestRenderInsecureTLS(t *testing.T) {
	d := renderDoc(t, []Service{{Name: "cockpit", Domain: "s.example.com", Target: "https://host:9090", InsecureTLS: true}}, "edge")
	if !d.HTTP.ServersTransports["adopted-cockpit"].InsecureSkipVerify {
		t.Fatalf("transports %v", d.HTTP.ServersTransports)
	}
	if got := d.HTTP.Services["adopted-cockpit"].LoadBalancer.ServersTransport; got != "adopted-cockpit" {
		t.Fatalf("service transport %q", got)
	}
}

func TestRenderEmpty(t *testing.T) {
	b, err := Render(nil, "edge", "10.203.0.1")
	if err != nil || b != nil {
		t.Fatalf("got %q, %v", b, err)
	}
}

const (
	testDir = "/etc/cvctl/adopted"
	dynDir  = "/etc/traefik/dynamic"
)

func TestLoadMissingDir(t *testing.T) {
	svcs, err := Load(systest.NewFS(), testDir)
	if err != nil || len(svcs) != 0 {
		t.Fatalf("got %v, %v", svcs, err)
	}
}

func TestSaveLoadRemove(t *testing.T) {
	fs := systest.NewFS()
	b := Service{Name: "b", Domain: "b.example.com", Target: "http://host:2"}
	a := Service{Name: "a", Domain: "a.example.com", Target: "https://host:1", InsecureTLS: true}
	for _, s := range []Service{b, a} {
		if err := Save(fs, testDir, s); err != nil {
			t.Fatal(err)
		}
	}
	got, err := Load(fs, testDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("got %+v", got)
	}
	if err := Remove(fs, testDir, "a"); err != nil {
		t.Fatal(err)
	}
	got, _ = Load(fs, testDir)
	if len(got) != 1 || got[0] != b {
		t.Fatalf("after remove %+v", got)
	}
}

func TestSyncWritesRoutes(t *testing.T) {
	fs := systest.NewFS()
	if err := Save(fs, testDir, Service{Name: "a", Domain: "a.example.com", Target: "http://host:1"}); err != nil {
		t.Fatal(err)
	}
	if err := Sync(fs, testDir, dynDir, "edge", "10.203.0.1"); err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(dynDir + "/" + RouteFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "adopted-a") {
		t.Fatalf("routes %s", b)
	}
}

func TestWriteRoutes(t *testing.T) {
	fs := systest.NewFS()
	svcs := []Service{{Name: "a", Domain: "a.example.com", Target: "http://host:1"}}
	if err := WriteRoutes(fs, dynDir, svcs, "edge", "10.203.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(dynDir + "/" + RouteFile); err != nil {
		t.Fatal(err)
	}
	if err := WriteRoutes(fs, dynDir, nil, "edge", "10.203.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(dynDir + "/" + RouteFile); err == nil {
		t.Fatal("routes file still there")
	}
}

func TestSyncRemovesRoutesWhenEmpty(t *testing.T) {
	fs := systest.NewFS()
	if err := fs.WriteFile(dynDir+"/"+RouteFile, []byte("old"), 0o644, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := Sync(fs, testDir, dynDir, "edge", "10.203.0.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := fs.ReadFile(dynDir + "/" + RouteFile); err == nil {
		t.Fatal("routes file still there")
	}
}

func TestSyncKeepsRoutesOnCorruptRegistry(t *testing.T) {
	fs := systest.NewFS()
	if err := fs.WriteFile(dynDir+"/"+RouteFile, []byte("old"), 0o644, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := fs.MkdirAll(testDir, 0o755, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := fs.WriteFile(testDir+"/bad.yml", []byte("name: [unclosed"), 0o644, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	err := Sync(fs, testDir, dynDir, "edge", "10.203.0.1")
	if err == nil || !strings.Contains(err.Error(), "bad.yml") {
		t.Fatalf("err %v", err)
	}
	b, _ := fs.ReadFile(dynDir + "/" + RouteFile)
	if string(b) != "old" {
		t.Fatalf("routes rewritten: %s", b)
	}
}
