package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

func addForBackupKeys(t *testing.T) (*Env, string) {
	t.Helper()
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, validCompose, validSpec)
	stub := &stubRepo{infraSource: infraDir}
	e := &Env{
		FS: sys.NewOSFS(), DeployRoot: tmp, StateRoot: filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: true, Catalog: testCatalog(t),
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}
	if _, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{}); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	return e, filepath.Join(tmp, "my-service-dev", "cvctl.env")
}

func readEnv(t *testing.T, path string) map[string]string {
	t.Helper()
	m, err := godotenv.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestAddGeneratesBackupKeys(t *testing.T) {
	_, path := addForBackupKeys(t)
	m := readEnv(t, path)
	checks := map[string]*regexp.Regexp{
		"CVCTL_BACKUP_ACCESS_KEY":      regexp.MustCompile(`^bk-my-service-dev$`),
		"CVCTL_BACKUP_SECRET_KEY":      regexp.MustCompile(`^[A-Za-z0-9]{32}$`),
		"CVCTL_BACKUP_LIBSODIUM_KEY":   regexp.MustCompile(`^[0-9a-f]{64}$`),
		"CVCTL_BACKUP_RESTIC_PASSWORD": regexp.MustCompile(`^[A-Za-z0-9]{40}$`),
	}
	for k, re := range checks {
		if !re.MatchString(m[k]) {
			t.Errorf("%s = %q", k, m[k])
		}
	}
	for k := range m {
		if strings.HasSuffix(k, "_BACKUP_KEY_GEN") || strings.HasSuffix(k, "_BACKUP_GEN") {
			t.Errorf("a new instance needs no generation change: %s", k)
		}
	}
}

func TestUpdateGeneratesKeysAndMovesToNewGeneration(t *testing.T) {
	e, path := addForBackupKeys(t)
	m := readEnv(t, path)
	for k := range m {
		if strings.HasPrefix(k, "CVCTL_BACKUP_") {
			delete(m, k)
		}
	}
	m["CVCTL_SVC_DB_BACKUP_GEN"] = "1"
	m["CVCTL_SVC_DB_BACKUP_GEN_MAX"] = "2"
	if err := os.WriteFile(path, instance.FormatEnv(m), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(context.Background(), e, "my-service-dev", UpdateOptions{}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got := readEnv(t, path)
	for k, want := range map[string]string{"CVCTL_SVC_DB_BACKUP_GEN": "3", "CVCTL_SVC_DB_BACKUP_GEN_MAX": "3", "CVCTL_SVC_DB_BACKUP_KEY_GEN": "3"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["CVCTL_BACKUP_LIBSODIUM_KEY"] == "" || got["CVCTL_BACKUP_RESTIC_PASSWORD"] == "" || got["CVCTL_BACKUP_SECRET_KEY"] == "" {
		t.Fatalf("keys not generated: %v", got)
	}
	if _, err := Update(context.Background(), e, "my-service-dev", UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	again := readEnv(t, path)
	for _, k := range []string{"CVCTL_BACKUP_LIBSODIUM_KEY", "CVCTL_BACKUP_SECRET_KEY", "CVCTL_SVC_DB_BACKUP_KEY_GEN", "CVCTL_SVC_DB_BACKUP_GEN"} {
		if again[k] != got[k] {
			t.Errorf("%s changed on a second update", k)
		}
	}
}

func TestMigrateBackupKeyGenRestic(t *testing.T) {
	p := &contract.Project{Data: []contract.DataService{
		{Name: "db", Kind: "postgres", Backup: "wal-g"},
		{Name: "files", Kind: "s3", Backup: "volume"},
		{Name: "ext", Kind: "postgres", Backup: "wal-g"},
	}}
	d := &instance.Decisions{Services: map[string]instance.ServiceDecision{
		"db": {Mode: "local", BackupGen: 4}, "files": {Mode: "local"}, "ext": {Mode: "external"},
	}}
	migrateBackupKeyGen(d, p, map[string]int{"db": 6})
	if s := d.Services["db"]; s.BackupGen != 7 || s.BackupGenMax != 7 || s.BackupKeyGen != 7 {
		t.Errorf("db %+v", s)
	}
	if s := d.Services["files"]; s.BackupKeyGen != 1 {
		t.Errorf("files %+v", s)
	}
	if s := d.Services["ext"]; s.BackupKeyGen != 0 {
		t.Errorf("external %+v", s)
	}
}

func TestTakeBackupExt(t *testing.T) {
	p := &contract.Project{}
	d := &instance.Decisions{}
	rest, err := takeBackupExt(d, p, map[string]map[string]string{
		"backup": {"bucket": "own", "access_key": "ak", "secret_key": "sk"},
		"media":  {"endpoint": "x"},
	})
	if err != nil || d.Backup.Ext["bucket"] != "own" || len(rest) != 1 || rest["media"] == nil {
		t.Fatalf("rest=%v ext=%v err=%v", rest, d.Backup.Ext, err)
	}
	var ue *output.UsageError
	if _, err := takeBackupExt(&instance.Decisions{}, p, map[string]map[string]string{"backup": {"nope": "x"}}); !errors.As(err, &ue) {
		t.Fatalf("unknown field: %v", err)
	}
	if _, err := takeBackupExt(&instance.Decisions{}, p, map[string]map[string]string{"backup": {"bucket": "b"}}); !errors.As(err, &ue) {
		t.Fatalf("missing keys: %v", err)
	}
	withSvc := &contract.Project{Data: []contract.DataService{{Name: "backup"}}}
	ext := map[string]map[string]string{"backup": {"url": "x"}}
	if rest, err := takeBackupExt(&instance.Decisions{}, withSvc, ext); err != nil || rest["backup"] == nil {
		t.Fatalf("a data service named backup keeps its --ext: %v %v", rest, err)
	}
}
