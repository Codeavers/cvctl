package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const sqliteCompose = `
services:
  web:
    image: ghcr.io/codeavers/web:develop
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy:
      resources:
        limits:
          memory: 256M
  store:
    labels:
      cvctl.kind: sqlite
      cvctl.owner: web
      cvctl.env: STORE
x-cvctl:
  health:
    service: web
    http: /healthz
`

const sqliteSpec = `
vars:
  STORE_PATH:
    source: service
    service: store
`

func sqliteEnv(t *testing.T, compose, spec string) (*Env, string) {
	t.Helper()
	t.Setenv("CVCTL_ALLOW_FILE_REPOS", "1")
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, compose, spec)
	stub := &stubRepo{infraSource: infraDir}
	return &Env{
		FS:         sys.NewOSFS(),
		DeployRoot: tmp,
		StateRoot:  filepath.Join(tmp, "var-lib-cvctl"),
		Catalog:    testCatalog(t),
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}, tmp
}

func TestSqliteRejectsBlueGreen(t *testing.T) {
	e, tmp := sqliteEnv(t, sqliteCompose, sqliteSpec)
	_, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{Env: "prod", Strategy: "blue-green", Domains: map[string]string{"web": "app.example.com"}})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_SQLITE_RECREATE" {
		t.Fatalf("err: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(tmp, "repo-prod")); statErr == nil {
		t.Fatal("a refused add must leave no instance behind")
	}
}

func TestSqliteProdDefaultsToRecreateAndSaysSo(t *testing.T) {
	e, tmp := sqliteEnv(t, sqliteCompose, sqliteSpec)
	res, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{Env: "prod", Domains: map[string]string{"web": "app.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decisions.Strategy != "recreate" {
		t.Fatalf("strategy: %s", res.Decisions.Strategy)
	}
	if !strings.Contains(strings.Join(res.Warnings, "\n"), "recreate") {
		t.Fatalf("the change of default must be reported: %v", res.Warnings)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "repo-prod", "cvctl.env"))
	if err != nil || !strings.Contains(string(raw), "CVCTL_STRATEGY=recreate") {
		t.Fatalf("cvctl.env: %s %v", raw, err)
	}
}

func TestProdWithoutSqliteKeepsBlueGreenDefault(t *testing.T) {
	e, _ := sqliteEnv(t, validCompose, validSpec)
	res, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{Env: "prod", Domains: map[string]string{"web": "app.example.com"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.Decisions.Strategy != "blue-green" {
		t.Fatalf("strategy: %s", res.Decisions.Strategy)
	}
}

func TestSqliteAddGeneratesNoCredentials(t *testing.T) {
	e, tmp := sqliteEnv(t, sqliteCompose, sqliteSpec)
	if _, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{Domains: map[string]string{"web": "app.example.com"}}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(tmp, "repo-dev", "cvctl.env"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	if !strings.Contains(s, "CVCTL_SVC_STORE_MODE=local") {
		t.Fatalf("cvctl.env: %s", s)
	}
	for _, key := range []string{"STORE_PASSWORD", "STORE_USER", "STORE_DB"} {
		if strings.Contains(s, key) {
			t.Fatalf("%s in cvctl.env: %s", key, s)
		}
	}
}

func TestSqliteRefusesExternal(t *testing.T) {
	e, _ := sqliteEnv(t, sqliteCompose, sqliteSpec)
	_, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{
		Domains: map[string]string{"web": "app.example.com"},
		Ext:     map[string]map[string]string{"store": {"path": "/x"}},
	})
	if err == nil {
		t.Fatal("sqlite has no external mode")
	}
}

func TestCheckDecisionsReportsRecreate(t *testing.T) {
	cat := testCatalog(t)
	p := &contract.Project{Data: []contract.DataService{{Name: "store", Kind: "sqlite"}}}
	d := &instance.Decisions{Instance: "app-prod", Env: "prod", Strategy: "blue-green", Services: map[string]instance.ServiceDecision{"store": {Mode: "local"}}}
	err := checkDecisions(d, p, cat)
	var ie *output.IssuesError
	if !errors.As(err, &ie) || len(ie.Issues) != 1 || ie.Issues[0].Code != "E_SQLITE_RECREATE" {
		t.Fatalf("project update must refuse it too: %v", err)
	}
}
