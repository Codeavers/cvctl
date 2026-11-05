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
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

const (
	dumpScript  = `fakedump --all-databases -p"$FAKE_ROOT_PASSWORD"`
	snapID      = "4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e"
	composePS   = "docker compose -p app-prod --env-file /srv/deploy/app-prod/app.env -f /srv/deploy/app-prod/compose.cvctl.yml ps -a --format json db"
	retention   = "forget --keep-daily 7 --keep-weekly 4 --prune"
	unmark      = "tag --remove partial " + snapID
	resticImage = "restic/restic:0.19.1"
	purge       = "forget --tag partial --unsafe-allow-remove-all"
	listCmd     = "docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 snapshots --json"
)

// Newest last, as restic prints them.
var snapshotsWithPartial = []byte(`[
{"short_id":"aaaa1111","time":"2026-11-03T03:00:00Z","tags":["app-prod"]},
{"short_id":"bbbb2222","time":"2026-11-04T03:00:00Z","tags":["app-prod","partial"]}]`)

var dumpContainer = fmt.Sprintf("cvctl-dump-app-prod-db-%d-0a0b0c0d", time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Unix())

var resticSummary = []byte(`{"message_type":"status","percent_done":1}
{"message_type":"summary","files_new":1,"snapshot_id":"` + snapID + `"}
`)

type dumpRig struct {
	*rig
	pipe *systest.Piper
}

func newDumpRig(t *testing.T) *dumpRig {
	t.Helper()
	g := newRig(t)
	g.okAll()
	g.run.OnPrefix(composePS, sys.Result{Stdout: []byte(`{"ID":"c0ffee","Service":"db","State":"running"}`)})
	pipe := &systest.Piper{Result: sys.PipeResult{To: sys.Result{Stdout: resticSummary}}}
	g.r.Pipe = pipe
	g.r.Catalog = kinds.Catalog{"fakedb": {
		Name: "fakedb", Backup: "dump",
		DumpCommand: []string{dumpScript}, RestoreCommand: []string{"fakerestore"},
	}}
	p := &contract.Project{Data: []contract.DataService{{Name: "db", Kind: "fakedb", Backup: "dump"}}}
	d := &instance.Decisions{Instance: "app-prod", Services: map[string]instance.ServiceDecision{"db": {Mode: "local"}}}
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
	want := "run -i --rm --name " + dumpContainer + " --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 --retry-lock 10m backup --stdin --stdin-filename db.dump --host h1 --tag app-prod --tag partial --json --quiet"
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
	k.DumpCommand = []string{"fakedump", "--all", `-p"$X"`}
	g.r.Catalog["fakedb"] = k
	if _, err := g.backup(t); err != nil {
		t.Fatal(err)
	}
	if args := g.pipe.Calls[0].From.Args; args[len(args)-1] != `fakedump --all -p"$X"` {
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
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 --retry-lock 10m forget "+snapID,
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
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 --retry-lock 10m tag",
		sys.Result{ExitCode: 1, Stderr: []byte("repo locked")})
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
	g.run.OnPrefix("docker run --name cvctl-verify", sys.Result{Stdout: []byte("1\n")})
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	for _, c := range g.calls() {
		if strings.Contains(c, "restic restore") && !strings.Contains(c, "restic restore aaaa1111 ") {
			t.Fatalf("verify must read the newest complete snapshot: %s", c)
		}
	}
	if !strings.Contains(strings.Join(g.calls(), "\n"), "restic restore aaaa1111 ") {
		t.Fatalf("no restore: %v", g.calls())
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
	all := strings.Join(g.calls(), "\n")
	if !strings.Contains(all, " restore aaaa1111") || strings.Contains(all, " restore latest") || strings.Contains(all, "bbbb2222") {
		t.Fatalf("calls: %v", g.calls())
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
	raw, err := g.fs.ReadFile("/srv/deploy/app-prod/restic-db.env")
	if err != nil || !strings.Contains(string(raw), resticPass) {
		t.Fatalf("restic env file: %v", err)
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
		sys.Result{Stdout: []byte(`[{"short_id":"4f1c2d3e","time":"2026-11-05T03:00:00Z"}]`)})
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
