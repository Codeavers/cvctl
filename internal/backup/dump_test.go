package backup_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

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
	enginePass  = "engine-pass-SECRET-321"
	dumpScript  = `FAKE_PWD="$FAKE_ROOT_PASSWORD" fakedump --all-databases`
	snapID      = "4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e"
	composePS   = "docker compose -p app-prod --env-file /srv/deploy/app-prod/app.env -f /srv/deploy/app-prod/compose.cvctl.yml ps -a --format json db"
	retention   = "forget --tag complete --keep-within 7d --keep-daily 7 --keep-weekly 4 --prune"
	unmark      = "tag --json --add complete --remove partial " + snapID
	resticImage = "restic/restic:0.19.1"
	purge       = "forget --tag partial --keep-within 2d --prune"
	listCmd     = "docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 snapshots --json"
)

// Newest last, as restic prints them.
var snapshotsWithPartial = []byte(`[
{"short_id":"aaaa1111","time":"2026-11-03T03:00:00Z","tags":["app-prod","complete"]},
{"short_id":"bbbb2222","time":"2026-11-04T03:00:00Z","tags":["app-prod","partial"]}]`)

var dumpContainer = fmt.Sprintf("cvctl-dump-app-prod-db-%d-0a0b0c0d", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix())

// dumpEnvAt is the env file of a dump started at now; the rig's fixed suffix
// makes it predictable.
func dumpEnvAt(now time.Time) string {
	return fmt.Sprintf("/srv/deploy/app-prod/cvctl-restic-app-prod-db-%d-0a0b0c0d.env", now.Unix())
}

func tagCmdAt(now time.Time) string {
	return "docker run --rm --network app-prod_backup --env-file " + dumpEnvAt(now) + " restic/restic:0.19.1 --retry-lock 10m tag"
}

var (
	dumpEnv = dumpEnvAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	tagCmd  = tagCmdAt(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
)

var resticSummary = []byte(`{"message_type":"status","percent_done":1}
{"message_type":"summary","files_new":1,"snapshot_id":"` + snapID + `"}
`)

func tagSummary(changed int) []byte {
	return fmt.Appendf(nil, `{"message_type":"changed","old_snapshot_id":"a","new_snapshot_id":"b"}
{"message_type":"summary","changed_snapshots":%d}
`, changed)
}

type dumpRig struct {
	*rig
	pipe *systest.Piper
}

func newDumpRig(t *testing.T) *dumpRig {
	t.Helper()
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix(composePS, sys.Result{Stdout: []byte(`{"ID":"c0ffee","Service":"db","State":"running"}`)})
	g.run.OnPrefix(tagCmd, sys.Result{Stdout: tagSummary(1)})
	pipe := &systest.Piper{Result: sys.PipeResult{To: sys.Result{Stdout: resticSummary}}}
	g.r.Pipe = pipe
	g.r.Catalog = kinds.Catalog{"fakedb": {
		Name: "fakedb", Backup: "dump", Image: "fake/db:{version}", Versions: []string{"1"},
		VolumePath: "/var/lib/fake", Command: []string{"fakedbd", "--user", "{user}"},
		ContainerEnv: map[string]string{"FAKE_ROOT_PASSWORD": "{password}"}, CapAdd: []string{"CHOWN", "SETUID"},
		Health:      []string{"CMD-SHELL", "fakeping"},
		DumpCommand: []string{dumpScript}, RestoreCommand: []string{"fakerestore", "--all"}, RestoreCheck: "fakecount",
	}}
	p := &contract.Project{Data: []contract.DataService{{Name: "db", Kind: "fakedb", Backup: "dump"}}}
	d := &instance.Decisions{Instance: "app-prod", Services: map[string]instance.ServiceDecision{"db": {
		Mode: "local", Version: "1", Creds: kinds.Credentials{User: "root", Password: enginePass, DB: "app"},
	}}}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }
	return &dumpRig{rig: g, pipe: pipe}
}

func (g *dumpRig) backup(t *testing.T) ([]backup.Result, error) {
	t.Helper()
	return g.r.BackupNow(context.Background(), "app-prod", "db")
}

func (g *dumpRig) resticCalls(suffix string) []string {
	var out []string
	for _, c := range g.calls() {
		if strings.HasSuffix(c, suffix) {
			out = append(out, c)
		}
	}
	return out
}

func TestTargetsIncludeDump(t *testing.T) {
	g := newDumpRig(t)
	p, d, _ := g.r.Load("app-prod")
	ts := backup.Targets(p, d)
	if len(ts) != 1 || ts[0].Service != "db" || ts[0].Strategy != backup.StrategyDump {
		t.Fatalf("targets: %+v", ts)
	}
}

func TestDumpPipesServiceIntoRestic(t *testing.T) {
	g := newDumpRig(t)
	res, err := g.backup(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].OK || res[0].Strategy != "dump" {
		t.Fatalf("results: %+v", res)
	}
	if len(g.pipe.Calls) != 1 {
		t.Fatalf("pipe calls: %+v", g.pipe.Calls)
	}
	from, to := g.pipe.Calls[0].From, g.pipe.Calls[0].To
	if !slices.Equal(from.Args, []string{"exec", "c0ffee", "sh", "-c", dumpScript}) || from.Name != "docker" {
		t.Fatalf("producer: %v", from)
	}
	want := "run -i --rm --name " + dumpContainer + " --network app-prod_backup --env-file " + dumpEnv + " restic/restic:0.19.1 --retry-lock 10m backup --stdin --stdin-filename db.dump --host h1 --tag app-prod --tag partial --json --quiet"
	if got := strings.Join(to.Args, " "); got != want {
		t.Fatalf("consumer:\n got %s\nwant %s", got, want)
	}
	if to.Stdin != nil {
		t.Fatal("the consumer reads the pipe, not a buffer")
	}
	if len(g.resticCalls(retention)) != 1 {
		t.Fatalf("retention missing: %v", g.calls())
	}
	if len(g.resticCalls("forget "+snapID+" --prune")) != 0 {
		t.Fatalf("a good snapshot was forgotten: %v", g.calls())
	}
}

func TestDumpJoinsCommandIntoOneScript(t *testing.T) {
	g := newDumpRig(t)
	k := g.r.Catalog["fakedb"]
	k.DumpCommand = []string{`PWD_VAR="$X"`, "fakedump", "--all"}
	g.r.Catalog["fakedb"] = k
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	if args := g.pipe.Calls[0].From.Args; args[len(args)-1] != `PWD_VAR="$X" fakedump --all` {
		t.Fatalf("args: %q", args)
	}
}

