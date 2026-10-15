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
	if !hasProjectMemoryLimit(p, "api") || hasProjectMemoryLimit(p, "main-db") {
		t.Fatal("memory limit detection wrong")
	}
	_ = d
	_ = cat
}
