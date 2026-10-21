package backup_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/secrets"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const (
	resticPass = "restic-pass-SECRET-123"
	libsodium  = "libsodium-key-SECRET-456"
	secretKey  = "s3-secret-SECRET-789"
)

type recNotifier struct {
	mu   sync.Mutex
	msgs []notify.Message
}

func (n *recNotifier) Send(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.msgs = append(n.msgs, m)
	return nil
}

type rig struct {
	run   *systest.Runner
	fs    *systest.FS
	store *state.Store
	note  *recNotifier
	r     *backup.Runner
}

func newRig(t *testing.T) *rig {
	t.Helper()
	old := sys.LockFile
	sys.LockFile = systest.NoopLockFile
	t.Cleanup(func() { sys.LockFile = old })

	run := &systest.Runner{}
	run.On("docker network inspect -f {{.Internal}} app-prod_backup", sys.Result{})
	fsys := systest.NewFS()
	_ = fsys.MkdirAll("/srv/deploy/app-prod", 0o755, sys.Owner{})
	_ = fsys.WriteFile("/srv/deploy/app-prod/cvctl.env", []byte("CVCTL_INSTANCE=app-prod\n"), 0o600, sys.Owner{})
	store := state.NewStore(fsys, "/var/lib/cvctl")
	note := &recNotifier{}
	p := &contract.Project{Data: []contract.DataService{
		{Name: "db", Kind: "postgres", Backup: "wal-g"},
		{Name: "files", Kind: "s3", Backup: "volume"},
		{Name: "cache", Kind: "redis", Backup: "none"},
		{Name: "ext", Kind: "postgres", Backup: "wal-g"},
	}}
	d := &instance.Decisions{Instance: "app-prod", Services: map[string]instance.ServiceDecision{
		"db":    {Mode: "local"},
		"files": {Mode: "local", Volume: "app-prod_files-data"},
		"cache": {Mode: "local"},
		"ext":   {Mode: "external"},
	}}
	r := &backup.Runner{
		Run: run, FS: fsys, Store: store, Notify: note,
		Compose: func(inst string) *compose.Project {
			return &compose.Project{Run: run, Instance: inst, Dir: "/srv/deploy/" + inst}
		},
		Load: func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil },
		Host: &render.BackupTarget{
			Host: "h1", Endpoint: "http://cvctl-storage:9000", Bucket: "bk", Region: "us-east-1",
			AccessKey: "s3-access-ACCESS-000", SecretKey: secretKey, LibsodiumKey: libsodium, ResticPassword: resticPass,
		},
		Clock:      func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) },
		Suffix:     func() string { return "0a0b0c0d" },
		DeployRoot: "/srv/deploy",
	}
	return &rig{run: run, fs: fsys, store: store, note: note, r: r}
}

func (g *rig) calls() []string {
	var out []string
	for _, c := range g.run.Calls {
		out = append(out, c.String())
	}
	return out
}

func (g *rig) okAll() {
	g.run.OnPrefix("docker", sys.Result{})
}

func TestTargetsOnlyLocalWithBackup(t *testing.T) {
	g := newRig(t)
	p, d, _ := g.r.Load("app-prod")
	ts := backup.Targets(p, d)
	if len(ts) != 2 || ts[0].Service != "db" || ts[0].Strategy != "wal-g" || ts[1].Service != "files" || ts[1].Volume != "app-prod_files-data" {
		t.Fatalf("targets: %+v", ts)
	}
}

func TestWalgSequence(t *testing.T) {
	g := newRig(t)
	g.okAll()
	res, err := g.r.BackupNow(context.Background(), "app-prod", "db")
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	calls := g.calls()
	if len(calls) != 2 {
		t.Fatalf("calls: %v", calls)
	}
	if !strings.HasSuffix(calls[0], "exec -T -u postgres db wal-g backup-push /var/lib/postgresql/data") {
		t.Fatalf("push: %s", calls[0])
	}
	if !strings.HasSuffix(calls[1], "exec -T -u postgres db wal-g delete retain FULL 7 --after 2025-12-26T03:04:05Z --confirm") {
		t.Fatalf("retain: %s", calls[1])
	}
	for _, c := range g.run.Calls {
		if len(c.Env) != 1 || c.Env[0] != "DOCKER_CONFIG=/var/lib/cvctl/docker" {
			t.Fatalf("env: %v", c.Env)
		}
	}
}

const resticPrefix = "docker run --rm --network app-prod_backup "

