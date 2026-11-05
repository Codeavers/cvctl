package backup_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

const (
	dumpScript = `fakedump --all-databases -p"$FAKE_ROOT_PASSWORD"`
	snapID     = "4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e4f1c2d3e"
	composePS  = "docker compose -p app-prod --env-file /srv/deploy/app-prod/app.env -f /srv/deploy/app-prod/compose.cvctl.yml ps -a --format json db"
	retention  = "forget --keep-daily 7 --keep-weekly 4 --prune"
)

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
	want := "run -i --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 backup --stdin --stdin-filename db.dump --host h1 --tag app-prod --json --quiet"
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
	g.run.OnPrefix("docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-db.env restic/restic:0.19.1 forget "+snapID,
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
	forgetErr error
}

func (r *ctxRecorder) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if strings.Contains(c.String(), "forget "+snapID) {
		r.forgetErr = ctx.Err()
	}
	return r.Runner.Run(ctx, c)
}

func TestDumpCancelledRunStillForgetsSnapshot(t *testing.T) {
	g := newDumpRig(t)
	rec := &ctxRecorder{Runner: g.run, forgetErr: errors.New("forget never ran")}
	g.r.Run = rec
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = pipeFunc(func(context.Context, sys.Cmd, sys.Cmd) (sys.PipeResult, error) {
		cancel()
		return sys.PipeResult{To: sys.Result{Stdout: resticSummary}}, context.Canceled
	})
	if err := g.r.BackupService(ctx, "app-prod", "db"); err == nil {
		t.Fatal("want error")
	}
	if rec.forgetErr != nil {
		t.Fatalf("forget context: %v", rec.forgetErr)
	}
}

func TestDumpCancelledRunWarnsWhenSnapshotUnknown(t *testing.T) {
	g := newDumpRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	g.r.Pipe = pipeFunc(func(context.Context, sys.Cmd, sys.Cmd) (sys.PipeResult, error) {
		cancel()
		return sys.PipeResult{}, context.Canceled
	})
	err := g.r.BackupService(ctx, "app-prod", "db")
	if err == nil || !strings.Contains(err.Error(), "partial snapshot may remain") {
		t.Fatalf("err: %v", err)
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
