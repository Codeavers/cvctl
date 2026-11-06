package backup_test

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

const (
	sqliteImage = "ghcr.io/codeavers/cvctl-sqlite:3"
	sqliteVol   = "app-prod_db-data"
	hardening   = "--network none --cap-drop ALL --security-opt no-new-privileges:true"
	sqliteProbe = "docker run --rm " + hardening + " --user 0 -v " + sqliteVol + ":/data:ro " + sqliteImage + " sh -c "
	apiRef      = "ghcr.io/acme/api:1@sha256:abc"
	inspectUser = "docker image inspect --format {{.Config.User}} " + apiRef
)

type sqliteRig struct {
	*dumpRestoreRig
	p *contract.Project
	d *instance.Decisions
}

// The app owns uid 1000 unless a test says otherwise: the label would hide the
// lookup in the image.
func newSqliteRig(t *testing.T, probeOut string) *sqliteRig {
	t.Helper()
	g := newDumpRestoreRig(t)
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	g.r.Catalog = cat
	p := &contract.Project{
		Apps: []contract.AppService{{Name: "api", Role: "app", Image: "ghcr.io/acme/api:1"}},
		Data: []contract.DataService{{Name: "db", Kind: "sqlite", Owner: "api", UID: "1000", Backup: "dump"}},
	}
	d := &instance.Decisions{Instance: "app-prod", Env: "prod", Services: map[string]instance.ServiceDecision{"db": {Mode: "local", Version: "3"}},
		Digests: map[string]string{"api": "sha256:abc"}}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }
	g.run.OnPrefix(sqliteProbe, sys.Result{Stdout: []byte(probeOut)})
	g.run.On("docker volume inspect "+sqliteVol, sys.Result{})
	g.pipe.Result = sys.PipeResult{To: sys.Result{Stdout: resticSummary}}
	return &sqliteRig{dumpRestoreRig: g, p: p, d: d}
}

// noHistory is a repository restic has not created yet.
func (g *sqliteRig) noHistory() {
	g.run.OnPrefix(listCmd, sys.Result{ExitCode: 10, Stderr: []byte("Fatal: repository does not exist")})
}

func (g *sqliteRig) fromArgs() string { return strings.Join(g.pipe.Calls[0].From.Args, " ") }

func TestSqliteHelperBackup(t *testing.T) {
	g := newSqliteRig(t, "1000 ro\n")
	res, err := g.backup(t)
	if err != nil || len(res) != 1 || !res[0].OK || res[0].Note != "" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(g.pipe.Calls) != 1 {
		t.Fatalf("pipe: %+v", g.pipe.Calls)
	}
	from := g.fromArgs()
	for _, want := range []string{
		"run --rm --name cvctl-sqlite-app-prod-db-", hardening + " --user 1000",
		"-v " + sqliteVol + ":/data:ro", sqliteImage + " sh -c",
		".backup /tmp/", "cat /tmp/",
	} {
		if !strings.Contains(from, want) {
			t.Errorf("helper lacks %q: %s", want, from)
		}
	}
	if strings.Contains(from, "--cap-add") {
		t.Errorf("reading a database needs no capability: %s", from)
	}
	if to := strings.Join(g.pipe.Calls[0].To.Args, " "); !strings.Contains(to, "backup --stdin --stdin-filename db.dump") {
		t.Fatalf("restic: %s", to)
	}
	if g.index(tagCmdAt(dumpNow)) < 0 {
		t.Fatalf("snapshot not marked complete: %v", g.calls())
	}
	for _, c := range g.calls() {
		if strings.Contains(c, "ps -a --format json") {
			t.Fatalf("there is no service container to look for: %s", c)
		}
	}
}

// Without a read transaction open, .backup starts over whenever the app commits
// between two steps, and a busy app keeps it from ever finishing.
func TestSqliteBackupCopiesOneSnapshot(t *testing.T) {
	g := newSqliteRig(t, "1000 ro\n")
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	from := g.fromArgs()
	begin, backupAt := strings.Index(from, `"BEGIN"`), strings.Index(from, ".backup")
	if begin < 0 || backupAt < begin || !strings.Contains(from, "sqlite_master") {
		t.Fatalf("no read transaction around .backup: %s", from)
	}
	if !strings.Contains(from[backupAt:], ">/dev/null") {
		t.Fatalf("the row that opens the transaction would land in the dump: %s", from)
	}
}