func TestResticInitsMissingRepo(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 snapshots", sys.Result{ExitCode: 10, Stderr: []byte("Fatal: repository does not exist")})
	res, err := g.r.BackupNow(context.Background(), "app-prod", "files")
	if err != nil || !res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	want := []string{
		"docker network inspect -f {{.Internal}} app-prod_backup",
		resticPrefix + "--env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 snapshots --json",
		resticPrefix + "--env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 init",
		resticPrefix + "-v app-prod_files-data:/data:ro --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 backup /data --host h1 --tag app-prod",
		resticPrefix + "--env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 forget --keep-daily 7 --keep-weekly 4 --prune",
	}
	got := g.calls()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

func TestResticSkipsInitWhenRepoExists(t *testing.T) {
	g := newRig(t)
	g.okAll()
	if _, err := g.r.BackupNow(context.Background(), "app-prod", "files"); err != nil {
		t.Fatal(err)
	}
	for _, c := range g.calls() {
		if strings.HasSuffix(c, " init") {
			t.Fatalf("init must not run: %v", g.calls())
		}
	}
}

func TestResticEnvFileHoldsSecretsArgvDoesNot(t *testing.T) {
	g := newRig(t)
	g.okAll()
	if _, err := g.r.BackupNow(context.Background(), "app-prod", ""); err != nil {
		t.Fatal(err)
	}
	path := "/srv/deploy/app-prod/restic-files.env"
	raw, err := g.fs.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{
		"RESTIC_REPOSITORY=s3:http://cvctl-storage:9000/bk/h1/app-prod/files",
		"RESTIC_PASSWORD=" + resticPass,
		"AWS_SECRET_ACCESS_KEY=" + secretKey,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("env file missing %q:\n%s", want, body)
		}
	}
	fi, _ := g.fs.Stat(path)
	if fi.Mode != 0o640 || fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("env file meta: %+v", fi)
	}
	for _, c := range g.run.Calls {
		for _, a := range c.Args {
			for _, s := range []string{resticPass, libsodium, secretKey} {
				if strings.Contains(a, s) {
					t.Fatalf("secret in argv: %v", c.Args)
				}
			}
		}
	}
}

func TestFailureRecordedRedactedAndNotifiedOnce(t *testing.T) {
	g := newRig(t)
	g.run.OnPrefix("docker", sys.Result{ExitCode: 1, Stderr: []byte("boom " + resticPass + " " + libsodium + " " + secretKey)})
	for i := 0; i < 2; i++ {
		res, err := g.r.BackupNow(context.Background(), "app-prod", "db")
		if err == nil || res[0].OK {
			t.Fatalf("expected failure, res=%+v", res)
		}
		for _, s := range []string{resticPass, libsodium, secretKey} {
			if strings.Contains(err.Error(), s) || strings.Contains(res[0].Error, s) {
				t.Fatalf("secret leaked in error: %v / %s", err, res[0].Error)
			}
		}
	}
	st, _ := g.store.Load("app-prod")
	bs := st.Backups["db"]
	if bs.LastError == "" || !bs.LastOK.IsZero() || bs.LastAttempt.IsZero() {
		t.Fatalf("status: %+v", bs)
	}
	if len(g.note.msgs) != 1 {
		t.Fatalf("want 1 notification, got %d", len(g.note.msgs))
	}
	m := g.note.msgs[0]
	if m.Level != "error" {
		t.Fatalf("level %q", m.Level)
	}
	for _, s := range []string{resticPass, libsodium, secretKey} {
		if strings.Contains(m.Text+m.Title, s) {
			t.Fatal("secret in notification")
		}
	}
}

func TestSuccessRecordedAndClearsError(t *testing.T) {
	g := newRig(t)
	g.run.OnPrefix("docker", sys.Result{ExitCode: 1, Stderr: []byte("down")})
	_, _ = g.r.BackupNow(context.Background(), "app-prod", "db")
	g.run.OnPrefix("docker", sys.Result{})
	if _, err := g.r.BackupNow(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	st, _ := g.store.Load("app-prod")
	bs := st.Backups["db"]
	if bs.LastError != "" || bs.LastOK.IsZero() {
		t.Fatalf("status: %+v", bs)
	}
}

func TestNoHostIsNotConfigured(t *testing.T) {
	g := newRig(t)
	g.r.Host = nil
	_, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || !strings.Contains(err.Error(), "E_BACKUP_NOT_CONFIGURED") {
		t.Fatalf("err=%v", err)
	}
}

func TestUnknownServiceNoTarget(t *testing.T) {
	g := newRig(t)
	_, err := g.r.BackupNow(context.Background(), "app-prod", "cache")
	if err == nil || !strings.Contains(err.Error(), "E_BACKUP_NO_TARGET") {
		t.Fatalf("err=%v", err)
	}
}

func TestListParsesWalgAndRestic(t *testing.T) {
	g := newRig(t)
	g.run.OnPrefix("docker compose", sys.Result{Stdout: []byte(`[{"backup_name":"base_0001","time":"2026-01-01T00:00:00Z"}]`)})
	g.run.OnPrefix("docker run", sys.Result{Stdout: []byte(`[{"id":"abcdef0123","short_id":"abcdef01","time":"2026-01-02T00:00:00Z"}]`)})
	snaps, err := g.r.List(context.Background(), "app-prod", "")
	if err != nil || len(snaps) != 3 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}
	if snaps[0].ID != "base_0001" || snaps[0].Strategy != "wal-g" || snaps[1].ID != "abcdef01" || snaps[1].Strategy != "volume" || snaps[2].ID != "abcdef01" || snaps[2].Strategy != "env" {
		t.Fatalf("snaps=%+v", snaps)
	}
	if !strings.HasSuffix(g.calls()[0], "wal-g backup-list --json") {
		t.Fatalf("calls %v", g.calls())
	}
}

