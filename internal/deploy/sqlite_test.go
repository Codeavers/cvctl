package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const sqliteComposeYAML = `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }
  hooks:
    pre-deploy:  [{ service: api, run: "node dist/migrate.js" }]

services:
  api:
    image: ghcr.io/org/project-api
    environment:
      STORE_PATH: ${STORE_PATH}
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy: { resources: { limits: { memory: 512m } } }
  store:
    labels: { cvctl.kind: sqlite, cvctl.owner: api, cvctl.env: STORE }
`

const sqliteSpecYAML = `vars:
  STORE_PATH: { source: service, service: store }
`

const sqliteCvctlEnv = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_STRATEGY=recreate
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_DOMAIN_API=api.example.com
CVCTL_DIGEST_API=sha256:0000000000000000000000000000000000000000000000000000000000000001
CVCTL_SVC_STORE_MODE=local
`

const apiImageRef = "ghcr.io/org/project-api@sha256:0000000000000000000000000000000000000000000000000000000000000001"

// orderedRunner notes in the compose fake's log what touches the volume, so a
// test can read one order across docker and compose.
type orderedRunner struct {
	*systest.Runner
	comp *fakeCompose
	// owner, when set, makes the volume answer with the last uid it was given.
	owner   string
	onChown func()
}

func (r *orderedRunner) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	s := c.String()
	if strings.HasPrefix(s, "docker stop ") || strings.HasPrefix(s, "docker start ") {
		r.comp.mu.Lock()
		r.comp.calls = append(r.comp.calls, s)
		r.comp.mu.Unlock()
	}
	if strings.Contains(s, "--cap-add CHOWN") {
		r.comp.mu.Lock()
		r.comp.calls = append(r.comp.calls, "CHOWN "+s)
		r.comp.mu.Unlock()
		if r.onChown != nil {
			r.onChown()
		}
		for _, f := range c.Args {
			if v, ok := strings.CutPrefix(f, "OWNER_UID="); ok && r.owner != "" {
				r.owner = v
			}
		}
	} else if r.owner != "" && strings.HasPrefix(s, "docker run --rm --network none") {
		return sys.Result{Stdout: []byte(r.owner + "\n")}, nil
	}
	return r.Runner.Run(ctx, c)
}

func sqliteRig(t *testing.T, cvctlEnv string) (*testRig, *systest.Runner) {
	t.Helper()
	rig := setupTestRig(t, "dev", "demo-dev")
	for name, body := range map[string]string{
		filepath.Join(rig.infraDir, "compose.yml"):  sqliteComposeYAML,
		filepath.Join(rig.infraDir, "env.spec.yml"): sqliteSpecYAML,
		filepath.Join(rig.instDir, "cvctl.env"):     cvctlEnv,
	} {
		if err := os.WriteFile(name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := &systest.Runner{}
	run.OnPrefix("docker", sys.Result{})
	run.On("docker image inspect --format {{.Config.User}} "+apiImageRef, sys.Result{Stdout: []byte("1000\n")})
	run.OnPrefix("docker run --rm --network none", sys.Result{Stdout: []byte("0\n")})
	rig.deployer.Run = &orderedRunner{Runner: run, comp: rig.compose}
	return rig, run
}

func TestSqliteDeployPreparesVolumeBeforeAnythingOpensIt(t *testing.T) {
	rig, _ := sqliteRig(t, sqliteCvctlEnv)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy: %v %+v", err, res)
	}
	calls := rig.compose.calls
	chown := slices.IndexFunc(calls, func(c string) bool { return strings.HasPrefix(c, "CHOWN ") })
	if chown < 0 || !strings.Contains(calls[chown], "OWNER_UID=1000") || !strings.Contains(calls[chown], "demo-dev_store-data:/data") {
		t.Fatalf("calls: %v", calls)
	}
	for i, c := range calls {
		if i < chown && (strings.HasPrefix(c, "hook") || strings.HasPrefix(c, "runhook") || strings.HasPrefix(c, "up")) {
			t.Fatalf("%s ran before the volume was prepared: %v", c, calls)
		}
		if strings.Contains(c, "pull") && i > chown {
			t.Fatalf("the image says who the user is, so the pull goes first: %v", calls)
		}
	}
}

func TestSqliteDeployStartsNoDataService(t *testing.T) {
	rig, _ := sqliteRig(t, sqliteCvctlEnv)
	if res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"}); err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy: %v %+v", err, res)
	}
	for _, c := range rig.compose.calls {
		if strings.Contains(c, "store") && !strings.HasPrefix(c, "CHOWN ") {
			t.Fatalf("store has no compose service: %v", rig.compose.calls)
		}
	}
	env, err := os.ReadFile(filepath.Join(rig.instDir, "app.env"))
	if err != nil || !strings.Contains(string(env), "STORE_PATH=/data/store/app.db") {
		t.Fatalf("app.env: %s %v", env, err)
	}
}

func TestSqliteRejectsBlueGreen(t *testing.T) {
	rig, run := sqliteRig(t, strings.Replace(sqliteCvctlEnv, "CVCTL_STRATEGY=recreate", "CVCTL_STRATEGY=blue-green", 1))
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusFailed || !strings.Contains(res.Detail, "E_SQLITE_RECREATE") {
		t.Fatalf("res: %+v", res)
	}
	if len(run.Calls) != 0 || rig.compose.upCount != 0 {
		t.Fatalf("nothing may run: %v %v", run.Calls, rig.compose.calls)
	}
}

const (
	oldDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000001"
	newDigest = "sha256:0000000000000000000000000000000000000000000000000000000000000002"
)

// uidChangeRig is an instance on the old release, uid 1000, whose new release
// runs as 2000. A volume answers with the owner it was last given.
func uidChangeRig(t *testing.T) (*testRig, state.Desired, state.Desired) {
	t.Helper()
	rig, run := sqliteRig(t, sqliteCvctlEnv)
	run.On("docker image inspect --format {{.Config.User}} ghcr.io/org/project-api@"+oldDigest, sys.Result{Stdout: []byte("1000\n")})
	run.On("docker image inspect --format {{.Config.User}} ghcr.io/org/project-api@"+newDigest, sys.Result{Stdout: []byte("2000\n")})
	run.OnPrefix("docker run --rm --network none", sys.Result{Stdout: []byte("1000\n")})
	old := defaultDesired()
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &old}); err != nil {
		t.Fatal(err)
	}
	next := defaultDesired()
	next.InfraCommit = "2222222222222222222222222222222222222222"
	next.Digests = map[string]string{"api": newDigest}
	rig.deployer.Run.(*orderedRunner).owner = "1000"
	return rig, old, next
}

func chowns(rig *testRig) []string {
	var out []string
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "CHOWN ") {
			for _, f := range strings.Fields(c) {
				if strings.HasPrefix(f, "OWNER_UID=") {
					out = append(out, strings.TrimPrefix(f, "OWNER_UID="))
				}
			}
		}
	}
	return out
}

func TestSqliteAutomaticRollbackGivesTheVolumeBack(t *testing.T) {
	rig, _, next := uidChangeRig(t)
	calls := 0
	rig.deployer.Health = func(context.Context, string, string) error {
		calls++
		if calls == 1 {
			return errors.New("unhealthy")
		}
		return nil
	}
	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	if got := chowns(rig); !slices.Equal(got, []string{"2000", "1000"}) {
		t.Fatalf("chowns %v in %v", got, rig.compose.calls)
	}
	last := slices.IndexFunc(rig.compose.calls, func(c string) bool { return strings.Contains(c, "OWNER_UID=1000") })
	if !slices.ContainsFunc(rig.compose.calls[last:], func(c string) bool { return strings.HasPrefix(c, "up") }) {
		t.Fatalf("the old app must start after the volume is back: %v", rig.compose.calls)
	}
}

func TestSqliteManualRollbackGivesTheVolumeBack(t *testing.T) {
	rig, old, next := uidChangeRig(t)
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &next, Previous: &old}); err != nil {
		t.Fatal(err)
	}
	rig.deployer.Run.(*orderedRunner).owner = "2000"
	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "cli")
	if err != nil || res.Status != state.StatusPaused {
		t.Fatalf("rollback: %v %+v", err, res)
	}
	if got := chowns(rig); !slices.Equal(got, []string{"1000"}) {
		t.Fatalf("chowns %v in %v", got, rig.compose.calls)
	}
	chown := slices.IndexFunc(rig.compose.calls, func(c string) bool { return strings.HasPrefix(c, "CHOWN ") })
	if !slices.ContainsFunc(rig.compose.calls[chown:], func(c string) bool { return strings.HasPrefix(c, "up") }) {
		t.Fatalf("calls: %v", rig.compose.calls)
	}
}

// A failed pre-deploy hook never reaches a swap: the old app keeps running, and
// its volume must not stay with the new release's user.
func TestSqliteFailureBeforePromotionGivesTheVolumeBack(t *testing.T) {
	rig, _, next := uidChangeRig(t)
	rig.compose.failHook["api:node dist/migrate.js"] = struct {
		res sys.Result
		err error
	}{sys.Result{ExitCode: 1, Stderr: []byte("migration failed")}, nil}
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusFailed {
		t.Fatalf("deploy: %v %+v", err, res)
	}
	if got := chowns(rig); !slices.Equal(got, []string{"2000", "1000"}) {
		t.Fatalf("chowns %v in %v", got, rig.compose.calls)
	}
}

const (
	oldAppPS     = "docker ps -q --filter label=com.docker.compose.project=demo-dev --filter volume=demo-dev_store-data"
	oldAppExited = "docker ps -aq --filter status=exited --filter id=0ldapp"
	oldAppStop   = "docker stop 0ldapp"
	oldAppStart  = "docker start 0ldapp"
)

// oldAppRig is uidChangeRig with the old app mounting the volume.
func oldAppRig(t *testing.T) (*testRig, *systest.Runner, state.Desired) {
	t.Helper()
	rig, _, next := uidChangeRig(t)
	run := rig.deployer.Run.(*orderedRunner).Runner
	run.On(oldAppPS, sys.Result{Stdout: []byte("0ldapp\n")})
	run.On(oldAppExited, sys.Result{Stdout: []byte("0ldapp\n")})
	return rig, run, next
}

func callIndex(rig *testRig, prefix string) int {
	return slices.IndexFunc(rig.compose.calls, func(c string) bool { return strings.HasPrefix(c, prefix) })
}

func TestSqliteUIDChangeStopsTheOwnerBeforeTheChown(t *testing.T) {
	rig, _, next := oldAppRig(t)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy: %v %+v", err, res)
	}
	stop, chown := callIndex(rig, oldAppStop), callIndex(rig, "CHOWN ")
	if stop < 0 || chown < stop {
		t.Fatalf("the old app must stop before its volume changes hands: %v", rig.compose.calls)
	}
	hook := callIndex(rig, "runhook")
	if hook < 0 || hook < chown {
		t.Fatalf("a hook needing the volume runs after the chown: %v", rig.compose.calls)
	}
	if callIndex(rig, "docker start") >= 0 {
		t.Fatalf("the new release replaces it: %v", rig.compose.calls)
	}
	if got := chowns(rig); !slices.Equal(got, []string{"2000"}) {
		t.Fatalf("chowns %v", got)
	}
}

func TestSqliteSameUIDLeavesTheOwnerRunning(t *testing.T) {
	rig, run, next := oldAppRig(t)
	run.On("docker image inspect --format {{.Config.User}} ghcr.io/org/project-api@"+newDigest, sys.Result{Stdout: []byte("1000\n")})
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy: %v %+v", err, res)
	}
	if callIndex(rig, "docker stop") >= 0 || len(chowns(rig)) != 0 {
		t.Fatalf("nothing changes hands: %v", rig.compose.calls)
	}
}

func TestSqliteFirstDeployStopsNothing(t *testing.T) {
	rig, run := sqliteRig(t, sqliteCvctlEnv)
	run.On(oldAppPS, sys.Result{Stdout: []byte("0ldapp\n")})
	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	if callIndex(rig, "docker stop") >= 0 {
		t.Fatalf("no old app: %v", rig.compose.calls)
	}
}

func TestSqliteFailureBeforePromotionRestartsTheOwner(t *testing.T) {
	for name, breakIt := range map[string]func(*testRig, *systest.Runner){
		"hook": func(rig *testRig, _ *systest.Runner) {
			rig.compose.failHook["api:node dist/migrate.js"] = struct {
				res sys.Result
				err error
			}{sys.Result{ExitCode: 1, Stderr: []byte("migration failed")}, nil}
		},
		"data services": func(rig *testRig, _ *systest.Runner) { rig.compose.failUpData = errors.New("no data") },
		"chown": func(_ *testRig, run *systest.Runner) {
			run.OnPrefix("docker run --rm --cap-add CHOWN -e OWNER_UID=2000", sys.Result{ExitCode: 1, Stderr: []byte("broken")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig, run, next := oldAppRig(t)
			breakIt(rig, run)
			if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"}); err != nil {
				t.Fatal(err)
			}
			if got := chowns(rig); !slices.Equal(got, []string{"2000", "1000"}) {
				t.Fatalf("chowns %v in %v", got, rig.compose.calls)
			}
			back := slices.IndexFunc(rig.compose.calls, func(c string) bool { return strings.Contains(c, "OWNER_UID=1000") })
			start := callIndex(rig, oldAppStart)
			if start < back {
				t.Fatalf("the old app starts once the volume is back: %v", rig.compose.calls)
			}
		})
	}
}

func TestSqliteCancelledDeployRestartsTheOwner(t *testing.T) {
	rig, _, next := oldAppRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rig.deployer.Run.(*orderedRunner).onChown = cancel
	_, _ = rig.deployer.Deploy(ctx, "demo-dev", next, Options{Actor: "agent"})
	if callIndex(rig, oldAppStart) < 0 {
		t.Fatalf("a cancelled deploy leaves the old app down: %v", rig.compose.calls)
	}
}

func TestSqliteStopFailureTouchesNoVolume(t *testing.T) {
	rig, run, next := oldAppRig(t)
	run.On(oldAppStop, sys.Result{ExitCode: 1, Stderr: []byte("cannot stop")})
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status == state.StatusOK || len(chowns(rig)) != 0 {
		t.Fatalf("res %+v calls %v", res, rig.compose.calls)
	}
	if callIndex(rig, oldAppStart) < 0 {
		t.Fatalf("a stop that half worked must be undone: %v", rig.compose.calls)
	}
}

func TestSqliteOwnerNotRestartedReachesTheResult(t *testing.T) {
	rig, run, next := oldAppRig(t)
	rig.compose.failUpData = errors.New("no data")
	run.On(oldAppStart, sys.Result{ExitCode: 1, Stderr: []byte("cannot start")})
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", next, Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Detail, "old app not restarted") {
		t.Fatalf("detail: %q", res.Detail)
	}
}

func TestSqliteFailedFirstDeployLeavesTheVolumeAlone(t *testing.T) {
	rig, _ := sqliteRig(t, sqliteCvctlEnv)
	rig.compose.failHook["api:node dist/migrate.js"] = struct {
		res sys.Result
		err error
	}{sys.Result{ExitCode: 1, Stderr: []byte("migration failed")}, nil}
	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	if got := chowns(rig); !slices.Equal(got, []string{"1000"}) {
		t.Fatalf("no old app to give it back to: %v", got)
	}
}

func TestSqliteUnusableImageUserFailsForGood(t *testing.T) {
	rig, run := sqliteRig(t, sqliteCvctlEnv)
	run.On("docker image inspect --format {{.Config.User}} "+apiImageRef, sys.Result{Stdout: []byte("node\n")})
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusFailed || !strings.Contains(res.Detail, "E_SQLITE_UID") || !strings.Contains(res.Detail, "cvctl.uid") {
		t.Fatalf("res: %+v", res)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil || st.FailedHash == "" {
		t.Fatalf("retrying the same state cannot help: %+v %v", st, err)
	}
}

func TestSqliteHelperFailureIsRetried(t *testing.T) {
	rig, run := sqliteRig(t, sqliteCvctlEnv)
	run.OnPrefix("docker run --rm --network none", sys.Result{ExitCode: 125, Stderr: []byte("pull failed")})
	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil || st.FailedHash != "" {
		t.Fatalf("a registry hiccup must not fail this state for good: %+v %v", st, err)
	}
	if rig.compose.upCount != 0 {
		t.Fatalf("the app started on an unprepared volume: %v", rig.compose.calls)
	}
}
