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
		Digests: map[string]string{"api": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
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

func TestEmptyInstanceRefused(t *testing.T) {
	p, d, cat := setup(t)
	d.Instance = ""
	if _, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"}); err == nil {
		t.Fatal("Override accepted empty instance")
	}
	if _, err := Traefik(p, d, Options{Edge: "edge"}); err == nil {
		t.Fatal("Traefik accepted empty instance")
	}
}

func TestRenderAppEnvPath(t *testing.T) {
	p, d, cat := setup(t)

	// 1. When AppEnvPath is provided, it is used in env_file
	out, err := Override(p, d, cat, Options{
		InstanceDir: "/srv/deploy/proj-dev",
		Edge:        "edge",
		AppEnvPath:  "/srv/deploy/proj-dev/staging.app.env",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "- /srv/deploy/proj-dev/staging.app.env") {
		t.Fatalf("expected staging.app.env in override output:\n%s", string(out))
	}

	// 2. When AppEnvPath is empty, defaults to filepath.Join(InstanceDir, "app.env")
	outDef, err := Override(p, d, cat, Options{
		InstanceDir: "/srv/deploy/proj-dev",
		Edge:        "edge",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(outDef), "- /srv/deploy/proj-dev/app.env") {
		t.Fatalf("expected default app.env in override output:\n%s", string(outDef))
	}
}

func testBackup() *BackupTarget {
	return &BackupTarget{
		Host: "h1", Endpoint: "http://cvctl-storage:9000", Bucket: "cvctl", Region: "us-east-1",
		AccessKey: "AKIDSECRET1", SecretKey: "SKSECRETVALUE", LibsodiumKey: "LIBSODIUMSECRET", ResticPassword: "RESTICSECRET",
	}
}

func TestOverrideBackupGolden(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge", Backup: testBackup()})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "override-backup.yml", out)
	for _, s := range []string{"AKIDSECRET1", "SKSECRETVALUE", "LIBSODIUMSECRET", "RESTICSECRET"} {
		if strings.Contains(string(out), s) {
			t.Fatalf("override leaks secret %q", s)
		}
	}
}

func TestBackupEnvs(t *testing.T) {
	p, d, cat := setup(t)
	envs := BackupEnvs(p, d, cat, Options{Backup: testBackup()})
	if len(envs) != 1 {
		t.Fatalf("want only main-db, got %v", envs)
	}
	got := envs["main-db"]
	want := map[string]string{
		"WALG_S3_PREFIX":               "s3://cvctl/h1/proj-dev/main-db",
		"WALG_PREVENT_WAL_OVERWRITE":   "true",
		"AWS_ACCESS_KEY_ID":            "AKIDSECRET1",
		"AWS_SECRET_ACCESS_KEY":        "SKSECRETVALUE",
		"AWS_ENDPOINT":                 "http://cvctl-storage:9000",
		"AWS_REGION":                   "us-east-1",
		"AWS_S3_FORCE_PATH_STYLE":      "true",
		"WALG_LIBSODIUM_KEY":           "LIBSODIUMSECRET",
		"WALG_LIBSODIUM_KEY_TRANSFORM": "hex",
		"WALG_COMPRESSION_METHOD":      "zstd",
	}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("%s = %q, want %q", k, got[k], v)
		}
	}
	if len(BackupEnvs(p, d, cat, Options{})) != 0 {
		t.Fatal("nil backup must produce no env files")
	}
}

func TestNoBackupNoNetwork(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "cvctl-backup") || strings.Contains(string(out), "backup-main-db.env") {
		t.Fatalf("backup wiring rendered without a target:\n%s", out)
	}
}

func TestVolumeNameDefaultAndOverride(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "name: proj-dev_main-db-data") {
		t.Fatalf("default volume name missing:\n%s", out)
	}
	sd := d.Services["main-db"]
	sd.Volume = "restored_vol"
	d.Services["main-db"] = sd
	out, err = Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "name: restored_vol") || strings.Contains(string(out), "name: proj-dev_main-db-data") {
		t.Fatalf("volume override not applied:\n%s", out)
	}
}

func TestBackupEnvDirForStaging(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/i", BackupEnvDir: "/i/.staging", Edge: "edge", Backup: testBackup()})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "/i/.staging/backup-main-db.env") {
		t.Fatalf("staging backup env path missing:\n%s", out)
	}
}

func TestArchiveFlagsOnlyWhenBackupWired(t *testing.T) {
	p, d, cat := setup(t)
	off, err := Override(p, d, cat, Options{InstanceDir: "/i", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"archive_mode", "archive_command", "archive_timeout", "env_file:\n      - /i/backup"} {
		if strings.Contains(string(off), s) {
			t.Fatalf("%q rendered without a backup target", s)
		}
	}
	if !strings.Contains(string(off), "wal_level=replica") || strings.Contains(string(off), "PGDATA:") {
		t.Fatalf("base command or PGDATA wrong:\n%s", off)
	}
	on, err := Override(p, d, cat, Options{InstanceDir: "/i", Edge: "edge", Backup: testBackup()})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"archive_mode=on", "archive_command=wal-g wal-push %p", "archive_timeout=60", "/i/backup-main-db.env"} {
		if !strings.Contains(string(on), s) {
			t.Fatalf("%q missing with a backup target", s)
		}
	}
}

func TestPostgresConnectsAsClusterRole(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/i", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"PGUSER: u", "PGDATABASE: d", "PGHOST: /var/run/postgresql"} {
		if !strings.Contains(string(out), s) {
			t.Fatalf("%q missing", s)
		}
	}
}

func TestBackupPrefixPerGeneration(t *testing.T) {
	b := testBackup()
	if got := b.Prefix("proj-dev", "main-db", 0); got != "s3://cvctl/h1/proj-dev/main-db" {
		t.Fatal(got)
	}
	if got := b.Prefix("proj-dev", "main-db", 3); got != "s3://cvctl/h1/proj-dev/main-db/g3" {
		t.Fatal(got)
	}
	if b.Env("proj-dev", "main-db", 3)["WALG_S3_PREFIX"] != "s3://cvctl/h1/proj-dev/main-db/g3" {
		t.Fatal("env prefix")
	}
}

func TestLabelBackupNoneOverridesKindDefault(t *testing.T) {
	p, d, cat := setup(t)
	for i := range p.Data {
		if p.Data[i].Name == "main-db" {
			p.Data[i].Backup = "none"
		}
	}
	out, err := Override(p, d, cat, Options{InstanceDir: "/i", Edge: "edge", Backup: testBackup()})
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"archive_mode", "archive_command", "/i/backup-main-db.env", "cvctl-backup"} {
		if strings.Contains(string(out), s) {
			t.Fatalf("%q rendered for backup: none:\n%s", s, out)
		}
	}
	if envs := BackupEnvs(p, d, cat, Options{Backup: testBackup()}); len(envs) != 0 {
		t.Fatalf("backup env for backup: none: %v", envs)
	}
}