type busyLocker struct{ busy int }

// busyLocker keeps the instance lock busy; the host data lock stays free.
func (b *busyLocker) lockFile(p string, _ os.FileMode, _ sys.Owner) (func(), error) {
	if b.busy != 0 && strings.HasSuffix(p, "/lock") {
		if b.busy > 0 {
			b.busy--
		}
		return nil, sys.ErrLocked
	}
	return func() {}, nil
}

func TestStatusWriteRetriesWhileLocked(t *testing.T) {
	g := newRig(t)
	g.okAll()
	b := &busyLocker{busy: 3}
	sys.LockFile = b.lockFile
	var slept []time.Duration
	g.r.LockInterval, g.r.LockTimeout = 15*time.Second, 30*time.Minute
	g.r.Sleep = func(d time.Duration) { slept = append(slept, d) }
	if _, err := g.r.BackupNow(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if len(slept) != 3 || slept[0] != 15*time.Second {
		t.Fatalf("slept %v", slept)
	}
	st, _ := g.store.Load("app-prod")
	if st.Backups["db"].LastOK.IsZero() {
		t.Fatalf("status not recorded: %+v", st.Backups)
	}
}

func TestStatusWriteFailureDoesNotAbortBackups(t *testing.T) {
	g := newRig(t)
	g.okAll()
	sys.LockFile = (&busyLocker{busy: -1}).lockFile
	g.r.Sleep = func(time.Duration) {}
	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || !strings.Contains(err.Error(), "E_BACKUP_STATUS_WRITE") || strings.Contains(err.Error(), "E_BACKUP_FAILED") {
		t.Fatalf("err=%v", err)
	}
	if len(res) != 3 || !res[0].OK || !res[1].OK || !res[2].OK {
		t.Fatalf("all targets must still run: %+v", res)
	}
	if len(g.run.Calls) < 3 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func code(err error) string {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

func TestEnvBackupRunsWithNoDataServices(t *testing.T) {
	g := newRig(t)
	g.okAll()
	p := &contract.Project{Data: nil}
	d := &instance.Decisions{Instance: "app-prod", Services: nil}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }

	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 || res[0].Service != "env" || res[0].Strategy != "env" || !res[0].OK {
		t.Fatalf("unexpected results: %+v", res)
	}
	st, err := g.store.Load("app-prod")
	if err != nil {
		t.Fatal(err)
	}
	bs := st.Backups["env"]
	if bs.LastOK.IsZero() || bs.LastError != "" {
		t.Fatalf("status: %+v", bs)
	}
}

func TestBackupOrderDataFirstThenEnv(t *testing.T) {
	g := newRig(t)
	g.okAll()
	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(res), res)
	}
	if res[0].Service != "db" || res[1].Service != "files" || res[2].Service != "env" {
		t.Fatalf("order of results: %+v", res)
	}
	calls := g.calls()
	var walgIdx, volumeIdx, envIdx int = -1, -1, -1
	for i, c := range calls {
		if strings.Contains(c, "wal-g backup-push") {
			walgIdx = i
		}
		if strings.Contains(c, "backup /data") {
			volumeIdx = i
		}
		if strings.Contains(c, "--stdin-filename env.tar") {
			envIdx = i
		}
	}
	if walgIdx == -1 || volumeIdx == -1 || envIdx == -1 {
		t.Fatalf("missing expected calls: walg=%d, vol=%d, env=%d (all=%v)", walgIdx, volumeIdx, envIdx, calls)
	}
	if !(walgIdx < volumeIdx && volumeIdx < envIdx) {
		t.Fatalf("wrong call order: walg=%d, volume=%d, env=%d", walgIdx, volumeIdx, envIdx)
	}
}

func TestBackupNowOnlyEnv(t *testing.T) {
	g := newRig(t)
	g.okAll()
	res, err := g.r.BackupNow(context.Background(), "app-prod", "env")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(res) != 1 || res[0].Service != "env" || res[0].Strategy != "env" || !res[0].OK {
		t.Fatalf("results: %+v", res)
	}
	for _, c := range g.calls() {
		if strings.Contains(c, "wal-g") || strings.Contains(c, "backup /data") {
			t.Fatalf("data service call ran during only=env: %s", c)
		}
	}
}

