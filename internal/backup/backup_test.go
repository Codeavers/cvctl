package backup_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/render"
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
	run.On("docker network inspect app-prod_backup", sys.Result{})
	fsys := systest.NewFS()
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
		"docker network inspect app-prod_backup",
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
	if err != nil || len(snaps) != 2 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}
	if snaps[0].ID != "base_0001" || snaps[0].Strategy != "wal-g" || snaps[1].ID != "abcdef01" || snaps[1].Strategy != "volume" {
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
	if len(res) != 2 || !res[0].OK || !res[1].OK {
		t.Fatalf("all targets must still run: %+v", res)
	}
	if len(g.run.Calls) < 3 {
		t.Fatalf("calls: %v", g.calls())
	}
}
