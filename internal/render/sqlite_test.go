package render

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/kinds"
)

const sqliteCompose = `
services:
  api:
    image: ghcr.io/acme/api:1
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy:
      resources:
        limits:
          memory: 128M
  worker:
    image: ghcr.io/acme/worker:1
    labels:
      cvctl.role: worker
    deploy:
      resources:
        limits:
          memory: 128M
  store:
    labels:
      cvctl.kind: sqlite
      cvctl.owner: api
      cvctl.env: STORE
x-cvctl:
  health:
    service: api
    http: /healthz
`

func setupSqlite(t *testing.T, compose string) (*contract.Project, *instance.Decisions, kinds.Catalog) {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := contract.Load(dir, cat)
	if err != nil {
		t.Fatal(err)
	}
	d := &instance.Decisions{
		Instance: "app-dev", Env: "dev", Strategy: "recreate",
		Services: map[string]instance.ServiceDecision{"store": {Mode: "local"}},
		Domains:  map[string]string{"api": "api.example.com"},
		Digests:  map[string]string{"api": fixedDigest, "worker": fixedDigest},
	}
	return p, d, cat
}

func renderSqlite(t *testing.T, p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o Options) map[string]any {
	t.Helper()
	o.InstanceDir = "/srv/deploy/app-dev"
	raw, err := Override(p, d, cat, o)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSqliteRenderMountsOwnerOnly(t *testing.T) {
	p, d, cat := setupSqlite(t, sqliteCompose)
	doc := renderSqlite(t, p, d, cat, Options{})
	svcs := doc["services"].(map[string]any)
	if _, ok := svcs["store"]; ok {
		t.Fatal("sqlite must render no service")
	}
	api := svcs["api"].(map[string]any)
	vols, _ := api["volumes"].([]any)
	if len(vols) != 1 || vols[0] != "store-data:/data/store" {
		t.Fatalf("owner volumes: %v", api["volumes"])
	}
	if _, ok := svcs["worker"].(map[string]any)["volumes"]; ok {
		t.Fatal("only the owner mounts the volume")
	}
	top := doc["volumes"].(map[string]any)["store-data"].(map[string]any)
	if top["name"] != "app-dev_store-data" {
		t.Fatalf("volume: %v", top)
	}
}

func TestSqliteRenderFollowsRestoredVolume(t *testing.T) {
	p, d, cat := setupSqlite(t, sqliteCompose)
	d.Services["store"] = instance.ServiceDecision{Mode: "local", Volume: "app-dev_store-data-r1"}
	doc := renderSqlite(t, p, d, cat, Options{})
	if got := doc["volumes"].(map[string]any)["store-data"].(map[string]any)["name"]; got != "app-dev_store-data-r1" {
		t.Fatalf("volume name: %v", got)
	}
}

func TestSqliteRenderNoBackupNetwork(t *testing.T) {
	p, d, cat := setupSqlite(t, sqliteCompose)
	doc := renderSqlite(t, p, d, cat, Options{Backup: testBackup()})
	if _, ok := doc["networks"].(map[string]any)["backup"]; ok {
		t.Fatal("nothing in the override joins the backup network: restic runs from the host")
	}
}

func TestSqliteRenderUIDRunsTheApp(t *testing.T) {
	compose := strings.Replace(sqliteCompose, "cvctl.owner: api\n", "cvctl.owner: api\n      cvctl.uid: \"1000\"\n", 1)
	p, d, cat := setupSqlite(t, compose)
	svcs := renderSqlite(t, p, d, cat, Options{})["services"].(map[string]any)
	if got := svcs["api"].(map[string]any)["user"]; got != "1000" {
		t.Fatalf("a volume owned by cvctl.uid is only writable if the app runs as it: user=%v", got)
	}
	if _, ok := svcs["worker"].(map[string]any)["user"]; ok {
		t.Fatal("only the owner changes user")
	}
}

func TestSqliteRenderNoUserWithoutLabel(t *testing.T) {
	p, d, cat := setupSqlite(t, sqliteCompose)
	svcs := renderSqlite(t, p, d, cat, Options{})["services"].(map[string]any)
	if _, ok := svcs["api"].(map[string]any)["user"]; ok {
		t.Fatal("without cvctl.uid the image's own user stands")
	}
}

func TestSqliteOverrideIsValidCompose(t *testing.T) {
	p, d, cat := setupSqlite(t, sqliteCompose)
	got, err := Override(p, d, cat, Options{InstanceDir: "/srv/deploy/app-dev", Backup: testBackup()})
	if err != nil {
		t.Fatal(err)
	}
	loadCompose(t, map[string][]byte{"compose.cvctl.yml": got}, []string{"compose.cvctl.yml"})
}

func TestSqliteEnvPath(t *testing.T) {
	p, d, cat := setupSqlite(t, sqliteCompose)
	vars, err := ServiceVars(p, d, cat)
	if err != nil {
		t.Fatal(err)
	}
	if got := vars["store"]; len(got) != 1 || got["STORE_PATH"] != "/data/store/app.db" {
		t.Fatalf("vars: %v", got)
	}
}