func TestEnvBackupFailureDoesNotStopDataTargets(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix("docker run -i --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 backup --stdin", sys.Result{ExitCode: 1, Stderr: []byte("s3 network failure")})
	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(res), res)
	}
	if !res[0].OK || !res[1].OK || res[2].OK {
		t.Fatalf("expected data targets OK, env failed: %+v", res)
	}
	if res[2].Service != "env" || !strings.Contains(res[2].Error, "s3 network failure") {
		t.Fatalf("env error: %v", res[2].Error)
	}
	st, _ := g.store.Load("app-prod")
	if st.Backups["env"].LastError == "" || !st.Backups["env"].LastOK.IsZero() {
		t.Fatalf("env status: %+v", st.Backups["env"])
	}
	if st.Backups["db"].LastOK.IsZero() || st.Backups["files"].LastOK.IsZero() {
		t.Fatalf("data targets status: %+v", st.Backups)
	}
}

func TestDataFailureDoesNotStopEnvBackup(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix("docker compose", sys.Result{ExitCode: 1, Stderr: []byte("postgres connection refused")})
	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	if len(res) != 3 {
		t.Fatalf("want 3 results, got %d: %+v", len(res), res)
	}
	if res[0].OK || !res[1].OK || !res[2].OK {
		t.Fatalf("expected db failed, files and env OK: %+v", res)
	}
	st, _ := g.store.Load("app-prod")
	if st.Backups["db"].LastError == "" {
		t.Fatalf("db status: %+v", st.Backups["db"])
	}
	if st.Backups["env"].LastOK.IsZero() || st.Backups["env"].LastError != "" {
		t.Fatalf("env status: %+v", st.Backups["env"])
	}
}

func TestEnvBackupArgvNeverContainsFileContents(t *testing.T) {
	g := newRig(t)
	g.okAll()
	secretPayload := "TOP_SECRET_CREDENTIAL=supersecret123456"
	_ = g.fs.WriteFile("/srv/deploy/app-prod/cvctl.env", []byte("CVCTL_INSTANCE=app-prod\n"+secretPayload+"\n"), 0o600, sys.Owner{})

	if _, err := g.r.BackupNow(context.Background(), "app-prod", "env"); err != nil {
		t.Fatal(err)
	}

	foundStdin := false
	for _, c := range g.run.Calls {
		for _, a := range c.Args {
			if strings.Contains(a, secretPayload) || strings.Contains(a, "supersecret123456") {
				t.Fatalf("file content leaked in argv: %s (args=%v)", a, c.Args)
			}
		}
		if len(c.Stdin) > 0 && strings.Contains(string(c.Stdin), "supersecret123456") {
			foundStdin = true
		}
	}
	if !foundStdin {
		t.Fatal("secret payload not found in any command Stdin")
	}
}

func TestEnvBackupUsesHostKeyEvenWithInstanceKeys(t *testing.T) {
	g := newRig(t)
	g.okAll()
	instAccess := "inst-access-key-777"
	instSecret := "inst-secret-key-777"
	instPass := "instance-restic-password-999"
	p, d, _ := g.r.Load("app-prod")
	d.Backup = instance.BackupDecision{
		AccessKey:      instAccess,
		SecretKey:      instSecret,
		LibsodiumKey:   "inst-libsodium-key-888",
		ResticPassword: instPass,
	}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }

	if _, err := g.r.BackupNow(context.Background(), "app-prod", "env"); err != nil {
		t.Fatal(err)
	}
	envPath := "/srv/deploy/app-prod/restic-env.env"
	raw, err := g.fs.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(raw)
	if !strings.Contains(content, "AWS_ACCESS_KEY_ID=s3-access-ACCESS-000") {
		t.Fatalf("restic-env.env must contain host AWS_ACCESS_KEY_ID, got:\n%s", content)
	}
	if !strings.Contains(content, "AWS_SECRET_ACCESS_KEY="+secretKey) {
		t.Fatalf("restic-env.env must contain host AWS_SECRET_ACCESS_KEY, got:\n%s", content)
	}
	if !strings.Contains(content, "RESTIC_PASSWORD="+resticPass) {
		t.Fatalf("restic-env.env must contain host password %q, got:\n%s", resticPass, content)
	}
	expectedRepo := g.r.Host.ResticEnv("app-prod", "env", 0)["RESTIC_REPOSITORY"]
	if !strings.HasSuffix(expectedRepo, "/h1/app-prod/env") {
		t.Fatalf("host repo must end in /h1/app-prod/env, got %s", expectedRepo)
	}
	if !strings.Contains(content, "RESTIC_REPOSITORY="+expectedRepo) {
		t.Fatalf("restic-env.env must contain RESTIC_REPOSITORY=%s, got:\n%s", expectedRepo, content)
	}
	for _, forbidden := range []string{instAccess, instSecret, instPass} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("restic-env.env must NOT contain instance key %q, got:\n%s", forbidden, content)
		}
	}
}

