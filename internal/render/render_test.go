package render

import (
	"context"
	"crypto/rand"
	"flag"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
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
	p.RawCompose = []byte(strings.Replace(string(p.RawCompose), "  main-db:\n", "  main-db:\n    deploy: { resources: { limits: { memory: 1g } } }\n", 1))
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
	if !strings.Contains(string(out), "memory: 1g") {
		t.Fatalf("main-db must keep the project memory limit:\n%s", out)
	}
	if strings.Count(string(out), "memory: 512m") != 2 {
		t.Fatalf("api and cache keep 512m, main-db must not:\n%s", out)
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

func setupBuckets(t *testing.T) (*contract.Project, *instance.Decisions, kinds.Catalog) {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := contract.Load("testdata/buckets", cat)
	if err != nil {
		t.Fatal(err)
	}
	d := &instance.Decisions{
		Instance: "proj-dev", Env: "dev", Strategy: "recreate",
		Services: map[string]instance.ServiceDecision{
			"media": {
				Mode: "local",
				Creds: kinds.Credentials{
					User: "rootuser", Password: "rootpassword", DB: "s3db",
					AppUser: "media-app", AppPassword: "media-app-pw",
				},
			},
		},
		Domains: map[string]string{
			"api":   "api.example.com",
			"media": "media.example.com",
		},
		Digests: map[string]string{
			"api": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	return p, d, cat
}

func TestOverrideGoldenBuckets(t *testing.T) {
	p, d, cat := setupBuckets(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "override-buckets.yml", out)
}

func TestTraefikGoldenBucketsEdge(t *testing.T) {
	p, d, _ := setupBuckets(t)
	out, err := Traefik(p, d, Options{Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "traefik-buckets-edge.yml", out)
}

func TestTraefikBucketsBehindProxy(t *testing.T) {
	p, d, _ := setupBuckets(t)
	out, err := Traefik(p, d, Options{Edge: "behind-proxy"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "entryPoints:\n        - web\n") {
		t.Fatalf("expected web entryPoint in behind-proxy:\n%s", out)
	}
	if strings.Contains(string(out), "websecure") || strings.Contains(string(out), "certResolver") {
		t.Fatalf("unexpected TLS config in behind-proxy:\n%s", out)
	}
}

func TestTraefikMultiplePublicBucketsRule(t *testing.T) {
	p, d, _ := setupBuckets(t)
	p.X.S3["media"] = []contract.Bucket{
		{Name: "zebra", Public: true},
		{Name: "photos-raw", Public: true},
		{Name: "hidden", Public: false},
		{Name: "alpha.v1", Public: true},
	}
	out, err := Traefik(p, d, Options{Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	wantRule := "rule: Host(`media.example.com`) && (Method(`GET`) || Method(`HEAD`)) && PathRegexp(`^/(alpha\\.v1|photos-raw|zebra)/.+`)"
	if !strings.Contains(string(out), wantRule) {
		t.Fatalf("expected rule %q in output:\n%s", wantRule, out)
	}
}

func TestOverrideIsValidComposeBuckets(t *testing.T) {
	p, d, cat := setupBuckets(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	cp := loadCompose(t, map[string][]byte{"compose.yml": p.RawCompose, "compose.cvctl.yml": out}, []string{"compose.yml", "compose.cvctl.yml"})
	if img := cp.Services["api"].Image; !strings.Contains(img, "@sha256:") {
		t.Fatalf("api image %q", img)
	}
	if img := cp.Services["media"].Image; !strings.Contains(img, "pgsty/silo:") {
		t.Fatalf("media image %q", img)
	}
}

func TestServiceVarsBucketsLocal(t *testing.T) {
	p, d, cat := setupBuckets(t)
	sv, err := ServiceVars(p, d, cat)
	if err != nil {
		t.Fatal(err)
	}
	m := sv["media"]
	if _, ok := m["S3_BUCKET"]; ok {
		t.Fatal("S3_BUCKET must be dropped when buckets are declared")
	}
	if m["S3_BUCKET_PRODUCTS"] != "products" {
		t.Fatalf("S3_BUCKET_PRODUCTS = %q, want %q", m["S3_BUCKET_PRODUCTS"], "products")
	}
	if m["S3_PUBLIC_URL_PRODUCTS"] != "https://media.example.com/products" {
		t.Fatalf("S3_PUBLIC_URL_PRODUCTS = %q, want %q", m["S3_PUBLIC_URL_PRODUCTS"], "https://media.example.com/products")
	}
	if m["S3_BUCKET_PRESCRIPTIONS"] != "prescriptions" {
		t.Fatalf("S3_BUCKET_PRESCRIPTIONS = %q, want %q", m["S3_BUCKET_PRESCRIPTIONS"], "prescriptions")
	}
	if _, ok := m["S3_PUBLIC_URL_PRESCRIPTIONS"]; ok {
		t.Fatal("private bucket must not have public url")
	}
	if m["S3_ACCESS_KEY"] != "media-app" || m["S3_SECRET_KEY"] != "media-app-pw" {
		t.Fatalf("credentials = %v", m)
	}
	if m["S3_ENDPOINT"] != "http://media:9000" {
		t.Fatalf("S3_ENDPOINT = %q", m["S3_ENDPOINT"])
	}
}

func TestServiceVarsBucketsExternal(t *testing.T) {
	p, d, cat := setupBuckets(t)
	d.Services["media"] = instance.ServiceDecision{
		Mode: "external",
		External: map[string]string{
			"endpoint":             "https://s3.amazonaws.com",
			"access_key":           "ak",
			"secret_key":           "sk",
			"region":               "us-east-1",
			"bucket_products":      "ext-prod",
			"public_url_products":  "https://cdn.example.com/products",
			"bucket_prescriptions": "ext-rx",
		},
	}
	sv, err := ServiceVars(p, d, cat)
	if err != nil {
		t.Fatal(err)
	}
	m := sv["media"]
	if _, ok := m["S3_BUCKET"]; ok {
		t.Fatal("S3_BUCKET must be dropped when buckets are declared")
	}
	if m["S3_BUCKET_PRODUCTS"] != "ext-prod" {
		t.Fatalf("S3_BUCKET_PRODUCTS = %q", m["S3_BUCKET_PRODUCTS"])
	}
	if m["S3_PUBLIC_URL_PRODUCTS"] != "https://cdn.example.com/products" {
		t.Fatalf("S3_PUBLIC_URL_PRODUCTS = %q", m["S3_PUBLIC_URL_PRODUCTS"])
	}
	if m["S3_BUCKET_PRESCRIPTIONS"] != "ext-rx" {
		t.Fatalf("S3_BUCKET_PRESCRIPTIONS = %q", m["S3_BUCKET_PRESCRIPTIONS"])
	}
	if _, ok := m["S3_PUBLIC_URL_PRESCRIPTIONS"]; ok {
		t.Fatal("private bucket must not have public url")
	}
	if m["S3_ACCESS_KEY"] != "ak" || m["S3_SECRET_KEY"] != "sk" || m["S3_ENDPOINT"] != "https://s3.amazonaws.com" {
		t.Fatalf("media vars: %v", m)
	}
}

func TestServiceVarsBucketsExternalMissing(t *testing.T) {
	for _, field := range []string{"endpoint", "access_key", "secret_key", "region", "bucket_products", "public_url_products", "bucket_prescriptions"} {
		p, d, cat := setupBuckets(t)
		ext := map[string]string{
			"endpoint":             "https://s3.amazonaws.com",
			"access_key":           "ak",
			"secret_key":           "sk",
			"region":               "us-east-1",
			"bucket_products":      "ext-prod",
			"public_url_products":  "https://cdn.example.com/products",
			"bucket_prescriptions": "ext-rx",
		}
		delete(ext, field)
		d.Services["media"] = instance.ServiceDecision{Mode: "external", External: ext}
		if _, err := ServiceVars(p, d, cat); err == nil {
			t.Fatalf("expected error when missing %s", field)
		}
	}
}

func TestRootPasswordNeverReachesServiceVarsOrAppEnv(t *testing.T) {
	p, d, cat := setupBuckets(t)
	rootPw := "super-secret-root-password-xyz123"
	sd := d.Services["media"]
	sd.Creds.Password = rootPw
	d.Services["media"] = sd

	sv, err := ServiceVars(p, d, cat)
	if err != nil {
		t.Fatal(err)
	}
	for svcName, vars := range sv {
		for k, v := range vars {
			if strings.Contains(v, rootPw) {
				t.Fatalf("root password found in ServiceVars[%q][%q] = %q", svcName, k, v)
			}
		}
	}

	spec, err := envspec.Load("testdata/buckets/env.spec.yml")
	if err != nil {
		t.Fatal(err)
	}
	res, err := envspec.Resolve(spec, envspec.Input{
		Env:         d.Env,
		Existing:    map[string]string{},
		ServiceVars: sv,
		Domains:     d.Domains,
		Rand:        rand.Reader,
	})
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range res.Values {
		if strings.Contains(v, rootPw) {
			t.Fatalf("root password found in resolved app env[%q] = %q", k, v)
		}
	}

	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]map[string]any `yaml:"services"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	apiSvc, ok := doc.Services["api"]
	if !ok {
		t.Fatal("api service missing from override")
	}
	apiBytes, err := yaml.Marshal(apiSvc)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(apiBytes), rootPw) {
		t.Fatalf("root password found in api service rendered override:\n%s", string(apiBytes))
	}
}

func bucketNets(t *testing.T, p *contract.Project, d *instance.Decisions, cat kinds.Catalog) (map[string]any, map[string]any) {
	t.Helper()
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge", Backup: testBackup()})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Services map[string]struct {
			Networks map[string]any `yaml:"networks"`
		} `yaml:"services"`
		Networks map[string]any `yaml:"networks"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	return doc.Services["media"].Networks, doc.Networks
}

func TestOverrideBucketsNotBackedUpByDefault(t *testing.T) {
	p, d, cat := setupBuckets(t)
	mediaNets, _ := bucketNets(t, p, d, cat)
	for _, n := range []string{"data", "edge"} {
		if _, ok := mediaNets[n]; !ok {
			t.Fatalf("media missing %s network", n)
		}
	}
	if _, ok := mediaNets["backup"]; ok {
		t.Fatal("media joined the backup network without cvctl.backup: volume")
	}
}

func TestOverrideBucketsBackedUpWhenOptedIn(t *testing.T) {
	p, d, cat := setupBuckets(t)
	for i := range p.Data {
		if p.Data[i].Name == "media" {
			p.Data[i].Backup = "volume"
		}
	}
	mediaNets, nets := bucketNets(t, p, d, cat)
	if _, ok := mediaNets["backup"]; !ok {
		t.Fatal("media missing backup network")
	}
	if _, ok := nets["backup"]; !ok {
		t.Fatal("top level missing backup network")
	}
}

func TestOverrideBucketsExternal(t *testing.T) {
	p, d, cat := setupBuckets(t)
	d.Services["media"] = instance.ServiceDecision{
		Mode: "external",
		External: map[string]string{
			"endpoint":             "https://s3",
			"access_key":           "ak",
			"secret_key":           "sk",
			"region":               "r",
			"bucket_products":      "p",
			"public_url_products":  "https://cdn.example.com/p",
			"bucket_prescriptions": "rx",
		},
	}
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "media") || strings.Contains(string(out), "cvctl-disabled") {
		t.Fatalf("external media must be omitted from the compose file:\n%s", out)
	}
	tout, err := Traefik(p, d, Options{Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(tout), "proj-dev-media") {
		t.Fatalf("external media should have no router in traefik:\n%s", tout)
	}
}

func TestOverrideIsSelfSufficient(t *testing.T) {
	p, d, cat := setup(t)
	out, err := Override(p, d, cat, Options{InstanceDir: "/srv/inst/proj-dev", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	cp := loadCompose(t, map[string][]byte{"compose.cvctl.yml": out}, []string{"compose.cvctl.yml"})
	if _, ok := cp.Services["media"]; ok {
		t.Fatal("external media must not be in the generated file")
	}
	for name, svc := range cp.Services {
		if svc.Privileged {
			t.Errorf("%s is privileged", name)
		}
		if !slices.Equal(svc.CapDrop, []string{"ALL"}) {
			t.Errorf("%s cap_drop = %v", name, svc.CapDrop)
		}
		if !slices.Equal(svc.SecurityOpt, []string{"no-new-privileges:true"}) {
			t.Errorf("%s security_opt = %v", name, svc.SecurityOpt)
		}
		for _, v := range svc.Volumes {
			if v.Type == types.VolumeTypeBind {
				t.Errorf("%s has a bind mount %s", name, v.Source)
			}
		}
	}
	if got := cp.Services["main-db"].CapAdd; !slices.Equal(got, []string{"CHOWN", "SETUID", "SETGID", "DAC_OVERRIDE", "FOWNER"}) {
		t.Errorf("main-db cap_add = %v", got)
	}
	if len(cp.Services["api"].CapAdd) != 0 {
		t.Errorf("api must have no cap_add")
	}
}

// Generated strings are escaped; values copied from the project keep their
// ${VAR} references for compose to interpolate from --env-file.
func TestOverrideEscapesOnlyGenerated(t *testing.T) {
	p, d, cat := setup(t)
	sd := d.Services["main-db"]
	sd.Creds.Password = "a$b"
	d.Services["main-db"] = sd
	out, err := Override(p, d, cat, Options{InstanceDir: "/x", Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "DATABASE_URL: ${DATABASE_URL}") {
		t.Fatalf("project value must keep ${DATABASE_URL}:\n%s", s)
	}
	if !strings.Contains(s, "a$$b") {
		t.Fatalf("generated password must be escaped:\n%s", s)
	}
	golden(t, "override-escape.yml", out)
}
