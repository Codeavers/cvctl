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
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
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
	d := &instance.Decisions{}
	rest, err := takeBackupExt(d, map[string]map[string]string{
		"backup": {"bucket": "own", "access_key": "ak", "secret_key": "sk"},
		"media":  {"endpoint": "x"},
	}, "s3", true)
	if err != nil || d.Backup.Ext["bucket"] != "own" || len(rest) != 1 || rest["media"] == nil {
		t.Fatalf("rest=%v ext=%v err=%v", rest, d.Backup.Ext, err)
	}
	for name, tc := range map[string]struct {
		fields  map[string]string
		storage string
		canSet  bool
		msg     string
	}{
		"unknown field":   {map[string]string{"nope": "x"}, "s3", true, "unknown external field"},
		"missing keys":    {map[string]string{"bucket": "b"}, "s3", true, "must both be set"},
		"local storage":   {map[string]string{"bucket": "b", "access_key": "a", "secret_key": "s"}, "local", true, "CVCTL_STORAGE=s3"},
		"after backups":   {map[string]string{"bucket": "b", "access_key": "a", "secret_key": "s"}, "s3", false, "the backup bucket is set at project add; changing it would orphan existing backups"},
		"no host storage": {map[string]string{"bucket": "b", "access_key": "a", "secret_key": "s"}, "", true, "CVCTL_STORAGE=s3"},
	} {
		_, err := takeBackupExt(&instance.Decisions{}, map[string]map[string]string{"backup": tc.fields}, tc.storage, tc.canSet)
		var ue *output.UsageError
		var oe *output.Error
		if !errors.As(err, &ue) || !errors.As(err, &oe) || oe.Code != "E_USAGE_EXT" || !strings.Contains(oe.Message, tc.msg) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if rest, err := takeBackupExt(&instance.Decisions{}, map[string]map[string]string{"db": {"url": "x"}}, "local", false); err != nil || rest["db"] == nil {
		t.Fatalf("other --ext untouched: %v %v", rest, err)
	}
}

func stripBackupKeys(t *testing.T, path string) {
	t.Helper()
	m := readEnv(t, path)
	for k := range m {
		if strings.HasPrefix(k, "CVCTL_BACKUP_") {
			delete(m, k)
		}
	}
	if err := os.WriteFile(path, instance.FormatEnv(m), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestUpdateBacksUpMovedServicesAndWarnsToExport(t *testing.T) {
	e, path := addForBackupKeys(t)
	stripBackupKeys(t, path)
	var log []string
	e.Data = &fakeData{t: t, log: &log}
	res, err := Update(context.Background(), e, "my-service-dev", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(log, ",") != "backup db" {
		t.Fatalf("backups: %v", log)
	}
	if len(res.Warnings) != 1 || res.Warnings[0] != keysCreatedWarning {
		t.Fatalf("warnings: %v", res.Warnings)
	}
	log = nil
	if res, err := Update(context.Background(), e, "my-service-dev", UpdateOptions{}); err != nil || len(log) != 0 || len(res.Warnings) != 0 {
		t.Fatalf("second update must not back up or warn: %v %v %v", log, res.Warnings, err)
	}

	stripBackupKeys(t, path)
	e.Data = &fakeData{t: t, log: &log, backupErr: errors.New("boom")}
	res, err = Update(context.Background(), e, "my-service-dev", UpdateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 2 || !strings.Contains(res.Warnings[1], "run cvctl backup now my-service-dev db") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
}

func TestAfterKeyMigrationNotifies(t *testing.T) {
	n := &recNotify{}
	var log []string
	e := &Env{Data: &fakeData{t: t, log: &log}, Deployer: &deploy.Deployer{Notify: n}, Host: &hostcfg.Config{HostName: "h1"}}
	p := &contract.Project{Data: []contract.DataService{{Name: "db", Kind: "postgres", Backup: "wal-g"}, {Name: "files", Kind: "s3", Backup: "volume"}}}
	d := &instance.Decisions{Instance: "app-dev", Services: map[string]instance.ServiceDecision{
		"db": {Mode: "local", BackupGen: 2, BackupGenMax: 2, BackupKeyGen: 2}, "files": {Mode: "local", BackupKeyGen: 1},
	}}
	w := afterKeyMigration(context.Background(), e, d, p, deploy.Result{Status: state.StatusOK})
	if len(w) != 1 || strings.Join(log, ",") != "backup db,backup files" {
		t.Fatalf("warnings %v log %v", w, log)
	}
	if len(n.msgs) != 1 || n.msgs[0].Level != "warn" || n.msgs[0].Host != "h1" || !strings.Contains(n.msgs[0].Text, keysCreatedWarning) {
		t.Fatalf("notification: %+v", n.msgs)
	}
	log = nil
	w = afterKeyMigration(context.Background(), e, d, p, deploy.Result{Status: state.StatusFailed})
	if len(log) != 0 || len(w) != 3 {
		t.Fatalf("failed deploy: no backups, a hint per service: %v %v", w, log)
	}
}