func TestListShowsEnvSnapshots(t *testing.T) {
	g := newRig(t)
	g.run.OnPrefix("docker compose", sys.Result{Stdout: []byte(`[{"backup_name":"base_0001","time":"2026-01-01T00:00:00Z"}]`)})
	g.run.OnPrefix("docker run", sys.Result{Stdout: []byte(`[{"id":"abcdef0123","short_id":"abcdef01","time":"2026-01-02T00:00:00Z"}]`)})

	// List only env
	snaps, err := g.r.List(context.Background(), "app-prod", "env")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(snaps) != 1 || snaps[0].Service != "env" || snaps[0].Strategy != "env" || snaps[0].ID != "abcdef01" {
		t.Fatalf("snaps for env: %+v", snaps)
	}
}

func TestEnvDumpSuccess(t *testing.T) {
	g := newRig(t)
	g.okAll()

	entriesSeed := []secrets.Entry{
		{
			Path:  "/srv/deploy/app-prod/cvctl.env",
			Mode:  0o600,
			User:  "deployer",
			Group: "deploy",
			Data:  []byte("FOO=bar\n"),
		},
		{
			Path:  "/var/lib/cvctl/app-prod/deploy_key",
			Mode:  0o600,
			User:  "deployer",
			Group: "deploy",
			Data:  []byte("KEY\n"),
		},
	}
	tarBytes, err := secrets.Pack(entriesSeed)
	if err != nil {
		t.Fatal(err)
	}

	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 snapshots --json latest",
		sys.Result{Stdout: []byte(`[{"id":"abcdef0123456789","short_id":"abcdef01","time":"2026-01-02T00:00:00Z"}]`)})
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 dump abcdef01 /env.tar",
		sys.Result{Stdout: tarBytes})

	entries, shortID, err := g.r.EnvDump(context.Background(), "app-prod", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if shortID != "abcdef01" {
		t.Fatalf("shortID: want abcdef01, got %q", shortID)
	}
	if len(entries) != 2 || entries[0].Path != "/srv/deploy/app-prod/cvctl.env" || entries[1].Path != "/var/lib/cvctl/app-prod/deploy_key" {
		t.Fatalf("entries: %+v", entries)
	}
}

func TestEnvDumpRejectsForeignPath(t *testing.T) {
	for _, foreignPath := range []string{"/srv/deploy/other-prod/cvctl.env", "/var/lib/cvctl/other-prod/deploy_key"} {
		g := newRig(t)
		g.okAll()

		entry := secrets.Entry{
			Path:  foreignPath,
			Mode:  0o600,
			User:  "deployer",
			Group: "deploy",
			Data:  []byte("FOO=bar\n"),
		}
		tarBytes, err := secrets.Pack([]secrets.Entry{entry})
		if err != nil {
			t.Fatal(err)
		}

		g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 snapshots --json latest",
			sys.Result{Stdout: []byte(`[{"id":"abcdef0123456789","short_id":"abcdef01","time":"2026-01-02T00:00:00Z"}]`)})
		g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 dump abcdef01 /env.tar",
			sys.Result{Stdout: tarBytes})

		_, _, err = g.r.EnvDump(context.Background(), "app-prod", "")
		if err == nil || code(err) != "E_BACKUP_ENV_FOREIGN" {
			t.Fatalf("path %q: want E_BACKUP_ENV_FOREIGN, got %v", foreignPath, err)
		}
	}
}

func TestEnvDumpNoEnv(t *testing.T) {
	g := newRig(t)
	g.okAll()

	// Missing repo (exit code 10)
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 snapshots --json latest",
		sys.Result{ExitCode: 10, Stderr: []byte("Fatal: repository does not exist")})

	_, _, err := g.r.EnvDump(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_NO_ENV" {
		t.Fatalf("want E_BACKUP_NO_ENV, got %v", err)
	}

	// Empty snapshots list
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-env.env restic/restic:0.19.1 snapshots --json latest",
		sys.Result{Stdout: []byte("[]")})

	_, _, err = g.r.EnvDump(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_NO_ENV" {
		t.Fatalf("want E_BACKUP_NO_ENV for empty snapshots, got %v", err)
	}
}

type warnFS struct {
	sys.FS
	symlinks map[string]bool
}

func (w *warnFS) Lstat(p string) (sys.FileInfo, error) {
	fi, err := w.FS.Lstat(p)
	if err != nil {
		return fi, err
	}
	if w.symlinks[p] {
		fi.IsSymlink = true
	}
	return fi, nil
}

