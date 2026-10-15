package render

import (
	"context"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

var update = flag.Bool("update", false, "rewrite golden files")

func setup(t *testing.T) (*contract.Project, *instance.Decisions, kinds.Catalog) {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := contract.Load("testdata/valid", cat)
	if err != nil {
		t.Fatal(err)
	}
	d := &instance.Decisions{
		Instance: "proj-dev", Env: "dev", Strategy: "recreate",
		Services: map[string]instance.ServiceDecision{
			"main-db": {Mode: "local", Version: "16", Creds: kinds.Credentials{User: "u", Password: "p", DB: "d"}},
			"cache":   {Mode: "local", Creds: kinds.Credentials{Password: "rp"}},
			"media":   {Mode: "external", External: map[string]string{"endpoint": "https://s3", "bucket": "b", "access_key": "ak", "secret_key": "sk", "region": "r"}},
		},
		Domains: map[string]string{"api": "api.example.com"},
		Digests: map[string]string{"api": "sha256:abc"},
	}
	return p, d, cat
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *update {
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != string(got) {
		t.Fatalf("%s differs\n--- got\n%s\n--- want\n%s", name, got, want)
	}
}

func TestOverrideGolden(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "override.yml", out)
}

func TestTraefikGoldenEdge(t *testing.T) {
	p, d, _ := setup(t)
	out, err := Traefik(p, d, Options{Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "traefik-edge.yml", out)
}

func TestTraefikGoldenBehindProxy(t *testing.T) {
	p, d, _ := setup(t)
	out, err := Traefik(p, d, Options{Edge: "behind-proxy"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "traefik-behind-proxy.yml", out)
}

func TestTraefikNoExpose(t *testing.T) {
	p, d, _ := setup(t)
	p.Apps[0].ExposePort = 0
	out, err := Traefik(p, d, Options{Edge: "edge"})
	if err != nil || out != nil {
		t.Fatalf("got %q, %v", out, err)
	}
}

func TestMissingDecisions(t *testing.T) {
	p, d, cat := setup(t)
	delete(d.Digests, "api")
	_, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if e, ok := err.(*output.Error); !ok || e.Code != "E_DECISION_MISSING" || e.Hint == "" {
		t.Fatalf("digest: %v", err)
	}
	p, d, _ = setup(t)
	delete(d.Domains, "api")
	_, err = Traefik(p, d, Options{Edge: "edge"})
	if e, ok := err.(*output.Error); !ok || e.Code != "E_DECISION_MISSING" {
		t.Fatalf("domain: %v", err)
	}
	p, d, cat = setup(t)
	delete(d.Services, "main-db")
	_, err = Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if e, ok := err.(*output.Error); !ok || e.Code != "E_DECISION_MISSING" {
		t.Fatalf("mode: %v", err)
	}
}

func TestServiceVarsExternalS3(t *testing.T) {
	p, d, cat := setup(t)
	sv, err := ServiceVars(p, d, cat)
	if err != nil {
		t.Fatal(err)
	}
	if sv["media"]["S3_BUCKET"] != "b" || sv["media"]["S3_ENDPOINT"] != "https://s3" || sv["media"]["S3_SECRET_KEY"] != "sk" {
		t.Fatalf("media: %v", sv["media"])
	}
	if sv["main-db"]["DATABASE_URL"] != "postgres://u:p@main-db:5432/d" {
		t.Fatalf("main-db: %v", sv["main-db"])
	}
	if sv["cache"]["REDIS_URL"] != "redis://:rp@cache:6379/0" {
		t.Fatalf("cache: %v", sv["cache"])
	}
}

func TestServiceVarsExternalMissingField(t *testing.T) {
	p, d, cat := setup(t)
	sd := d.Services["media"]
	delete(sd.External, "bucket")
	d.Services["media"] = sd
	if _, err := ServiceVars(p, d, cat); err == nil {
		t.Fatal("expected error")
	}
}

func loadCompose(t *testing.T, files map[string][]byte, order []string) *types.Project {
	t.Helper()
	var cfs []types.ConfigFile
	for _, n := range order {
		cfs = append(cfs, types.ConfigFile{Filename: n, Content: files[n]})
	}
	cp, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir: "testdata/valid", ConfigFiles: cfs, Environment: types.Mapping{},
	}, func(o *loader.Options) {
		o.SkipInterpolation = true
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
		o.SetProjectName("cvctl-lint", true)
	})
	if err != nil {
		t.Fatal(err)
	}
	return cp
}

func TestOverrideIsValidCompose(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	cp := loadCompose(t, map[string][]byte{"compose.yml": p.RawCompose, "compose.cvctl.yml": out}, []string{"compose.yml", "compose.cvctl.yml"})
	if img := cp.Services["api"].Image; !strings.Contains(img, "@sha256:") {
		t.Fatalf("api image %q", img)
	}
}

func TestProjectMemoryLimitKept(t *testing.T) {
	p, d, cat := setup(t)
	svc := p.Compose.Services["main-db"]
	svc.Deploy = &types.DeployConfig{Resources: types.Resources{Limits: &types.Resource{MemoryBytes: 1 << 30}}}
	p.Compose.Services["main-db"] = svc
	out, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Services["main-db"]["deploy"]; ok {
		t.Fatal("main-db must not get a deploy block")
	}
	if _, ok := doc.Services["cache"]["deploy"]; !ok || !strings.Contains(string(out), "memory: 512m") {
		t.Fatal("cache must still get 512m")
	}
}

func TestDollarEscaping(t *testing.T) {
	p, d, cat := setup(t)
	pw := "ab$cd$$e"
	sd := d.Services["main-db"]
	sd.Creds.Password = pw
	d.Services["main-db"] = sd
	sd = d.Services["cache"]
	sd.Creds.Password = pw
	d.Services["cache"] = sd
	out, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(out), "ab$$cd$$$$e"); n < 3 {
		t.Fatalf("want escaped password in environment, command and healthcheck, got %d\n%s", n, out)
	}
	cp, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
		WorkingDir: "testdata/valid",
		ConfigFiles: []types.ConfigFile{
			{Filename: "compose.yml", Content: p.RawCompose},
			{Filename: "compose.cvctl.yml", Content: out},
		},
		Environment: types.Mapping{},
	}, func(o *loader.Options) {
		o.SkipConsistencyCheck = true
		o.SkipResolveEnvironment = true
		o.SetProjectName("cvctl-lint", true)
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := cp.Services["main-db"].Environment["POSTGRES_PASSWORD"]; got == nil || *got != pw {
		t.Fatalf("interpolated password = %v, want %q", got, pw)
	}
	if cmd := cp.Services["cache"].Command; cmd[2] != pw {
		t.Fatalf("interpolated command = %v", cmd)
	}
}

func TestUnsafeUserOrDB(t *testing.T) {
	for field, want := range map[string]string{"user": "CVCTL_SVC_MAIN_DB_USER", "db": "CVCTL_SVC_MAIN_DB_DB"} {
		p, d, cat := setup(t)
		sd := d.Services["main-db"]
		if field == "user" {
			sd.Creds.User = "u; rm -rf /"
		} else {
			sd.Creds.DB = "d$(x)"
		}
		d.Services["main-db"] = sd
		_, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
		e, ok := err.(*output.Error)
		if !ok || e.Code != "E_DECISION_INVALID" || !strings.Contains(e.Hint, want) {
			t.Fatalf("%s: %v", field, err)
		}
	}
}