func TestBackupServiceRunsDump(t *testing.T) {
	g := newDumpRig(t)
	if err := g.r.BackupService(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 {
		t.Fatalf("pipe calls: %+v", g.pipe.Calls)
	}
}

func TestDumpFailureFailsBackupAndForgetsSnapshot(t *testing.T) {
	for name, exit := range map[string]int{"exit 1": 1, "signal": -1} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRig(t)
			g.pipe.Result.From = sys.Result{ExitCode: exit, Stderr: []byte("dump broke")}
			res, err := g.backup(t)
			if err == nil {
				t.Fatal("a failed dump must fail the backup")
			}
			if len(res) != 1 || res[0].OK || !strings.Contains(res[0].Error, "dump broke") {
				t.Fatalf("results: %+v", res)
			}
			if len(g.resticCalls("forget "+snapID+" --prune")) != 1 {
				t.Fatalf("partial snapshot not forgotten: %v", g.calls())
			}
			if len(g.resticCalls(retention)) != 0 {
				t.Fatalf("retention ran after a failure: %v", g.calls())
			}
		})
	}
}

func TestDumpResticFailure(t *testing.T) {
	g := newDumpRig(t)
	g.pipe.Result = sys.PipeResult{
		From: sys.Result{ExitCode: -1},
		To:   sys.Result{ExitCode: 1, Stderr: []byte("repository unreachable")},
	}
	res, err := g.backup(t)
	if err == nil {
		t.Fatal("a failed restic must fail the backup")
	}
	if res[0].OK || !strings.Contains(res[0].Error, "repository unreachable") {
		t.Fatalf("restic is the cause, the producer only got SIGPIPE: %+v", res)
	}
	for _, c := range g.calls() {
		if strings.Contains(c, " forget ") {
			t.Fatalf("nothing to forget: %s", c)
		}
	}
}

// restic exits 3 when it stored a snapshot of incomplete input.
func TestDumpResticIncompleteForgetsSnapshot(t *testing.T) {
	g := newDumpRig(t)
	g.pipe.Result.To.ExitCode = 3
	if _, err := g.backup(t); err == nil {
		t.Fatal("want error")
	}
	if len(g.resticCalls("forget "+snapID+" --prune")) != 1 {
		t.Fatalf("incomplete snapshot not forgotten: %v", g.calls())
	}
}

func TestDumpForgetFailureIsReported(t *testing.T) {
	g := newDumpRig(t)
	g.pipe.Result.From = sys.Result{ExitCode: 1, Stderr: []byte("dump broke")}
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file "+dumpEnv+" restic/restic:0.19.1 --retry-lock 10m forget "+snapID,
		sys.Result{ExitCode: 1, Stderr: []byte("repo locked")})
	res, err := g.backup(t)
	if err == nil {
		t.Fatal("want error")
	}
	for _, want := range []string{"dump broke", snapID, "could not be removed", "repo locked"} {
		if !strings.Contains(res[0].Error, want) {
			t.Fatalf("error misses %q: %s", want, res[0].Error)
		}
	}
}

type ctxRecorder struct {
	*systest.Runner
	match string
	err   error
}

func (r *ctxRecorder) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if strings.Contains(c.String(), r.match) {
		r.err = ctx.Err()
	}
	return r.Runner.Run(ctx, c)
}

func cancellingPipe(cancel func(), res sys.PipeResult) pipeFunc {
	return func(context.Context, sys.Cmd, sys.Cmd) (sys.PipeResult, error) {
		cancel()
		return res, context.Canceled
	}
}

func TestDumpCancelledRunStillForgetsSnapshot(t *testing.T) {
	g := newDumpRig(t)
	rec := &ctxRecorder{Runner: g.run, match: "forget " + snapID, err: errors.New("forget never ran")}
	g.r.Run = rec
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = cancellingPipe(cancel, sys.PipeResult{To: sys.Result{Stdout: resticSummary}})
	if err := g.r.BackupService(ctx, "app-prod", "db"); err == nil {
		t.Fatal("want error")
	}
	if rec.err != nil {
		t.Fatalf("forget context: %v", rec.err)
	}
}

func TestDumpCancelRemovesContainer(t *testing.T) {
	g := newDumpRig(t)
	rec := &ctxRecorder{Runner: g.run, match: "docker rm -f " + dumpContainer, err: errors.New("rm never ran")}
	g.r.Run = rec
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = cancellingPipe(cancel, sys.PipeResult{})
	if err := g.r.BackupService(ctx, "app-prod", "db"); err == nil {
		t.Fatal("want error")
	}
	if rec.err != nil {
		t.Fatalf("rm context: %v", rec.err)
	}
}

func TestDumpFailureWithoutCancelLeavesContainerAlone(t *testing.T) {
	g := newDumpRig(t)
	g.pipe.Result.From.ExitCode = 1
	if _, err := g.backup(t); err == nil {
		t.Fatal("want error")
	}
	for _, c := range g.calls() {
		if strings.HasPrefix(c, "docker rm") {
			t.Fatalf("restic already exited: %s", c)
		}
	}
}

func TestDumpMarksPartialUntilComplete(t *testing.T) {
	g := newDumpRig(t)
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	if to := strings.Join(g.pipe.Calls[0].To.Args, " "); !strings.Contains(to, "--tag partial") {
		t.Fatalf("snapshot not marked partial: %s", to)
	}
	calls := g.calls()
	at := func(suffix string) int {
		return slices.IndexFunc(calls, func(c string) bool { return strings.HasSuffix(c, suffix) })
	}
	iUnmark, iPurge, iRetain := at(unmark), at(purge), at(retention)
	if iUnmark < 0 || iPurge < iUnmark || iRetain < iPurge {
		t.Fatalf("want unmark, purge, retention in order: %v", calls)
	}
}

func TestDumpFailureKeepsPartialMark(t *testing.T) {
	for name, res := range map[string]sys.PipeResult{
		"producer": {From: sys.Result{ExitCode: 1}, To: sys.Result{Stdout: resticSummary}},
		"restic":   {To: sys.Result{ExitCode: 3, Stdout: resticSummary}},
	} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRig(t)
			g.pipe.Result = res
			if _, err := g.backup(t); err == nil {
				t.Fatal("want error")
			}
			if len(g.resticCalls(unmark)) != 0 {
				t.Fatalf("a failed run must stay partial: %v", g.calls())
			}
		})
	}
}