func TestEnvBackupCollectWarningFails(t *testing.T) {
	g := newRig(t)
	g.okAll()
	_ = g.fs.WriteFile("/srv/deploy/app-prod/app.env", []byte("X=1"), 0o640, sys.Owner{})
	symFS := &warnFS{FS: g.fs, symlinks: map[string]bool{"/srv/deploy/app-prod/app.env": true}}
	g.r.FS = symFS

	res, err := g.r.BackupNow(context.Background(), "app-prod", "env")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("result should not be OK: %+v", res)
	}
	if !strings.Contains(err.Error(), "not a regular file") && !strings.Contains(res[0].Error, "not a regular file") {
		t.Fatalf("expected warning text in error: %v / %s", err, res[0].Error)
	}
}

func TestEnvBackupEmptyEntriesFails(t *testing.T) {
	g := newRig(t)
	g.okAll()
	fsys := systest.NewFS()
	_ = fsys.MkdirAll("/srv/deploy/app-prod", 0o755, sys.Owner{})
	g.r.FS = fsys

	res, err := g.r.BackupNow(context.Background(), "app-prod", "env")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	if len(res) != 1 || res[0].OK {
		t.Fatalf("result should not be OK: %+v", res)
	}
	if !strings.Contains(res[0].Error, "no env files found") {
		t.Fatalf("expected 'no env files found' in error: %v / %s", err, res[0].Error)
	}
}

func TestBackupNowLoadErrorEnvStillRuns(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) {
		return nil, nil, errors.New("contract yaml parse failure")
	}
	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	called := false
	for _, c := range g.run.Calls {
		if c.Name == "docker" && slices.Contains(c.Args, "backup") && slices.Contains(c.Args, "--stdin") {
			called = true
			break
		}
	}
	if !called {
		t.Fatalf("restic backup --stdin was not called: %v", g.calls())
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(res), res)
	}
	if res[0].Service != "" || res[0].OK || !strings.Contains(res[0].Error, "contract yaml parse failure") {
		t.Fatalf("res[0] should be failed data target: %+v", res[0])
	}
	if res[1].Service != "env" || !res[1].OK {
		t.Fatalf("res[1] should be OK env target: %+v", res[1])
	}
	if !strings.Contains(err.Error(), "contract yaml parse failure") {
		t.Fatalf("final error should mention data error: %v", err)
	}
}

type psFailRunner struct {
	sys.Runner
	count int
}

func (r *psFailRunner) Run(ctx context.Context, cmd sys.Cmd) (sys.Result, error) {
	if strings.Contains(cmd.String(), "docker ps") {
		r.count++
		if r.count == 1 {
			return sys.Result{ExitCode: 1, Stderr: []byte("silo container crashed")}, nil
		}
		return sys.Result{Stdout: []byte("silo123\n")}, nil
	}
	return r.Runner.Run(ctx, cmd)
}

func TestBackupNowSiloDockerPSFailureEnvStillRuns(t *testing.T) {
	g := newRig(t)
	g.okAll()
	p, d, _ := g.r.Load("app-prod")
	d.Backup = instance.BackupDecision{
		AccessKey:      "inst-ak",
		SecretKey:      "inst-sk",
		LibsodiumKey:   "inst-lk",
		ResticPassword: "inst-rp",
	}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }
	g.r.Host.Local = true
	g.run.On("docker network connect --alias cvctl-storage app-prod_backup silo123", sys.Result{})
	g.r.Run = &psFailRunner{Runner: g.run}

	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	called := false
	for _, c := range g.run.Calls {
		if c.Name == "docker" && slices.Contains(c.Args, "backup") && slices.Contains(c.Args, "--stdin") {
			called = true
			break
		}
	}
	if !called {
		t.Fatalf("restic backup --stdin was not called: %v", g.calls())
	}
	if len(res) != 2 {
		t.Fatalf("want 2 results, got %d: %+v", len(res), res)
	}
	if res[0].Service != "" || res[0].OK {
		t.Fatalf("res[0] should be failed data target: %+v", res[0])
	}
	if res[1].Service != "env" || !res[1].OK {
		t.Fatalf("res[1] should be OK env target: %+v", res[1])
	}
}

func TestBackupNowBothDataAndEnvFailMentionsBoth(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) {
		return nil, nil, errors.New("cannot read contract")
	}
	g.run.OnPrefix("docker run -i --rm --network app-prod_backup", sys.Result{ExitCode: 1, Stderr: []byte("s3 network unreachable")})
	res, err := g.r.BackupNow(context.Background(), "app-prod", "")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	if len(res) != 2 || res[0].OK || res[1].OK {
		t.Fatalf("both should be failed: %+v", res)
	}
	msg := err.Error()
	if !strings.Contains(msg, "cannot read contract") || !strings.Contains(msg, "env") {
		t.Fatalf("error must mention both data setup error and env failure: %s", msg)
	}
}