func TestSqliteBackupMountsReadWriteWhenSqliteMustWrite(t *testing.T) {
	g := newSqliteRig(t, "1000 rw\n")
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	from := g.fromArgs()
	if !strings.Contains(from, "-v "+sqliteVol+":/data ") || !strings.Contains(from, "--user 1000") {
		t.Fatalf("a WAL database needs its -shm file: %s", from)
	}
}

func TestSqliteBackupRunsAsTheFileOwner(t *testing.T) {
	g := newSqliteRig(t, "1234 ro\n")
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	if from := g.fromArgs(); !strings.Contains(from, "--user 1234") {
		t.Fatalf("with no capabilities only the owner reads a 0600 file: %s", from)
	}
}

func TestSqliteBackupWithoutDatabaseIsANoop(t *testing.T) {
	for name, setup := range map[string]func(*sqliteRig){
		"no file":   func(*sqliteRig) {},
		"no volume": func(g *sqliteRig) { g.run.On("docker volume inspect "+sqliteVol, sys.Result{ExitCode: 1}) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newSqliteRig(t, "absent\n")
			g.noHistory()
			setup(g)
			res, err := g.backup(t)
			if err != nil || len(res) != 1 || !res[0].OK {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if !strings.Contains(res[0].Note, "no database") {
				t.Fatalf("the no-op must say so: %+v", res[0])
			}
			if len(g.pipe.Calls) != 0 {
				t.Fatalf("an empty stream would become a complete snapshot: %+v", g.pipe.Calls)
			}
			for _, c := range g.calls() {
				if strings.Contains(c, "restic/restic") && !strings.HasPrefix(c, listCmd) {
					t.Fatalf("restic stored something: %s", c)
				}
			}
		})
	}
}

// Snapshots prove there was a database, so its absence is a loss, not a new app.
func TestSqliteLostDatabaseFailsTheBackup(t *testing.T) {
	for name, setup := range map[string]func(*sqliteRig){
		"file gone":   func(*sqliteRig) {},
		"volume gone": func(g *sqliteRig) { g.run.On("docker volume inspect "+sqliteVol, sys.Result{ExitCode: 1}) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newSqliteRig(t, "absent\n")
			setup(g)
			res, err := g.backup(t)
			if err == nil || len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "E_SQLITE_LOST") {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			st, err := g.store.Load("app-prod")
			if err != nil || !st.Backups["db"].LastOK.IsZero() {
				t.Fatalf("LastOK advanced: %+v %v", st.Backups, err)
			}
			if len(g.note.msgs) != 1 || g.note.msgs[0].Level != "error" {
				t.Fatalf("notify: %+v", g.note.msgs)
			}
		})
	}
}