func TestDumpUnmarkFailureForgetsSnapshot(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(tagCmd, sys.Result{ExitCode: 1, Stderr: []byte("repo locked")})
	res, err := g.backup(t)
	if err == nil || res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(g.resticCalls("forget "+snapID+" --prune")) != 1 || len(g.resticCalls(retention)) != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

// Without the id the mark cannot come off, so the snapshot is unusable.
func TestDumpWithoutSnapshotIDFails(t *testing.T) {
	g := newDumpRig(t)
	g.pipe.Result.To.Stdout = nil
	res, err := g.backup(t)
	if err == nil || res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDumpListIgnoresPartial(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(listCmd, sys.Result{Stdout: snapshotsWithPartial})
	snaps, err := g.r.List(context.Background(), "app-prod", "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].ID != "aaaa1111" {
		t.Fatalf("snapshots: %+v", snaps)
	}
}

func TestVolumeListKeepsPartialTag(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 snapshots --json",
		sys.Result{Stdout: snapshotsWithPartial})
	snaps, err := g.r.List(context.Background(), "app-prod", "files")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("a user tag on a volume snapshot means nothing: %+v", snaps)
	}
}

func TestDumpVerifyIgnoresPartial(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(listCmd, sys.Result{Stdout: snapshotsWithPartial})
	g.run.On("docker exec "+dumpVerifyName+" sh -c fakecount", sys.Result{Stdout: []byte("1\n")})
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 || !strings.Contains(g.pipe.Calls[0].From.String(), " dump aaaa1111 db.dump") {
		t.Fatalf("verify must read the newest complete snapshot: %+v", g.pipe.Calls)
	}
}

func TestDumpVerifyWithOnlyPartialFails(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(listCmd, sys.Result{Stdout: []byte(`[{"short_id":"bbbb2222","time":"2026-11-04T03:00:00Z","tags":["partial"]}]`)})
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err == nil || res[0].OK || !strings.Contains(res[0].Error, "no complete") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDumpRestoreIgnoresPartial(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(listCmd, sys.Result{Stdout: snapshotsWithPartial})
	g.run.On("docker volume inspect fresh-vol", sys.Result{ExitCode: 1, Stderr: []byte("no such volume")})
	p, d, _ := g.r.Load("app-prod")
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, "fresh-vol"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 || !strings.Contains(g.pipe.Calls[0].From.String(), " dump aaaa1111 db.dump") {
		t.Fatalf("pipe: %+v", g.pipe.Calls)
	}
}

// A deploy holds the instance lock, and a backup holding the data lock waits
// for that same lock to record its status: waiting here would invert the order.
func TestRestorePointDumpTakesNoDataLock(t *testing.T) {
	g := newDumpRig(t)
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	holdDataLock(t)
	g.r.Sleep = func(time.Duration) { t.Error("the restore point waited for the data lock") }
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 {
		t.Fatalf("pipe calls: %+v", g.pipe.Calls)
	}
	if to := strings.Join(g.pipe.Calls[0].To.Args, " "); !strings.Contains(to, "--retry-lock 10m backup --stdin") {
		t.Fatalf("backup must wait for restic's repository lock: %s", to)
	}
}

// A prune locks the repository exclusively, and tag and forget need that lock
// too, so a retry on the backup alone only moves the collision.
func TestDumpEveryResticCommandRetriesLock(t *testing.T) {
	g := newDumpRig(t)
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, c := range g.calls() {
		if strings.Contains(c, resticImage) {
			n++
			if !strings.Contains(c, resticImage+" --retry-lock 10m ") {
				t.Fatalf("no retry: %s", c)
			}
		}
	}
	if n != 4 {
		t.Fatalf("want snapshots, tag, purge and retention: %v", g.calls())
	}
}

func TestDumpForgetAfterFailureRetriesLock(t *testing.T) {
	g := newDumpRig(t)
	g.pipe.Result.From.ExitCode = 1
	if _, err := g.backup(t); err == nil {
		t.Fatal("want error")
	}
	if len(g.resticCalls("--retry-lock 10m forget "+snapID+" --prune")) != 1 {
		t.Fatalf("calls: %v", g.calls())
	}
}

type pipeFunc func(context.Context, sys.Cmd, sys.Cmd) (sys.PipeResult, error)

func (f pipeFunc) Pipe(ctx context.Context, from, to sys.Cmd) (sys.PipeResult, error) {
	return f(ctx, from, to)
}

func TestDumpNoSecretsInArgv(t *testing.T) {
	g := newDumpRig(t)
	var raw []byte
	g.r.Pipe = racingPiper{Piper: g.pipe, during: func(sys.Cmd) {
		var err error
		if raw, err = g.fs.ReadFile(dumpEnv); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	argv := g.calls()
	for _, c := range g.pipe.Calls {
		argv = append(argv, c.From.String(), c.To.String())
		if slices.Contains(c.From.Args, "-e") || slices.Contains(c.To.Args, "-e") {
			t.Fatalf("docker -e passes env through argv: %v", c)
		}
		for _, e := range append(c.From.Env, c.To.Env...) {
			if strings.Contains(e, resticPass) {
				t.Fatalf("secret in process env: %s", e)
			}
		}
	}
	for _, c := range argv {
		for _, s := range []string{resticPass, secretKey, libsodium} {
			if strings.Contains(c, s) {
				t.Fatalf("secret in argv: %s", c)
			}
		}
	}
	if !strings.Contains(string(raw), resticPass) {
		t.Fatal("restic env file holds no password")
	}
}

// repoSpy notes the repository each restic command of a dump would read from
// its env file at the moment it runs.
type repoSpy struct {
	g     *dumpRig
	repos map[string]string
}

func (s *repoSpy) note(c sys.Cmd) {
	i := slices.Index(c.Args, "--env-file")
	if i < 0 {
		return
	}
	raw, err := s.g.fs.ReadFile(c.Args[i+1])
	if err != nil {
		s.repos[c.String()] = "missing: " + c.Args[i+1]
		return
	}
	for line := range strings.Lines(string(raw)) {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "RESTIC_REPOSITORY="); ok {
			s.repos[c.String()] = strings.Trim(v, `"'`)
		}
	}
}

func spyRepos(g *dumpRig) *repoSpy {
	s := &repoSpy{g: g, repos: map[string]string{}}
	g.r.Run = spyRunner{Runner: g.run, before: func(_ context.Context, c sys.Cmd) (sys.Result, bool) {
		s.note(c)
		return sys.Result{}, false
	}}
	return s
}

// racingPiper runs during the dump whatever else touches the instance then.
type racingPiper struct {
	*systest.Piper
	during func(to sys.Cmd)
}

func (p racingPiper) Pipe(ctx context.Context, from, to sys.Cmd) (sys.PipeResult, error) {
	p.during(to)
	return p.Piper.Pipe(ctx, from, to)
}

func (g *dumpRig) leftoverEnvs(t *testing.T) []string {
	t.Helper()
	entries, err := g.fs.ReadDir("/srv/deploy/app-prod")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "cvctl-restic-") {
			out = append(out, e.Name())
		}
	}
	return out
}