func TestHostLocalSiloConnectBeforeResticStdinZeroDataTargets(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.r.Host.Local = true
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) {
		return &contract.Project{}, &instance.Decisions{Instance: "app-prod"}, nil
	}
	g.run.On(siloPS, sys.Result{Stdout: []byte("silo-container-99\n")})
	g.run.On("docker network connect --alias cvctl-storage app-prod_backup silo-container-99", sys.Result{})

	if _, err := g.r.BackupNow(context.Background(), "app-prod", ""); err != nil {
		t.Fatal(err)
	}

	connectIdx := -1
	stdinIdx := -1
	for i, c := range g.run.Calls {
		s := c.String()
		if strings.Contains(s, "network connect") && strings.Contains(s, "app-prod_backup") && strings.Contains(s, "silo-container-99") {
			connectIdx = i
		}
		if strings.Contains(s, "restic") && strings.Contains(s, "backup") && strings.Contains(s, "--stdin") {
			stdinIdx = i
		}
		if strings.Contains(s, "docker exec") && strings.Contains(s, "sh -s") {
			t.Fatalf("no user script should run for env path, but found: %s", s)
		}
	}
	if connectIdx == -1 {
		t.Fatal("docker network connect not found")
	}
	if stdinIdx == -1 {
		t.Fatal("restic backup --stdin not found")
	}
	if connectIdx >= stdinIdx {
		t.Fatalf("network connect (%d) must happen before restic backup --stdin (%d)", connectIdx, stdinIdx)
	}
}

func TestHostLocalSiloConnectBeforeResticStdinKeyedInstance(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.r.Host.Local = true
	p, d, _ := g.r.Load("app-prod")
	d.Backup = instance.BackupDecision{
		AccessKey:      "inst-ak-1",
		SecretKey:      "inst-sk-1",
		LibsodiumKey:   "inst-lk-1",
		ResticPassword: "inst-rp-1",
	}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }
	g.run.On(siloPS, sys.Result{Stdout: []byte("silo-container-keyed\n")})
	g.run.On("docker network connect --alias cvctl-storage app-prod_backup silo-container-keyed", sys.Result{})

	if _, err := g.r.BackupNow(context.Background(), "app-prod", "env"); err != nil {
		t.Fatal(err)
	}

	connectIdx := -1
	stdinIdx := -1
	for i, c := range g.run.Calls {
		s := c.String()
		if strings.Contains(s, "network connect") && strings.Contains(s, "app-prod_backup") && strings.Contains(s, "silo-container-keyed") {
			connectIdx = i
		}
		if strings.Contains(s, "restic") && strings.Contains(s, "backup") && strings.Contains(s, "--stdin") {
			stdinIdx = i
		}
		if strings.Contains(s, "docker exec") && strings.Contains(s, "sh -s") {
			t.Fatalf("no user script should run for env path, but found: %s", s)
		}
	}
	if connectIdx == -1 {
		t.Fatal("docker network connect not found")
	}
	if stdinIdx == -1 {
		t.Fatal("restic backup --stdin not found")
	}
	if connectIdx >= stdinIdx {
		t.Fatalf("network connect (%d) must happen before restic backup --stdin (%d)", connectIdx, stdinIdx)
	}
}

func TestResticBackupStdinIncludesInteractiveFlag(t *testing.T) {
	g := newRig(t)
	g.okAll()
	if _, err := g.r.BackupNow(context.Background(), "app-prod", "env"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range g.run.Calls {
		if c.Name == "docker" && slices.Contains(c.Args, "backup") && slices.Contains(c.Args, "--stdin") {
			if !slices.Contains(c.Args, "-i") {
				t.Fatalf("docker backup --stdin must include -i, args: %v", c.Args)
			}
			found = true
		}
	}
	if !found {
		t.Fatal("docker backup --stdin call not found")
	}
}

func TestVerifyEnvReturnsNoTarget(t *testing.T) {
	g := verifyRig(t, "")
	_, err := g.r.Verify(context.Background(), "app-prod", "env")
	if err == nil || code(err) != "E_BACKUP_NO_TARGET" {
		t.Fatalf("want E_BACKUP_NO_TARGET, got %v", err)
	}
}

func TestEnvDumpSnapshotValidation(t *testing.T) {
	g := newRig(t)
	g.okAll()

	invalidSnapshots := []string{
		"xyz",
		"12",
		"abc",
		"ABCDEF01",
		"snapshot;rm",
		"../../etc/passwd",
	}
	for _, snap := range invalidSnapshots {
		_, _, err := g.r.EnvDump(context.Background(), "app-prod", snap)
		if err == nil || code(err) != "E_USAGE_SNAPSHOT" {
			t.Fatalf("snap %q: want E_USAGE_SNAPSHOT, got %v", snap, err)
		}
	}
}

func TestBackupServiceRedactsEnvError(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix("docker run -i --rm --network app-prod_backup", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("restic s3 error containing host secret " + secretKey),
	})
	err := g.r.BackupService(context.Background(), "app-prod", "env")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if strings.Contains(err.Error(), secretKey) {
		t.Fatalf("secret leaked in BackupService error: %v", err)
	}
	if !strings.Contains(err.Error(), "***") {
		t.Fatalf("expected redacted *** in error, got: %v", err)
	}
}

