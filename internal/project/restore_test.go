package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type fakeData struct {
	t            *testing.T
	store        *state.Store
	log          *[]string
	restoreErr   error
	pushErr      error
	afterRestore func()
	removeErr    error
}

func (f *fakeData) RestoreFresh(_ context.Context, inst, svc string, _ *contract.Project, _ *instance.Decisions, volume string) error {
	if _, err := f.store.Lock(inst); err == nil {
		f.t.Error("instance lock must be held during RestoreFresh")
	}
	*f.log = append(*f.log, "restore "+svc+" "+volume)
	if f.afterRestore != nil && f.restoreErr == nil {
		f.afterRestore()
	}
	return f.restoreErr
}

func (f *fakeData) RemoveVolume(_ context.Context, volume string) error {
	*f.log = append(*f.log, "rm "+volume)
	return f.removeErr
}

func (f *fakeData) BaseBackup(_ context.Context, _, svc string) error {
	*f.log = append(*f.log, "push "+svc)
	return f.pushErr
}

const restoreEnvBase = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_INFRA_URL=git@github.com:org/demo-infra.git
CVCTL_SVC_DB_MODE=local
CVCTL_SVC_DB_VERSION=16
CVCTL_SVC_DB_USER=db
CVCTL_SVC_DB_PASSWORD=existingpass123456789012345678
CVCTL_SVC_DB_DB=db
CVCTL_SVC_DB_BACKUP_GEN=2
CVCTL_SVC_DB_BACKUP_GEN_MAX=3
CVCTL_DOMAIN_WEB=web.example.com
`

type restoreRig struct {
	env     *Env
	store   *state.Store
	data    *fakeData
	log     *[]string
	instDir string
}

func newRestoreRig(t *testing.T, envBody string) *restoreRig {
	t.Helper()
	tmp := t.TempDir()
	instDir := filepath.Join(tmp, "demo-dev")
	originDir := filepath.Join(tmp, "origin")
	writeTestInfra(t, originDir, validCompose, validSpec)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if envBody != "" {
		if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(envBody), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(tmp, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	fl := systest.NewFakeLocker()
	oldLock := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLock })
	sys.LockFile = fl.LockFile

	osfs := sys.NewOSFS()
	stateRoot := filepath.Join(tmp, "state")
	store := state.NewStore(osfs, stateRoot)
	log := &[]string{}
	cat := testCatalog(t)
	deployer := &deploy.Deployer{
		Backup:     &render.BackupTarget{Host: "h1", Endpoint: "http://s3", Bucket: "bk", Region: "r", AccessKey: "ak-ACCESS-0000", SecretKey: "sk-SECRET-0000", LibsodiumKey: "lib-KEY-00000"},
		DeployRoot: tmp,
		TraefikDir: filepath.Join(tmp, "traefik"),
		StateRoot:  stateRoot,
		FS:         osfs,
		Store:      store,
		Catalog:    cat,
		Compose:    func(_, _, _ string) deploy.ComposeAPI { return noopCompose{} },
		Health: func(context.Context, string, string) error {
			*log = append(*log, "health")
			return nil
		},
	}
	stub := &stubRepo{dir: filepath.Join(instDir, "infra"), infraSource: originDir, fetchCommit: "feedface00000000", log: log}
	data := &fakeData{t: t, store: store, log: log}
	e := &Env{
		FS:          osfs,
		DeployRoot:  tmp,
		StateRoot:   stateRoot,
		Catalog:     cat,
		Host:        &hostcfg.Config{Registries: "ghcr.io/codeavers", Storage: "s3"},
		Deployer:    deployer,
		Data:        data,
		Resolver:    fixedResolver{},
		RepoFactory: func(string, string) RepoLike { return stub },
	}
	return &restoreRig{env: e, store: store, data: data, log: log, instDir: instDir}
}

func codeOf(t *testing.T, err error) string {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected output.Error, got %v", err)
	}
	if oe.Hint == "" {
		t.Fatalf("error %s has no hint", oe.Code)
	}
	return oe.Code
}

func TestRestoreOrderOfOperations(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	res, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err != nil {
		var ie *output.IssuesError
		if errors.As(err, &ie) {
			t.Fatalf("%+v", ie.Issues)
		}
		t.Fatal(err)
	}
	want := "clone,restore db demo-dev_db-data,fetch develop,checkout feedface00000000,health,push db"
	if got := strings.Join(*rig.log, ","); got != want {
		t.Fatalf("order\n got %s\nwant %s", got, want)
	}
	if res.Deploy.Status != state.StatusOK || len(res.Volumes) != 1 {
		t.Fatalf("res: %+v", res)
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	for _, w := range []string{"CVCTL_SVC_DB_BACKUP_GEN=4", "CVCTL_SVC_DB_BACKUP_GEN_MAX=4", "CVCTL_INFRA_URL=", "CVCTL_SVC_DB_PASSWORD=existingpass123456789012345678"} {
		if !strings.Contains(string(raw), w) {
			t.Fatalf("cvctl.env missing %q:\n%s", w, raw)
		}
	}
	hist, _ := rig.store.History("demo-dev", 5)
	if len(hist) == 0 || hist[0].Actor != "cli:project-restore" {
		t.Fatalf("history: %+v", hist)
	}
	unlock, err := rig.store.Lock("demo-dev")
	if err != nil {
		t.Fatalf("lock must be released: %v", err)
	}
	unlock()
}

func TestRestoreHoldsLockThroughout(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.env.Deployer.Health = func(context.Context, string, string) error {
		if _, err := rig.store.Lock("demo-dev"); err == nil {
			t.Error("instance lock must be held during deploy")
		}
		return nil
	}
	if _, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRefusesLocalStorage(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.env.Host.Storage = "local"
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_LOCAL_STORAGE" || !strings.Contains(oe.Hint, "local backups died with the host") || len(*rig.log) != 0 {
		t.Fatalf("err=%v log=%v", err, *rig.log)
	}
}

func TestRestoreRequiresImportedSecrets(t *testing.T) {
	rig := newRestoreRig(t, "")
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_NO_SECRETS" || !strings.Contains(oe.Hint, "secrets import") {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreValidatesInstanceFirst(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	_, err := Restore(context.Background(), rig.env, "../etc-dev", RestoreOptions{})
	if codeOf(t, err) != "E_PROJECT_NAME_INVALID" {
		t.Fatalf("got %v", err)
	}
}

func TestRestoreVolumeExistsStopsBeforeDeploy(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.data.restoreErr = &output.Error{Code: "E_RESTORE_VOLUME_EXISTS", Message: "volume exists", Hint: "h"}
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if codeOf(t, err) != "E_RESTORE_VOLUME_EXISTS" {
		t.Fatalf("got %v", err)
	}
	if got := strings.Join(*rig.log, ","); got != "clone,restore db demo-dev_db-data" {
		t.Fatalf("log: %s", got)
	}
	if st, _ := rig.store.Load("demo-dev"); st != nil && st.Current != nil {
		t.Fatalf("nothing may be deployed: %+v", st)
	}
}

func TestRestoreNeedsRepoWhenNotRecorded(t *testing.T) {
	rig := newRestoreRig(t, strings.Replace(restoreEnvBase, "CVCTL_INFRA_URL=git@github.com:org/demo-infra.git\n", "", 1))
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if codeOf(t, err) != "E_RESTORE_NO_REPO" {
		t.Fatalf("got %v", err)
	}
	if _, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{Repo: "git@github.com:org/demo-infra.git"}); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreBaseBackupFailureIsWarning(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.data.pushErr = errors.New("boom")
	res, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err != nil || len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "backup now") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestAddRecordsInfraURL(t *testing.T) {
	t.Setenv("CVCTL_ALLOW_FILE_REPOS", "1")
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, validCompose, validSpec)
	stub := &stubRepo{infraSource: infraDir}
	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Catalog:     testCatalog(t),
		RepoFactory: func(dir, _ string) RepoLike { r := *stub; r.dir = dir; return &r },
	}
	if _, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{}); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "repo-dev", "cvctl.env"))
	if !strings.Contains(string(raw), "CVCTL_INFRA_URL=file:///tmp/repo-infra") {
		t.Fatalf("cvctl.env:\n%s", raw)
	}
}

func TestUpdateRecordsMissingInfraURL(t *testing.T) {
	rig := newUpdateRig(t, map[string]string{"data_extra_mode": "local"})
	run := &systest.Runner{}
	run.OnPrefix("git -C "+rig.infraDir+" remote get-url origin", sys.Result{Stdout: []byte("git@github.com:org/demo-infra.git\n")})
	rig.env.Run = run
	if _, err := Update(context.Background(), rig.env, "demo-dev"); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	if !strings.Contains(string(raw), "CVCTL_INFRA_URL=git@github.com:org/demo-infra.git") {
		t.Fatalf("cvctl.env:\n%s", raw)
	}
}

func TestRestoreDeploysAtTheNewGeneration(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	var atDeploy string
	rig.env.Deployer.Health = func(context.Context, string, string) error {
		b, _ := os.ReadFile(filepath.Join(rig.instDir, "backup-db.env"))
		atDeploy = string(b)
		return nil
	}
	if _, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(atDeploy, "/demo-dev/db/g4") || strings.Contains(atDeploy, "/g2") {
		t.Fatalf("deploy must render the new generation:\n%s", atDeploy)
	}
}

func TestRestoreFailedDeployKeepsDataAndGenerations(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.env.Deployer.Health = func(context.Context, string, string) error { return errors.New("unhealthy") }
	res, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	var oe *output.Error
	if !errors.As(err, &oe) || res == nil || len(res.Volumes) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, w := range []string{"demo-dev_db-data", "cvctl project update demo-dev"} {
		if !strings.Contains(oe.Hint, w) {
			t.Fatalf("hint %q lacks %q", oe.Hint, w)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	if !strings.Contains(string(raw), "CVCTL_SVC_DB_BACKUP_GEN=4") {
		t.Fatalf("generation must stay advanced:\n%s", raw)
	}
	if _, err := os.Stat(filepath.Join(rig.instDir, "infra")); err != nil {
		t.Fatalf("infra must be kept: %v", err)
	}
	for _, l := range *rig.log {
		if strings.HasPrefix(l, "rm ") || l == "push db" {
			t.Fatalf("nothing is cleaned or pushed after a failed deploy: %v", *rig.log)
		}
	}
}

func TestRestoreFailureBeforeDeployCleansWhatItCreated(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	envPath := filepath.Join(rig.instDir, "cvctl.env")
	orig, _ := os.ReadFile(envPath)
	rig.data.afterRestore = func() {
		// a directory in place of the file makes the generation write fail
		_ = os.Remove(envPath)
		_ = os.Mkdir(envPath, 0o755)
	}
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_FILE_WRITE" || !strings.Contains(oe.Message, "cleaned up") || !strings.Contains(oe.Message, "volume demo-dev_db-data") {
		t.Fatalf("err=%v", err)
	}
	if got := strings.Join(*rig.log, ","); got != "clone,restore db demo-dev_db-data,rm demo-dev_db-data" {
		t.Fatalf("log: %s", got)
	}
	if _, statErr := os.Stat(filepath.Join(rig.instDir, "infra")); statErr == nil {
		t.Fatal("the cloned infra must be removed")
	}
	_ = orig
}

func TestRestoreRestoreFreshFailureRemovesClone(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.data.restoreErr = &output.Error{Code: "E_RESTORE_FAILED", Message: "boom", Hint: "h"}
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	var oe *output.Error
	if !errors.As(err, &oe) || !strings.Contains(oe.Message, "cleaned up") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rig.instDir, "infra")); statErr == nil {
		t.Fatal("the cloned infra must be removed")
	}
	if got := strings.Join(*rig.log, ","); strings.Contains(got, "rm ") {
		t.Fatalf("a volume that was never created must not be removed: %s", got)
	}
}

func TestRestoreExistingInfraHint(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	if err := os.MkdirAll(filepath.Join(rig.instDir, "infra"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_INFRA_EXISTS" || !strings.Contains(oe.Hint, "cvctl project update demo-dev") {
		t.Fatalf("err=%v", err)
	}
	if _, statErr := os.Stat(filepath.Join(rig.instDir, "infra")); statErr != nil {
		t.Fatal("a pre-existing infra dir must never be removed")
	}
}

func TestStorableInfraURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		stripped bool
	}{
		{"https://x-token:ghp_SECRET@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"https://ghp_SECRET@github.com/org/repo.git", "https://github.com/org/repo.git", true},
		{"https://github.com/org/repo.git", "https://github.com/org/repo.git", false},
		{"https://github.com/org/a@b.git", "https://github.com/org/a@b.git", false},
		{"ssh://git@github.com/org/repo.git", "ssh://git@github.com/org/repo.git", false},
		{"https://u:p@ss@host/x", "https://host/x", true},
		{"ssh://:pw@h/x", "ssh://h/x", true},
		{"ssh://u:p@ss@h/x", "ssh://u@h/x", true},
		{"ssh://git:hunter2@github.com/org/repo.git", "ssh://git@github.com/org/repo.git", true},
		{"git@github.com:org/repo.git", "git@github.com:org/repo.git", false},
	} {
		got, stripped := storableInfraURL(tc.in)
		if got != tc.want || stripped != tc.stripped {
			t.Errorf("%s: got %q %v", tc.in, got, stripped)
		}
	}
}

func TestUpdateStripsCredentialsFromRecordedURL(t *testing.T) {
	rig := newUpdateRig(t, map[string]string{"data_extra_mode": "local"})
	run := &systest.Runner{}
	run.OnPrefix("git -C "+rig.infraDir+" remote get-url origin", sys.Result{Stdout: []byte("https://x:ghp_SECRET@github.com/org/demo-infra.git\n")})
	rig.env.Run = run
	res, err := Update(context.Background(), rig.env, "demo-dev")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	if strings.Contains(string(raw), "ghp_SECRET") || !strings.Contains(string(raw), "CVCTL_INFRA_URL=https://github.com/org/demo-infra.git") {
		t.Fatalf("cvctl.env:\n%s", raw)
	}
	if len(res.Warnings) != 1 || strings.Contains(res.Warnings[0], "ghp_SECRET") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
}

func TestAddStripsCredentialsFromStoredURL(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, validCompose, validSpec)
	stub := &stubRepo{infraSource: infraDir}
	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Catalog:     testCatalog(t),
		RepoFactory: func(dir, _ string) RepoLike { r := *stub; r.dir = dir; return &r },
	}
	res, err := Add(context.Background(), e, "https://x:ghp_SECRET@github.com/org/repo-infra.git", AddOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "repo-dev", "cvctl.env"))
	if strings.Contains(string(raw), "ghp_SECRET") || !strings.Contains(string(raw), "CVCTL_INFRA_URL=https://github.com/org/repo-infra.git") {
		t.Fatalf("cvctl.env:\n%s", raw)
	}
	if len(res.Warnings) != 1 || strings.Contains(res.Warnings[0], "ghp_SECRET") {
		t.Fatalf("warnings: %v", res.Warnings)
	}
}

func TestRepoURLErrorRedactsUppercaseSchemeCredentials(t *testing.T) {
	err := validateRepoURL("HTTPS://tok-SECRET@github.com/org/repo.git")
	if err == nil || strings.Contains(err.Error(), "tok-SECRET") {
		t.Fatalf("err=%v", err)
	}
}

// failWrites fails state writes (and, once armed, cvctl.env writes) to drive the
// failure after the generation write.
type failWrites struct {
	sys.FS
	armed *bool
}

func (f failWrites) WriteFile(path string, data []byte, mode os.FileMode, o sys.Owner) error {
	if strings.HasSuffix(path, "state.json") {
		*f.armed = true
		return errors.New("state disk full")
	}
	if *f.armed && strings.HasSuffix(path, "cvctl.env") {
		return errors.New("env disk full")
	}
	return f.FS.WriteFile(path, data, mode, o)
}

func failStateSave(rig *restoreRig, revertAlsoFails bool) {
	armed := false
	osfs := sys.NewOSFS()
	rig.env.Deployer.Store = state.NewStore(failWrites{FS: osfs, armed: &armed}, rig.env.StateRoot)
	if revertAlsoFails {
		rig.env.FS = failWrites{FS: osfs, armed: &armed}
	} else {
		never := false
		rig.env.FS = failWrites{FS: osfs, armed: &never}
	}
}

func TestRestoreFailureAfterGenerationWriteRevertsCvctlEnv(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	failStateSave(rig, false)
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err == nil {
		t.Fatal("expected failure")
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	if string(raw) != restoreEnvBase {
		t.Fatalf("cvctl.env must be back to the original:\n%s", raw)
	}
	if !strings.Contains(err.Error(), "backup generations in cvctl.env") || !strings.Contains(err.Error(), "volume demo-dev_db-data") {
		t.Fatalf("err=%v", err)
	}
}

func TestRestoreRevertFailureNamesTheKeys(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	failStateSave(rig, true)
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, w := range []string{
		"could not revert backup generations in cvctl.env",
		"set CVCTL_SVC_DB_BACKUP_GEN back to 2",
		"CVCTL_SVC_DB_BACKUP_GEN_MAX back to 3",
	} {
		if !strings.Contains(err.Error(), w) {
			t.Fatalf("err %q lacks %q", err, w)
		}
	}
	if strings.Contains(err.Error(), "existingpass") {
		t.Fatalf("secret in error: %v", err)
	}
}

func TestRestoreReportsWhatCouldNotBeRemoved(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	failStateSave(rig, false)
	rig.data.removeErr = errors.New("volume in use")
	_, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err == nil || !strings.Contains(err.Error(), "not removed: volume demo-dev_db-data") {
		t.Fatalf("err=%v", err)
	}
}

func TestRestoreDeployErrorReportsFailedStatusAndHint(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	rig.env.Deployer.Health = func(context.Context, string, string) error { return errors.New("unhealthy") }
	res, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err == nil || res == nil || res.Deploy.Status == "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	var oe *output.Error
	if !errors.As(err, &oe) || !strings.Contains(oe.Hint, "cvctl backup now demo-dev <service> for each wal-g service") {
		t.Fatalf("err=%v", err)
	}
}