// backup list and a restore point write restic-<svc>.env for whichever
// repository they read last, and neither waits for the data lock. A dump that
// shared the file would tag and prune repository 0 instead of its own.
func TestDumpKeepsItsRepositoryWhileTheSharedEnvIsRewritten(t *testing.T) {
	g := newDumpRig(t)
	g.r.Host.KeyGen = map[string]int{"db": 2}
	g.r.Host.HostResticPassword = "host-pass-SECRET-777"
	spy := spyRepos(g)
	g.r.Pipe = racingPiper{Piper: g.pipe, during: func(to sys.Cmd) {
		spy.note(to)
		if _, err := g.r.List(context.Background(), "app-prod", "db"); err != nil {
			t.Fatal(err)
		}
	}}
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	shared, err := g.fs.ReadFile("/srv/deploy/app-prod/restic-db.env")
	if err != nil || strings.Contains(string(shared), "/db/r2") {
		t.Fatalf("the race did not happen: %v %s", err, shared)
	}
	seen := map[string]bool{}
	for cmd, repo := range spy.repos {
		if strings.Contains(cmd, "restic-db.env") {
			continue
		}
		if !strings.HasSuffix(repo, "/db/r2") {
			t.Fatalf("%s ran against %q", cmd, repo)
		}
		for _, step := range []string{"snapshots --json", "backup --stdin", "tag --json", purge, retention} {
			if strings.Contains(cmd, step) {
				seen[step] = true
			}
		}
	}
	if len(seen) != 5 {
		t.Fatalf("steps checked: %v in %v", seen, g.calls())
	}
}

func TestDumpEnvFileIsUniquePerRun(t *testing.T) {
	g := newDumpRig(t)
	n := 0
	g.r.Suffix = func() string { n++; return fmt.Sprintf("%08x", n) }
	g.r.Run = spyRunner{Runner: g.run, before: func(_ context.Context, c sys.Cmd) (sys.Result, bool) {
		if strings.Contains(c.String(), " tag --json ") {
			return sys.Result{Stdout: tagSummary(1)}, true
		}
		return sys.Result{}, false
	}}
	seen := map[string]bool{}
	for range 2 {
		if _, err := g.backup(t); err != nil {
			t.Fatal(err)
		}
		to := g.pipe.Calls[len(g.pipe.Calls)-1].To.Args
		seen[to[slices.Index(to, "--env-file")+1]] = true
	}
	if len(seen) != 2 || seen["/srv/deploy/app-prod/restic-db.env"] {
		t.Fatalf("env files: %v", seen)
	}
}

func TestDumpRemovesItsEnvFileOnEveryPath(t *testing.T) {
	resticCmd := "docker run --rm --network app-prod_backup --env-file " + dumpEnv + " restic/restic:0.19.1 --retry-lock 10m "
	for name, tc := range map[string]struct {
		breakIt func(*dumpRig)
		ok      bool
	}{
		"success":  {breakIt: func(*dumpRig) {}, ok: true},
		"init":     {breakIt: func(g *dumpRig) { g.run.OnPrefix(resticCmd+"snapshots", sys.Result{ExitCode: 1}) }},
		"network":  {breakIt: func(g *dumpRig) { g.run.OnErrorPrefix("docker network", errors.New("no docker")) }},
		"restic":   {breakIt: func(g *dumpRig) { g.pipe.Result.To.ExitCode = 1 }},
		"no id":    {breakIt: func(g *dumpRig) { g.pipe.Result.To.Stdout = nil }},
		"producer": {breakIt: func(g *dumpRig) { g.pipe.Result.From.ExitCode = 1 }},
		"tag":      {breakIt: func(g *dumpRig) { g.run.OnPrefix(tagCmd, sys.Result{ExitCode: 1}) }},
		"forget": {breakIt: func(g *dumpRig) {
			g.pipe.Result.From.ExitCode = 1
			g.run.OnPrefix(resticCmd+"forget "+snapID, sys.Result{ExitCode: 1})
		}},
		"purge":        {breakIt: func(g *dumpRig) { g.run.OnPrefix(resticCmd+purge, sys.Result{ExitCode: 1}) }},
		"retention":    {breakIt: func(g *dumpRig) { g.run.OnPrefix(resticCmd+retention, sys.Result{ExitCode: 1}) }},
		"pipe crashes": {breakIt: func(g *dumpRig) { g.pipe.Err = errors.New("broken") }},
	} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRig(t)
			tc.breakIt(g)
			spy := spyRepos(g)
			_, err := g.backup(t)
			if tc.ok != (err == nil) {
				t.Fatalf("err: %v", err)
			}
			for cmd, repo := range spy.repos {
				if strings.HasPrefix(repo, "missing") {
					t.Fatalf("%s ran without its file", cmd)
				}
			}
			if left := g.leftoverEnvs(t); len(left) != 0 {
				t.Fatalf("left behind: %v", left)
			}
		})
	}
}

func TestDumpRemovesItsEnvFileWhenCancelled(t *testing.T) {
	g := newDumpRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = racingPiper{Piper: g.pipe, during: func(sys.Cmd) { cancel() }}
	g.pipe.Err = context.Canceled
	if _, err := g.r.BackupNow(ctx, "app-prod", "db"); err == nil {
		t.Fatal("want error")
	}
	if left := g.leftoverEnvs(t); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}
}

func TestDumpNeedsRunningContainer(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(composePS, sys.Result{Stdout: []byte(`{"ID":"c0ffee","Service":"db","State":"exited"}`)})
	res, err := g.backup(t)
	if err == nil || res[0].OK || !strings.Contains(res[0].Error, "not running") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(g.pipe.Calls) != 0 {
		t.Fatal("nothing to dump from")
	}
}

// An empty script would run sh -c "" and store an empty snapshot as a success.
func TestDumpWithoutCommandFails(t *testing.T) {
	g := newDumpRig(t)
	g.r.Catalog["fakedb"] = kinds.Kind{Name: "fakedb", Backup: "dump", Helper: "cvctl-fake"}
	res, err := g.backup(t)
	if err == nil || res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if len(g.pipe.Calls) != 0 {
		t.Fatal("no pipe without a dump command")
	}
}

func TestListUsesRestic(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 snapshots --json",
		sys.Result{Stdout: []byte(`[{"short_id":"4f1c2d3e","time":"2026-11-05T03:00:00Z","tags":["complete"]}]`)})
	snaps, err := g.r.List(context.Background(), "app-prod", "db")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 1 || snaps[0].ID != "4f1c2d3e" || snaps[0].Strategy != "dump" {
		t.Fatalf("snapshots: %+v", snaps)
	}
}

func TestRestorePointTakesDump(t *testing.T) {
	g := newDumpRig(t)
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 1 {
		t.Fatalf("pipe calls: %+v", g.pipe.Calls)
	}
	to := strings.Join(g.pipe.Calls[0].To.Args, " ")
	if !strings.Contains(to, "--tag app-prod --tag deploy-5") {
		t.Fatalf("the snapshot must carry the restore point id: %s", to)
	}
}