func TestSecretsRootValidation(t *testing.T) {
	g := newRig(t)
	g.okAll()

	// DeployRoot not ending in /srv/deploy
	g.r.DeployRoot = "/var/deploy/instances"
	res, err := g.r.BackupNow(context.Background(), "app-prod", "env")
	if err == nil || code(err) != "E_BACKUP_FAILED" {
		t.Fatalf("want E_BACKUP_FAILED, got %v", err)
	}
	expectedMsg := "deploy root /var/deploy/instances does not end in /srv/deploy"
	if len(res) == 0 || !strings.Contains(res[0].Error, expectedMsg) {
		t.Fatalf("expected %q in res[0].Error, got %v", expectedMsg, res)
	}
	svcErr := g.r.BackupService(context.Background(), "app-prod", "env")
	if svcErr == nil || !strings.Contains(svcErr.Error(), expectedMsg) {
		t.Fatalf("expected %q in BackupService err, got %v", expectedMsg, svcErr)
	}

	// DeployRoot ending in /srv/deploy
	g.r.DeployRoot = "/tmp/testroot/srv/deploy"
	_ = g.fs.MkdirAll("/tmp/testroot/srv/deploy/app-prod", 0o755, sys.Owner{})
	_ = g.fs.WriteFile("/tmp/testroot/srv/deploy/app-prod/cvctl.env", []byte("CVCTL_INSTANCE=app-prod\n"), 0o600, sys.Owner{})
	g.run.OnPrefix("docker run", sys.Result{})
	res2, err := g.r.BackupNow(context.Background(), "app-prod", "env")
	if err != nil {
		t.Fatalf("unexpected error with valid prefixed DeployRoot: %v", err)
	}
	if len(res2) != 1 || !res2[0].OK {
		t.Fatalf("expected successful env backup, got: %+v", res2)
	}
}

func TestEnvPackExcludesRegeneratedEnvFiles(t *testing.T) {
	g := newRig(t)
	g.okAll()
	instDir := "/srv/deploy/app-prod"
	_ = g.fs.WriteFile(instDir+"/cvctl.env", []byte("CVCTL=1\n"), 0o600, sys.Owner{})
	_ = g.fs.WriteFile(instDir+"/app.env", []byte("APP=1\n"), 0o600, sys.Owner{})
	_ = g.fs.WriteFile(instDir+"/backup-db.env", []byte("BACKUP_DB=secret\n"), 0o600, sys.Owner{})
	_ = g.fs.WriteFile(instDir+"/restic-files.env", []byte("RESTIC_FILES=secret\n"), 0o600, sys.Owner{})
	_ = g.fs.WriteFile(instDir+"/restic-env.env", []byte("RESTIC_ENV=secret\n"), 0o600, sys.Owner{})
	_ = g.fs.WriteFile("/var/lib/cvctl/app-prod/deploy_key", []byte("KEY\n"), 0o600, sys.Owner{})
	_ = g.fs.WriteFile("/var/lib/cvctl/app-prod/deploy_key.pub", []byte("PUB\n"), 0o644, sys.Owner{})

	if _, err := g.r.BackupNow(context.Background(), "app-prod", "env"); err != nil {
		t.Fatal(err)
	}

	var stdinBytes []byte
	for _, c := range g.run.Calls {
		if c.Name == "docker" && slices.Contains(c.Args, "backup") && slices.Contains(c.Args, "--stdin") {
			stdinBytes = c.Stdin
			break
		}
	}
	if len(stdinBytes) == 0 {
		t.Fatal("restic backup --stdin was not called or stdin was empty")
	}

	entries, err := secrets.Unpack(stdinBytes)
	if err != nil {
		t.Fatalf("failed to unpack stdin tar: %v", err)
	}
	paths := map[string]bool{}
	for _, e := range entries {
		paths[filepath.Base(e.Path)] = true
	}

	for _, expected := range []string{"cvctl.env", "app.env", "deploy_key", "deploy_key.pub"} {
		if !paths[expected] {
			t.Fatalf("expected file %q in env.tar, but missing. Paths: %v", expected, paths)
		}
	}
	for _, forbidden := range []string{"backup-db.env", "restic-files.env", "restic-env.env"} {
		if paths[forbidden] {
			t.Fatalf("regenerated file %q should NOT be in env.tar", forbidden)
		}
	}
}