func TestSqliteLostDatabaseCountsOlderRepositories(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	g.r.Host.KeyGen = map[string]int{"db": 1}
	g.r.Host.HostResticPassword = "host-pass-SECRET-777"
	calls := 0
	g.r.Run = countingList{Runner: g.run, onList: func() sys.Result {
		calls++
		if calls == 1 {
			return sys.Result{ExitCode: 10}
		}
		return sys.Result{Stdout: dumpHistory}
	}}
	res, err := g.backup(t)
	if err == nil || res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSqliteLostDatabaseUnknownHistoryFails(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	g.run.OnPrefix(listCmd, sys.Result{ExitCode: 1, Stderr: []byte("connection refused")})
	res, err := g.backup(t)
	if err == nil || res[0].OK {
		t.Fatalf("not knowing whether it was lost must not read as new: %+v %v", res, err)
	}
}

func TestSqliteRestorePointOfLostDatabaseFails(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	err := rp.Create(context.Background(), "app-prod", "deploy-5")
	if err == nil || errors.Is(err, backup.ErrFreshInstance) || !strings.Contains(err.Error(), "E_SQLITE_LOST") {
		t.Fatalf("err: %v", err)
	}
}

func TestDumpListWithoutRepositoryIsEmpty(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	g.noHistory()
	snaps, err := g.r.List(context.Background(), "app-prod", "db")
	if err != nil || snaps == nil || len(snaps) != 0 {
		t.Fatalf("snaps=%+v err=%v", snaps, err)
	}
}

func TestVolumeListWithoutRepositoryStaysAnError(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 snapshots", sys.Result{ExitCode: 10})
	if _, err := g.r.List(context.Background(), "app-prod", "files"); err == nil {
		t.Fatal("only a dump service is known to start without a repository")
	}
}

func TestSqliteBackupNeverCreatesTheVolume(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	g.noHistory()
	g.run.On("docker volume inspect "+sqliteVol, sys.Result{ExitCode: 1})
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	for _, c := range g.calls() {
		if strings.Contains(c, sqliteVol+":/data") {
			t.Fatalf("docker run -v would create the volume: %s", c)
		}
	}
}

func TestSqliteBackupServiceNoopIsNotAnError(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	g.noHistory()
	if err := g.r.BackupService(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
}

func TestSqliteBackupFailureForgetsSnapshot(t *testing.T) {
	g := newSqliteRig(t, "1000 ro\n")
	g.pipe.Result.From.ExitCode = 1
	if _, err := g.backup(t); err == nil {
		t.Fatal("a failed .backup must fail the backup")
	}
	if g.index(tagCmdAt(dumpNow)) >= 0 {
		t.Fatalf("marked complete: %v", g.calls())
	}
}

func TestSqliteBackupUnreadableProbeFails(t *testing.T) {
	for name, res := range map[string]sys.Result{
		"exit":    {ExitCode: 1, Stderr: []byte("ls: can't open '.': Permission denied")},
		"garbage": {Stdout: []byte("what\n")},
		"bad uid": {Stdout: []byte("x ro\n")},
	} {
		t.Run(name, func(t *testing.T) {
			g := newSqliteRig(t, "")
			g.run.OnPrefix(sqliteProbe, res)
			if _, err := g.backup(t); err == nil {
				t.Fatal("not knowing must not read as no database")
			}
			if len(g.pipe.Calls) != 0 {
				t.Fatalf("pipe: %+v", g.pipe.Calls)
			}
		})
	}
}

func TestSqliteBackupCancelRemovesHelper(t *testing.T) {
	g := newSqliteRig(t, "1000 ro\n")
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = pipeFunc(func(c context.Context, from, to sys.Cmd) (sys.PipeResult, error) {
		cancel()
		return g.pipe.Pipe(c, from, to)
	})
	_, _ = g.r.BackupNow(ctx, "app-prod", "db")
	if g.index("docker rm -f cvctl-sqlite-app-prod-db-") < 0 {
		t.Fatalf("killing the docker client leaves the helper running: %v", g.calls())
	}
}

func TestSqliteRestorePointNeedsNoContainer(t *testing.T) {
	g := newSqliteRig(t, "1000 ro\n")
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 || !strings.Contains(strings.Join(g.pipe.Calls[0].To.Args, " "), "--tag deploy-5") {
		t.Fatalf("pipe: %+v", g.pipe.Calls)
	}
}

func TestSqliteRestorePointWithoutDatabaseIsFresh(t *testing.T) {
	g := newSqliteRig(t, "absent\n")
	g.noHistory()
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); !errors.Is(err, backup.ErrFreshInstance) {
		t.Fatalf("err: %v", err)
	}
}

func sqliteVerifyRig(t *testing.T, integrity sys.Result) (*sqliteRig, string) {
	t.Helper()
	g := newSqliteRig(t, "1000 ro\n")
	g.pipe.Result = sys.PipeResult{}
	name := "cvctl-verify-app-prod-db-" + strconv.FormatInt(dumpNow.Unix(), 10) + "-0a0b0c0d"
	g.run.OnPrefix("docker run --rm "+hardening+" --user 0 -v "+name+":/data "+sqliteImage+" sh -c sqlite3", integrity)
	return g, name
}

func TestSqliteVerifyIntegrityCheck(t *testing.T) {
	g, name := sqliteVerifyRig(t, sys.Result{Stdout: []byte("ok\n")})
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	last := -1
	for _, want := range []string{
		"docker volume create " + name, "PIPE",
		"docker run --rm " + hardening + " --user 0 -v " + name + ":/data " + sqliteImage + " sh -c sqlite3 -batch /data/app.db 'PRAGMA integrity_check'",
		"docker volume rm -f " + name,
	} {
		i := g.index(want)
		if i <= last {
			t.Fatalf("%q out of order (%d <= %d): %v", want, i, last, g.calls())
		}
		last = i
	}
	if len(g.pipe.Calls) != 1 || !strings.Contains(g.fromArgs(), " dump dddd4444 db.dump") {
		t.Fatalf("pipe: %+v", g.pipe.Calls)
	}
	to := strings.Join(g.pipe.Calls[0].To.Args, " ")
	for _, want := range []string{"run --rm -i --name", hardening, "-v " + name + ":/data", "/data/app.db"} {
		if !strings.Contains(to, want) {
			t.Errorf("loader lacks %q: %s", want, to)
		}
	}
	if slices.ContainsFunc(g.calls(), func(c string) bool {
		return strings.Contains(c, sqliteVol+":/data") && strings.Contains(c, "integrity")
	}) {
		t.Fatalf("the check ran on the live volume: %v", g.calls())
	}
}

func TestSqliteVerifyRejectsAnythingButOk(t *testing.T) {
	for name, res := range map[string]sys.Result{
		"corrupt":   {Stdout: []byte("*** in database main ***\nPage 3: btreeInitPage() returns error code 11\n")},
		"ok after":  {Stdout: []byte("row 1 missing from index\nok\n")},
		"no output": {},
		"failed":    {ExitCode: 26, Stderr: []byte("Error: file is not a database")},
	} {
		t.Run(name, func(t *testing.T) {
			g, vol := sqliteVerifyRig(t, res)
			results, err := g.r.Verify(context.Background(), "app-prod", "db")
			var oe *output.Error
			if !errors.As(err, &oe) || oe.Code != "E_VERIFY_FAILED" || len(results) != 1 || results[0].OK {
				t.Fatalf("res=%+v err=%v", results, err)
			}
			if g.index("docker volume rm -f "+vol) < 0 {
				t.Fatalf("temporary volume left behind: %v", g.calls())
			}
		})
	}
}

func TestSqliteLoadRefusesAnEmptyStream(t *testing.T) {
	g, _ := sqliteVerifyRig(t, sys.Result{Stdout: []byte("ok\n")})
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if to := strings.Join(g.pipe.Calls[0].To.Args, " "); !strings.Contains(to, "[ -s ") {
		t.Fatalf("an empty file passes integrity_check, so the load must refuse it: %s", to)
	}
}

var (
	sqliteRestoreCheck = "docker run --rm " + hardening + " --user 0 -v " + dumpNewVol + ":/data " + sqliteImage + " sh -c sqlite3"
	sqliteNewProbe     = "docker run --rm " + hardening + " --user 0 -v " + dumpNewVol + ":/data:ro " + sqliteImage + " sh -c stat"
	sqliteChown        = "docker run --rm --cap-add CHOWN -e OWNER_UID=1000 " + hardening + " --user 0 -v " + dumpNewVol + ":/data " + sqliteImage + " sh -c "
)

func newSqliteRestoreRig(t *testing.T) *sqliteRig {
	t.Helper()
	g := newSqliteRig(t, "1000 ro\n")
	g.pipe.Result = sys.PipeResult{}
	g.run.OnPrefix(sqliteRestoreCheck, sys.Result{Stdout: []byte("ok\n")})
	g.run.OnPrefix(sqliteNewProbe, sys.Result{Stdout: []byte("0\n")})
	return g
}

func TestSqliteRestoreStopsApp(t *testing.T) {
	g := newSqliteRestoreRig(t)
	g.p.Apps = []contract.AppService{{Name: "api", Role: "app", Image: "ghcr.io/acme/api:1"}}
	res, err := g.restore("latest")
	if err != nil {
		t.Fatal(err)
	}
	if res.Volume != dumpNewVol || res.PreviousVolume != sqliteVol || res.Snapshot != "dddd4444" {
		t.Fatalf("res: %+v", res)
	}
	last := -1
	for _, want := range []string{
		cp + "stop api", "docker volume create " + dumpNewVol, "PIPE", sqliteRestoreCheck, sqliteChown, "APPLY",
	} {
		i := g.index(want)
		if i <= last {
			t.Fatalf("%q out of order (%d <= %d): %v", want, i, last, g.calls())
		}
		last = i
	}
	raw, _ := g.fs.ReadFile(envPath)
	if !strings.Contains(string(raw), "CVCTL_SVC_DB_VOLUME="+dumpNewVol) {
		t.Fatalf("not switched: %s", raw)
	}
	for _, c := range g.calls() {
		if strings.Contains(c, " stop db") || strings.Contains(c, "--wait") {
			t.Fatalf("db has no compose service: %s", c)
		}
	}
}

func TestSqliteRestoreFailureBeforeSwapRestartsApp(t *testing.T) {
	for name, breakIt := range map[string]func(*sqliteRig){
		"stream":    func(g *sqliteRig) { g.pipe.Result.To.ExitCode = 1 },
		"integrity": func(g *sqliteRig) { g.run.OnPrefix(sqliteRestoreCheck, sys.Result{Stdout: []byte("broken\n")}) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newSqliteRestoreRig(t)
			breakIt(g)
			res, err := g.restore("latest")
			if err == nil || res != nil {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if g.applies != 1 || g.index("APPLY") < g.index(cp+"stop api") {
				t.Fatalf("the app must come back: %v", g.calls())
			}
			raw, _ := g.fs.ReadFile(envPath)
			if string(raw) != dumpEnvBefore {
				t.Fatalf("cvctl.env changed: %s", raw)
			}
			if !strings.Contains(err.Error(), "back on volume "+sqliteVol) || strings.Contains(err.Error(), "kept for inspection") {
				t.Fatalf("err: %v", err)
			}
			if g.index("docker volume rm -f "+dumpNewVol) < 0 {
				t.Fatalf("half-written volume left behind: %v", g.calls())
			}
		})
	}
}

func TestSqliteRestoreChownFailureKeepsLoadedVolume(t *testing.T) {
	g := newSqliteRestoreRig(t)
	g.run.OnPrefix(sqliteChown, sys.Result{ExitCode: 1})
	if _, err := g.restore("latest"); err == nil {
		t.Fatal("want error")
	}
	if g.applies != 1 {
		t.Fatalf("applies: %d", g.applies)
	}
	raw, _ := g.fs.ReadFile(envPath)
	if string(raw) != dumpEnvBefore {
		t.Fatalf("cvctl.env changed: %s", raw)
	}
}

func TestSqliteRestoreRollsBackAfterSwapFailure(t *testing.T) {
	g := newSqliteRestoreRig(t)
	g.failAt = 1
	if _, err := g.restore("latest"); err == nil {
		t.Fatal("want error")
	}
	if g.applies != 2 {
		t.Fatalf("applies: %d", g.applies)
	}
	raw, _ := g.fs.ReadFile(envPath)
	if strings.Contains(string(raw), dumpNewVol) {
		t.Fatalf("still on the new volume: %s", raw)
	}
}

func TestSqliteRestoreToForms(t *testing.T) {
	for to, snap := range map[string]string{
		"latest":               "dddd4444",
		"cccc3333":             "cccc3333",
		"deploy-5":             "cccc3333",
		"2026-11-03T12:00:00Z": "cccc3333",
	} {
		t.Run(to, func(t *testing.T) {
			g := newSqliteRestoreRig(t)
			res, err := g.restore(to)
			if err != nil {
				t.Fatal(err)
			}
			if res.Snapshot != snap || !strings.Contains(g.fromArgs(), " dump "+snap+" db.dump") {
				t.Fatalf("res=%+v pipe=%+v", res, g.pipe.Calls)
			}
		})
	}
}

func TestSqliteRestoreToTimestampWarns(t *testing.T) {
	g := newSqliteRestoreRig(t)
	res, err := g.restore("2026-11-03T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "no point-in-time recovery") {
		t.Fatalf("warnings: %+v", res.Warnings)
	}
}

func TestSqliteRestoreBadTargetLeavesAppRunning(t *testing.T) {
	for _, to := range []string{"nonsense", "abcdef99", "2020-01-01T00:00:00Z"} {
		t.Run(to, func(t *testing.T) {
			g := newSqliteRestoreRig(t)
			if _, err := g.restore(to); err == nil {
				t.Fatal("want error")
			}
			if g.index(cp+"stop") >= 0 {
				t.Fatalf("stopped the app for a target that cannot be restored: %v", g.calls())
			}
		})
	}
}

func TestSqliteRestoreUnknownUIDLeavesAppRunning(t *testing.T) {
	g := newSqliteRestoreRig(t)
	g.p.Data[0].UID = ""
	g.d.Digests = nil
	if err := g.store.Save(&state.State{Instance: "app-prod", Status: state.StatusOK, Current: &state.Desired{InfraCommit: "abc", Digests: map[string]string{"api": "sha256:abc"}}, UpdatedAt: dumpNow}); err != nil {
		t.Fatal(err)
	}
	g.run.On(inspectUser, sys.Result{Stdout: []byte("node\n")})
	_, err := g.restore("latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != backup.CodeSqliteUID || !strings.Contains(oe.Hint, "cvctl.uid") {
		t.Fatalf("err: %v", err)
	}
	if g.index(cp+"stop") >= 0 {
		t.Fatalf("stopped the app: %v", g.calls())
	}
}

func TestSqliteRestoreFreshLeavesOwnershipToTheDeploy(t *testing.T) {
	g := newSqliteRestoreRig(t)
	g.run.On("docker volume inspect fresh-vol", sys.Result{ExitCode: 1})
	g.run.OnPrefix("docker run --rm "+hardening+" --user 0 -v fresh-vol:/data "+sqliteImage+" sh -c sqlite3", sys.Result{Stdout: []byte("ok\n")})
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", g.p, g.d, "fresh-vol"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 {
		t.Fatalf("pipe: %+v", g.pipe.Calls)
	}
	for _, c := range g.calls() {
		if strings.Contains(c, "image inspect") || strings.Contains(c, "--cap-add") {
			t.Fatalf("a rebuilt host has not pulled the app image yet: %s", c)
		}
	}
}