func TestRestorePointDumpFailureFailsCreate(t *testing.T) {
	g := newDumpRig(t)
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	g.pipe.Result.From.ExitCode = 1
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err == nil || errors.Is(err, backup.ErrFreshInstance) {
		t.Fatalf("err: %v", err)
	}
}

func TestRestorePointDumpFreshInstance(t *testing.T) {
	g := newDumpRig(t)
	g.run.On(psAll, sys.Result{})
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); !errors.Is(err, backup.ErrFreshInstance) {
		t.Fatalf("err: %v", err)
	}
	if len(g.pipe.Calls) != 0 {
		t.Fatal("no data to protect on a fresh instance")
	}
}

func TestRestorePointDumpWithoutDestinationSkips(t *testing.T) {
	g := newDumpRig(t)
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	g.r.Host = nil
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err != nil {
		t.Fatal(err)
	}
	if len(g.pipe.Calls) != 0 {
		t.Fatal("no destination, no dump")
	}
}

const (
	engineEnvPath = "/srv/deploy/app-prod/engine-db.env"
	dumpEnvBefore = "CVCTL_INSTANCE=app-prod\nCVCTL_SVC_DB_MODE=local\n"
)

var (
	dumpVerifyName = fmt.Sprintf("cvctl-verify-app-prod-db-%d-0a0b0c0d", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix())

	dumpNow     = time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	dumpNewVol  = fmt.Sprintf("app-prod_db-data-r%d", dumpNow.Unix())
	dumpTemp    = fmt.Sprintf("cvctl-restore-app-prod-db-%d-0a0b0c0d", dumpNow.Unix())
	dumpReader  = fmt.Sprintf("cvctl-read-app-prod-db-%d-0a0b0c0d", dumpNow.Unix())
	dumpHistory = []byte(`[
{"short_id":"aaaa1111","time":"2026-11-02T03:00:00Z","tags":["app-prod","complete"]},
{"short_id":"cccc3333","time":"2026-11-03T03:00:00.5+01:00","tags":["app-prod","complete","deploy-5"]},
{"short_id":"dddd4444","time":"2026-11-04T03:00:00Z","tags":["app-prod","complete"]},
{"short_id":"bbbb2222","time":"2026-11-04T09:00:00Z","tags":["app-prod","partial"]},
{"short_id":"eeee5555","time":"2026-11-04T10:00:00Z","tags":["app-prod","deploy-7","partial"]}]`)
)

type dumpRestoreRig struct {
	*dumpRig
	applies int
	failAt  int
}

// PIPE and APPLY markers put both in the call order the tests read.
func newDumpRestoreRig(t *testing.T) *dumpRestoreRig {
	t.Helper()
	g := &dumpRestoreRig{dumpRig: newDumpRig(t)}
	g.r.Clock = func() time.Time { return dumpNow }
	g.r.Sleep = func(time.Duration) {}
	g.run.OnPrefix(tagCmdAt(dumpNow), sys.Result{Stdout: tagSummary(1)})
	if err := g.fs.WriteFile(envPath, []byte(dumpEnvBefore), 0o640, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := g.store.Save(&state.State{Instance: "app-prod", Status: state.StatusOK, Current: &state.Desired{InfraCommit: "abc"}, UpdatedAt: dumpNow}); err != nil {
		t.Fatal(err)
	}
	g.r.Apply = func(context.Context, string, *state.Desired) error {
		g.run.Calls = append(g.run.Calls, sys.Cmd{Name: "APPLY"})
		g.applies++
		if g.applies == g.failAt {
			return errors.New("apply broke")
		}
		return nil
	}
	g.r.Pipe = pipeFunc(func(ctx context.Context, from, to sys.Cmd) (sys.PipeResult, error) {
		g.run.Calls = append(g.run.Calls, sys.Cmd{Name: "PIPE"})
		return g.pipe.Pipe(ctx, from, to)
	})
	g.pipe.Result = sys.PipeResult{}
	g.run.OnPrefix(listCmd, sys.Result{Stdout: dumpHistory})
	g.run.On("docker volume inspect "+dumpNewVol, sys.Result{ExitCode: 1})
	return g
}

func (g *dumpRestoreRig) restore(to string) (*backup.RestoreResult, error) {
	return g.r.Restore(context.Background(), "app-prod", "db", to)
}

func (g *dumpRestoreRig) read() string {
	if len(g.pipe.Calls) != 1 {
		return ""
	}
	return strings.Join(g.pipe.Calls[0].From.Args, " ")
}

func (g *dumpRestoreRig) assertUntouched(t *testing.T) {
	t.Helper()
	for _, c := range g.calls() {
		if strings.Contains(c, " stop db") || strings.Contains(c, " stop web") || c == "APPLY" {
			t.Fatalf("the service was touched: %v", g.calls())
		}
	}
	raw, err := g.fs.ReadFile(envPath)
	if err != nil || string(raw) != dumpEnvBefore {
		t.Fatalf("cvctl.env changed: %s %v", raw, err)
	}
	st, err := g.store.Load("app-prod")
	if err != nil || st.Status != state.StatusOK || st.Current == nil || st.Current.InfraCommit != "abc" {
		t.Fatalf("state changed: %+v %v", st, err)
	}
}

func TestDumpRestoreUsesNewVolume(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("latest")
	if err != nil {
		t.Fatal(err)
	}
	if res.Volume != dumpNewVol || res.PreviousVolume != "app-prod_db-data" || res.Snapshot != "dddd4444" {
		t.Fatalf("res: %+v", res)
	}
	last := -1
	for _, want := range []string{
		"docker volume inspect " + dumpNewVol,
		"docker volume create " + dumpNewVol,
		"docker run -d --name " + dumpTemp,
		"docker exec " + dumpTemp + " sh -c fakeping",
		"PIPE",
		"docker stop -t 120 " + dumpTemp,
		"docker rm -f " + dumpTemp,
		cp + "stop db",
		"APPLY",
		cp + "up -d --wait --wait-timeout 150 db",
	} {
		i := g.index(want)
		if i <= last {
			t.Fatalf("%q out of order (%d <= %d): %v", want, i, last, g.calls())
		}
		last = i
	}
	wantRun := "docker run -d --name " + dumpTemp + " --network none --env-file " + engineEnvPath +
		" --cap-drop ALL --security-opt no-new-privileges:true --cap-add CHOWN --cap-add SETUID -v " + dumpNewVol + ":/var/lib/fake fake/db:1 fakedbd --user root"
	if run := g.calls()[g.index("docker run -d")]; run != wantRun {
		t.Fatalf("engine:\n got %s\nwant %s", run, wantRun)
	}
	wantRead := "run --rm --name " + dumpReader + " --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env " + resticImage + " --retry-lock 10m dump dddd4444 db.dump"
	if got := g.read(); got != wantRead {
		t.Fatalf("reader:\n got %s\nwant %s", got, wantRead)
	}
	if to := g.pipe.Calls[0].To.Args; !slices.Equal(to, []string{"exec", "-i", dumpTemp, "sh", "-c", "fakerestore --all"}) {
		t.Fatalf("consumer: %q", to)
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("a good restore keeps its volume: %v", g.calls())
	}
	raw, _ := g.fs.ReadFile(envPath)
	if !strings.Contains(string(raw), dumpNewVol) {
		t.Fatalf("no swap: %s", raw)
	}
}

func TestDumpRestoreFailureKeepsCurrentVolume(t *testing.T) {
	for name, breakIt := range map[string]func(*dumpRestoreRig){
		"restic":   func(g *dumpRestoreRig) { g.pipe.Result.From.ExitCode = 1 },
		"restore":  func(g *dumpRestoreRig) { g.pipe.Result.To.ExitCode = 1 },
		"pipe":     func(g *dumpRestoreRig) { g.pipe.Err = errors.New("no pipe") },
		"run":      func(g *dumpRestoreRig) { g.run.OnPrefix("docker run -d", sys.Result{ExitCode: 125}) },
		"exited":   func(g *dumpRestoreRig) { g.run.OnPrefix("docker inspect", sys.Result{Stdout: []byte("false\n")}) },
		"health":   func(g *dumpRestoreRig) { g.run.On("docker exec "+dumpTemp+" sh -c fakeping", sys.Result{ExitCode: 1}) },
		"shutdown": func(g *dumpRestoreRig) { g.run.On("docker stop -t 120 "+dumpTemp, sys.Result{ExitCode: 1}) },
	} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRestoreRig(t)
			breakIt(g)
			res, err := g.restore("latest")
			if err == nil || res != nil {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			g.assertUntouched(t)
			rm, rmVol := g.index("docker rm -f "+dumpTemp), g.index("docker volume rm -f "+dumpNewVol)
			if rm < 0 || rmVol < rm {
				t.Fatalf("temporary container and volume must go, in that order: %v", g.calls())
			}
			hist, err := g.store.History("app-prod", 0)
			if err != nil || len(hist) != 1 || hist[0].Result != "restore_failed" {
				t.Fatalf("history: %+v %v", hist, err)
			}
			if len(g.note.msgs) != 1 || g.note.msgs[0].Level != "error" {
				t.Fatalf("notify: %+v", g.note.msgs)
			}
		})
	}
}

func TestDumpRestoreBothSidesFailedReportsBoth(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.pipe.Result = sys.PipeResult{
		From: sys.Result{ExitCode: 1, Stderr: []byte("wrong password")},
		To:   sys.Result{ExitCode: 1, Stderr: []byte("unexpected end of input")},
	}
	_, err := g.restore("latest")
	if err == nil || !strings.Contains(err.Error(), "wrong password") || !strings.Contains(err.Error(), "unexpected end of input") {
		t.Fatalf("err: %v", err)
	}
}

func TestDumpRestoreCancelRemovesEverything(t *testing.T) {
	g := newDumpRestoreRig(t)
	rec := &ctxRecorder{Runner: g.run, match: "docker rm -f " + dumpReader, err: errors.New("rm never ran")}
	g.r.Run = rec
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = cancellingPipe(cancel, sys.PipeResult{})
	if _, err := g.r.Restore(ctx, "app-prod", "db", "latest"); err == nil {
		t.Fatal("want error")
	}
	if rec.err != nil {
		t.Fatalf("rm context: %v", rec.err)
	}
	if g.index("docker rm -f "+dumpTemp) < 0 || g.index("docker volume rm -f "+dumpNewVol) < 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestDumpRestoreRollsBackAfterSwapFailure(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.failAt = 1
	_, err := g.restore("latest")
	if err == nil || !strings.Contains(err.Error(), "was kept for inspection") {
		t.Fatalf("err: %v", err)
	}
	raw, _ := g.fs.ReadFile(envPath)
	if strings.Contains(string(raw), dumpNewVol) {
		t.Fatalf("env still names the new volume: %s", raw)
	}
	if g.applies != 2 {
		t.Fatalf("applies: %d", g.applies)
	}
}

func TestDumpRestoreToTimestampPicksPrevious(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("2026-11-03T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	if res.Snapshot != "cccc3333" || !strings.Contains(g.read(), " dump cccc3333 db.dump") {
		t.Fatalf("res=%+v read=%s", res, g.read())
	}
	said := strings.Join(res.Warnings, "\n")
	for _, want := range []string{"cccc3333", "2026-11-03T03:00:00.5+01:00", "2026-11-03T12:00:00Z", "no point-in-time"} {
		if !strings.Contains(said, want) {
			t.Fatalf("output misses %q: %s", want, said)
		}
	}
}

func TestDumpRestoreTimestampComparesInstantsInclusive(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("2026-11-03T02:00:00.5Z")
	if err != nil || res.Snapshot != "cccc3333" {
		t.Fatalf("a snapshot at the timestamp is not newer: res=%+v err=%v", res, err)
	}
}

func TestDumpRestoreTimestampSkipsPartial(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("2026-11-04T11:00:00Z")
	if err != nil || res.Snapshot != "dddd4444" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDumpRestoreTimestampLooksInOlderRepositories(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.r.Host.KeyGen = map[string]int{"db": 1}
	// Both repositories share the env path, so the listing alternates: the
	// current one is empty, the older one holds the snapshot.
	lists := 0
	g.r.Run = &beforeRun{Runner: g.run, hook: func(c sys.Cmd) {
		if c.String() != listCmd {
			return
		}
		lists++
		out := []byte(`[]`)
		if lists%2 == 0 {
			out = []byte(`[{"short_id":"ffff6666","time":"2026-11-04T03:00:00Z","tags":["complete"]}]`)
		}
		g.run.OnPrefix(listCmd, sys.Result{Stdout: out})
	}}
	res, err := g.restore("2026-11-05T01:00:00Z")
	if err != nil || res.Snapshot != "ffff6666" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDumpRestoreTimestampBeforeEverySnapshotFails(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("2026-11-01T00:00:00Z")
	var oe *output.Error
	if res != nil || !errors.As(err, &oe) || oe.Code != "E_RESTORE_NO_BACKUP" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if g.has("docker volume create") != 0 || len(g.pipe.Calls) != 0 {
		t.Fatalf("nothing may start: %v", g.calls())
	}
	g.assertUntouched(t)
}

func TestDumpRestoreToSnapshotID(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("aaaa1111")
	if err != nil || res.Snapshot != "aaaa1111" || len(res.Warnings) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestDumpRestoreToRestorePoint(t *testing.T) {
	g := newDumpRestoreRig(t)
	res, err := g.restore("deploy-5")
	if err != nil || res.Snapshot != "cccc3333" || !strings.Contains(g.read(), " dump cccc3333 db.dump") {
		t.Fatalf("res=%+v err=%v read=%s", res, err, g.read())
	}
}

func TestDumpRestoreNeverReadsPartial(t *testing.T) {
	for _, to := range []string{"bbbb2222", "deploy-7", "deploy-404", "abcdef12"} {
		t.Run(to, func(t *testing.T) {
			g := newDumpRestoreRig(t)
			if _, err := g.restore(to); err == nil || !strings.Contains(err.Error(), "no complete snapshot") {
				t.Fatalf("err: %v", err)
			}
			if g.has("docker volume create") != 0 {
				t.Fatalf("nothing may start: %v", g.calls())
			}
			g.assertUntouched(t)
		})
	}
}

func TestDumpRestoreRejectsBadTargets(t *testing.T) {
	for _, to := range []string{"yesterday", "2099-01-01T00:00:00Z", "abc"} {
		g := newDumpRestoreRig(t)
		var oe *output.Error
		if _, err := g.restore(to); !errors.As(err, &oe) || oe.Code != "E_RESTORE_TARGET_INVALID" {
			t.Fatalf("%s: %v", to, err)
		}
	}
}

func TestDumpRestoreWithoutCommandFailsCleanly(t *testing.T) {
	g := newDumpRestoreRig(t)
	k := g.r.Catalog["fakedb"]
	k.RestoreCommand = nil
	g.r.Catalog["fakedb"] = k
	if _, err := g.restore("latest"); err == nil || !strings.Contains(err.Error(), "restore_command") {
		t.Fatalf("err: %v", err)
	}
	if g.has("docker run -d") != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
	g.assertUntouched(t)
}

func TestDumpRestoreWithoutHealthFails(t *testing.T) {
	g := newDumpRestoreRig(t)
	k := g.r.Catalog["fakedb"]
	k.Health = nil
	g.r.Catalog["fakedb"] = k
	if _, err := g.restore("latest"); err == nil || !strings.Contains(err.Error(), "health") {
		t.Fatalf("err: %v", err)
	}
	if len(g.pipe.Calls) != 0 {
		t.Fatal("a restore into an engine of unknown readiness")
	}
}

func TestDumpEngineExecHealthForm(t *testing.T) {
	g := newDumpRestoreRig(t)
	k := g.r.Catalog["fakedb"]
	k.Health = []string{"CMD", "fakeping", "--user", "{user}"}
	g.r.Catalog["fakedb"] = k
	if _, err := g.restore("latest"); err != nil {
		t.Fatal(err)
	}
	if g.index("docker exec "+dumpTemp+" fakeping --user root") < 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

type beforeRun struct {
	*systest.Runner
	hook func(sys.Cmd)
}

func (r *beforeRun) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	r.hook(c)
	return r.Runner.Run(ctx, c)
}

// The engine reads its credentials from a file: docker -e would put them in argv.
func TestDumpRestoreNoSecretsInArgv(t *testing.T) {
	g := newDumpRestoreRig(t)
	var body string
	g.r.Run = &beforeRun{Runner: g.run, hook: func(c sys.Cmd) {
		if strings.HasPrefix(c.String(), "docker run -d") {
			raw, _ := g.fs.ReadFile(engineEnvPath)
			body = string(raw)
		}
	}}
	if _, err := g.restore("latest"); err != nil {
		t.Fatal(err)
	}
	if body != "FAKE_ROOT_PASSWORD="+enginePass+"\n" {
		t.Fatalf("engine env file: %q", body)
	}
	if _, err := g.fs.ReadFile(engineEnvPath); err == nil {
		t.Fatal("the engine env file must not outlive docker run")
	}
	argv := append(g.calls(), g.pipe.Calls[0].From.String(), g.pipe.Calls[0].To.String())
	for _, c := range argv {
		for _, s := range []string{enginePass, resticPass, secretKey, libsodium} {
			if strings.Contains(c, s) {
				t.Fatalf("secret in argv: %s", c)
			}
		}
		if strings.Contains(c, " -e ") {
			t.Fatalf("docker -e passes env through argv: %s", c)
		}
	}
}

func TestDumpRestoreFreshReadsDump(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.run.On("docker volume inspect fresh-vol", sys.Result{ExitCode: 1})
	p, d, _ := g.r.Load("app-prod")
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) {
		return nil, nil, errors.New("not deployed yet: the caller's decisions are the ones to use")
	}
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, "fresh-vol"); err != nil {
		t.Fatal(err)
	}
	if g.has("docker volume create fresh-vol") != 1 || !strings.Contains(g.read(), " dump dddd4444 db.dump") {
		t.Fatalf("calls: %v", g.calls())
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestDumpRestoreFreshFailureRemovesVolume(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.run.On("docker volume inspect fresh-vol", sys.Result{ExitCode: 1})
	g.pipe.Result.To.ExitCode = 1
	p, d, _ := g.r.Load("app-prod")
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, "fresh-vol"); err == nil {
		t.Fatal("want error")
	}
	if g.has("docker volume rm -f fresh-vol") == 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func dumpVerifyRig(t *testing.T, check sys.Result) (*dumpRig, string) {
	t.Helper()
	g := newDumpRig(t)
	g.r.Sleep = func(time.Duration) {}
	g.pipe.Result = sys.PipeResult{}
	g.run.OnPrefix(listCmd, sys.Result{Stdout: dumpHistory})
	g.run.On("docker exec "+dumpVerifyName+" sh -c fakecount", check)
	return g, dumpVerifyName
}

func TestDumpVerifyRunsRestoreCheck(t *testing.T) {
	g, name := dumpVerifyRig(t, sys.Result{Stdout: []byte("a notice\n3\n")})
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err != nil || len(res) != 1 || !res[0].OK || res[0].Strategy != "dump" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	last := -1
	for _, want := range []string{
		"docker volume create " + name,
		"docker run -d --name " + name,
		"docker exec " + name + " sh -c fakeping",
		"docker exec " + name + " sh -c fakecount",
		"docker rm -f " + name,
		"docker volume rm -f " + name,
	} {
		i := g.index(want)
		if i <= last {
			t.Fatalf("%q out of order (%d <= %d): %v", want, i, last, g.calls())
		}
		last = i
	}
	if len(g.pipe.Calls) != 1 || !strings.Contains(g.pipe.Calls[0].From.String(), " dump dddd4444 db.dump") {
		t.Fatalf("pipe: %+v", g.pipe.Calls)
	}
	if to := g.pipe.Calls[0].To.Args; !slices.Equal(to, []string{"exec", "-i", name, "sh", "-c", "fakerestore --all"}) {
		t.Fatalf("consumer: %q", to)
	}
	st, err := g.store.Load("app-prod")
	if err != nil || st.Backups["db"].LastVerifyOK.IsZero() || st.Backups["db"].LastVerifyError != "" {
		t.Fatalf("recorded: %+v %v", st.Backups, err)
	}
}

func TestDumpVerifyCleansUp(t *testing.T) {
	for name, tc := range map[string]struct {
		check    sys.Result
		breakIt  func(*dumpRig)
		wantText string
	}{
		"empty":     {check: sys.Result{Stdout: []byte("0\n")}, wantText: "no data"},
		"not a num": {check: sys.Result{Stdout: []byte("many\n")}, wantText: "expected a number"},
		"check":     {check: sys.Result{ExitCode: 1, Stderr: []byte("check broke")}, wantText: "check broke"},
		"restore": {check: sys.Result{Stdout: []byte("3\n")}, wantText: "restore broke", breakIt: func(g *dumpRig) {
			g.pipe.Result.To = sys.Result{ExitCode: 1, Stderr: []byte("restore broke")}
		}},
		"restic": {check: sys.Result{Stdout: []byte("3\n")}, wantText: "repo broke", breakIt: func(g *dumpRig) {
			g.pipe.Result.From = sys.Result{ExitCode: 1, Stderr: []byte("repo broke")}
		}},
		"engine": {check: sys.Result{Stdout: []byte("3\n")}, wantText: "exited", breakIt: func(g *dumpRig) {
			g.run.OnPrefix("docker inspect", sys.Result{Stdout: []byte("false\n")})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			g, temp := dumpVerifyRig(t, tc.check)
			if tc.breakIt != nil {
				tc.breakIt(g)
			}
			res, err := g.r.Verify(context.Background(), "app-prod", "db")
			if err == nil || res[0].OK || !strings.Contains(res[0].Error, tc.wantText) {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if g.has("docker rm -f "+temp) != 1 || g.has("docker volume rm -f "+temp) != 1 {
				t.Fatalf("cleanup: %v", g.calls())
			}
			st, err := g.store.Load("app-prod")
			if err != nil || !strings.Contains(st.Backups["db"].LastVerifyError, tc.wantText) {
				t.Fatalf("recorded: %+v %v", st.Backups, err)
			}
		})
	}
}

// A check that cannot run proves nothing, so it must not pass as a good verify.
func TestDumpVerifyWithoutRestoreCheckFails(t *testing.T) {
	g, _ := dumpVerifyRig(t, sys.Result{})
	k := g.r.Catalog["fakedb"]
	k.RestoreCheck = ""
	g.r.Catalog["fakedb"] = k
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err == nil || res[0].OK || !strings.Contains(res[0].Error, "restore_check") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// keep-daily alone keeps one snapshot per day, so a second deploy on a day would
// drop the first one's restore point. WAL-G keeps a week of them.
func TestDumpRetentionKeepsRestorePointsAsLongAsWalg(t *testing.T) {
	g := newDumpRig(t)
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load, Data: g.r}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	if n := len(g.resticCalls("--retry-lock 10m " + retention)); n != 2 {
		t.Fatalf("restore point and daily runs must both keep a week: %v", g.calls())
	}
}

// restic tag logs a per-snapshot failure and exits 0, so only the count proves
// the mark changed. Trusting the exit left a good dump partial for the purge.
func TestDumpTagZeroChangesFailsBackup(t *testing.T) {
	for name, out := range map[string][]byte{"zero": tagSummary(0), "no summary": nil, "two": tagSummary(2)} {
		t.Run(name, func(t *testing.T) {
			g := newDumpRig(t)
			g.run.OnPrefix(tagCmd, sys.Result{Stdout: out})
			res, err := g.backup(t)
			if err == nil || res[0].OK || !strings.Contains(res[0].Error, "not marked complete") {
				t.Fatalf("res=%+v err=%v", res, err)
			}
			if len(g.resticCalls(purge)) != 0 || len(g.resticCalls(retention)) != 0 {
				t.Fatalf("nothing may be purged: %v", g.calls())
			}
			if len(g.resticCalls("forget "+snapID+" --prune")) != 1 {
				t.Fatalf("a failed run forgets its own snapshot: %v", g.calls())
			}
		})
	}
}

// Another run's snapshot is partial until that run finishes, so the purge is
// bounded by age and names no id but the run's own.
func TestDumpOverlapKeepsRecentPartial(t *testing.T) {
	g := newDumpRig(t)
	g.run.OnPrefix(listCmd, sys.Result{Stdout: snapshotsWithPartial})
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	if len(g.resticCalls(purge)) != 1 {
		t.Fatalf("old leftovers must still go: %v", g.calls())
	}
	for _, c := range g.calls() {
		if strings.Contains(c, "--unsafe-allow-remove-all") {
			t.Fatalf("that removes every partial, recent ones too: %s", c)
		}
		if strings.Contains(c, "bbbb2222") {
			t.Fatalf("touched another run's snapshot: %s", c)
		}
	}
}

func TestRetentionIgnoresPartial(t *testing.T) {
	g := newDumpRig(t)
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	var forgets int
	for _, c := range g.calls() {
		if !strings.Contains(c, " forget ") || strings.Contains(c, "--tag partial") {
			continue
		}
		forgets++
		if !strings.Contains(c, " forget --tag complete ") {
			t.Fatalf("retention must see only complete snapshots: %s", c)
		}
	}
	if forgets != 1 {
		t.Fatalf("calls: %v", g.calls())
	}
}

// Absence of partial proves nothing: only the mark says the dump finished.
func TestDumpReadersRequireComplete(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.run.OnPrefix(listCmd, sys.Result{Stdout: []byte(`[
{"short_id":"aaaa1111","time":"2026-11-03T03:00:00Z","tags":["app-prod","complete"]},
{"short_id":"bbbb2222","time":"2026-11-04T03:00:00Z","tags":["app-prod"]}]`)})
	snaps, err := g.r.List(context.Background(), "app-prod", "db")
	if err != nil || len(snaps) != 1 || snaps[0].ID != "aaaa1111" {
		t.Fatalf("list: %+v %v", snaps, err)
	}
	for _, to := range []string{"latest", "2026-11-05T00:00:00Z"} {
		res, err := g.restore(to)
		if err != nil || res.Snapshot != "aaaa1111" {
			t.Fatalf("%s: res=%+v err=%v", to, res, err)
		}
		g.run.Calls, g.pipe.Calls = nil, nil
		if err := g.fs.WriteFile(envPath, []byte(dumpEnvBefore), 0o640, sys.Owner{}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := g.restore("bbbb2222"); err == nil {
		t.Fatal("a snapshot without the mark must not be restored")
	}
}