func TestPrepareSqlite(t *testing.T) {
	stat := func(vol string) string {
		return "docker run --rm " + hardening + " --user 0 -v " + vol + ":/data:ro " + sqliteImage + " sh -c stat"
	}
	chown := func(uid, vol string) string {
		return "docker run --rm --cap-add CHOWN -e OWNER_UID=" + uid + " " + hardening + " --user 0 -v " + vol + ":/data " + sqliteImage + " sh -c "
	}
	for _, tc := range []struct {
		name, label, imageUser, owner, wantChown, wantErr string
	}{
		{name: "label", label: "1000", owner: "0", wantChown: "1000"},
		{name: "image number", imageUser: "1001", owner: "0", wantChown: "1001"},
		{name: "image uid:gid", imageUser: "1002:1002", owner: "0", wantChown: "1002"},
		{name: "already owned", label: "1000", owner: "1000"},
		{name: "owner changed", label: "1000", owner: "999", wantChown: "1000"},
		{name: "image empty", imageUser: "", wantErr: "cvctl.uid"},
		{name: "image name", imageUser: "node", wantErr: "cvctl.uid"},
		{name: "image root", imageUser: "0", wantErr: "root"},
		{name: "image root group", imageUser: "0:0", wantErr: "root"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newSqliteRig(t, "")
			g.p.Data[0].UID = tc.label
			g.run.On(inspectUser, sys.Result{Stdout: []byte(tc.imageUser + "\n")})
			g.run.OnPrefix(stat(sqliteVol), sys.Result{Stdout: []byte(tc.owner + "\n")})
			_, err := g.r.PrepareSqlite(context.Background(), g.p, g.d)
			if tc.wantErr != "" {
				var oe *output.Error
				if !errors.As(err, &oe) || oe.Code != backup.CodeSqliteUID || !strings.Contains(oe.Message+oe.Hint, tc.wantErr) {
					t.Fatalf("err: %v", err)
				}
				if g.index("docker run") >= 0 {
					t.Fatalf("no helper without a uid: %v", g.calls())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if (tc.label != "") == (g.index("docker image inspect") >= 0) {
				t.Fatalf("the label replaces the image lookup: %v", g.calls())
			}
			got := g.index("docker run --rm --cap-add CHOWN")
			if tc.wantChown == "" {
				if got >= 0 {
					t.Fatalf("no capability when nothing changes: %v", g.calls())
				}
				return
			}
			if g.index(chown(tc.wantChown, sqliteVol)) < 0 {
				t.Fatalf("chown: %v", g.calls())
			}
		})
	}
}

func TestPrepareSqliteChownsLastTheRootOfTheVolume(t *testing.T) {
	g := newSqliteRig(t, "")
	g.run.OnPrefix("docker run --rm "+hardening+" --user 0 -v "+sqliteVol+":/data:ro "+sqliteImage+" sh -c stat", sys.Result{Stdout: []byte("0\n")})
	if _, err := g.r.PrepareSqlite(context.Background(), g.p, g.d); err != nil {
		t.Fatal(err)
	}
	var script string
	for _, c := range g.calls() {
		if strings.Contains(c, "--cap-add CHOWN") {
			script = c
		}
	}
	children := strings.Index(script, "find /data -mindepth 1 -depth")
	root := strings.Index(script, `chown -h "$OWNER_UID" /data`)
	if children < 0 || root < children || !strings.Contains(script, "set -e") {
		t.Fatalf("the root marks the volume as done, so it goes alone and last: %s", script)
	}
}

func TestPrepareSqliteSkipsOtherKinds(t *testing.T) {
	g := newSqliteRig(t, "")
	g.p.Data[0].Kind = "postgres"
	if _, err := g.r.PrepareSqlite(context.Background(), g.p, g.d); err != nil {
		t.Fatal(err)
	}
	if len(g.calls()) != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestPrepareSqliteUndoGivesBackThePreviousOwner(t *testing.T) {
	g := newSqliteRig(t, "")
	owner := "999"
	g.r.Run = spyRunner{Runner: g.run, before: func(_ context.Context, c sys.Cmd) (sys.Result, bool) {
		s := c.String()
		if strings.Contains(s, "sh -c stat") {
			return sys.Result{Stdout: []byte(owner + "\n")}, true
		}
		if strings.Contains(s, "OWNER_UID=1000") {
			owner = "1000"
		}
		return sys.Result{}, false
	}}
	undo, err := g.r.PrepareSqlite(context.Background(), g.p, g.d)
	if err != nil {
		t.Fatal(err)
	}
	if err := undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	want, then := "--cap-add CHOWN -e OWNER_UID=1000", "--cap-add CHOWN -e OWNER_UID=999"
	first, second := -1, -1
	for i, c := range g.calls() {
		if strings.Contains(c, want) {
			first = i
		}
		if strings.Contains(c, then) {
			second = i
		}
	}
	if first < 0 || second < first {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestPrepareSqliteUndoTouchesNothingUnchanged(t *testing.T) {
	g := newSqliteRig(t, "")
	g.run.OnPrefix("docker run --rm "+hardening+" --user 0 -v "+sqliteVol+":/data:ro "+sqliteImage+" sh -c stat", sys.Result{Stdout: []byte("1000\n")})
	undo, err := g.r.PrepareSqlite(context.Background(), g.p, g.d)
	if err != nil {
		t.Fatal(err)
	}
	if err := undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.index("docker run --rm --cap-add CHOWN") >= 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

// countingList answers restic listings one by one, for repositories that differ
// only in an env file the fake runner cannot see.
type countingList struct {
	*systest.Runner
	onList func() sys.Result
}

func (c countingList) Run(ctx context.Context, cmd sys.Cmd) (sys.Result, error) {
	if strings.HasSuffix(cmd.String(), "snapshots --json") {
		return c.onList(), nil
	}
	return c.Runner.Run(ctx, cmd)
}

// A chown that fails partway leaves the children on the new user and the root
// on the old one, so the root says nothing about what the undo has to do.
func TestPrepareSqliteUndoAfterPartialChown(t *testing.T) {
	g := newSqliteRig(t, "")
	g.r.Run = spyRunner{Runner: g.run, before: func(_ context.Context, c sys.Cmd) (sys.Result, bool) {
		s := c.String()
		if strings.Contains(s, "sh -c stat") {
			return sys.Result{Stdout: []byte("999\n")}, true
		}
		if strings.Contains(s, "OWNER_UID=1000") {
			return sys.Result{ExitCode: 1, Stderr: []byte("find: /data/private: Permission denied")}, true
		}
		return sys.Result{}, false
	}}
	undo, err := g.r.PrepareSqlite(context.Background(), g.p, g.d)
	if err == nil {
		t.Fatal("want error")
	}
	if err := undo(context.Background()); err != nil {
		t.Fatal(err)
	}
	if g.index("docker run --rm --cap-add CHOWN -e OWNER_UID=999 ") < 0 {
		t.Fatalf("the undo skipped the volume: %v", g.calls())
	}
}
