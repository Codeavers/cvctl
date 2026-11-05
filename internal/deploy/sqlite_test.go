package deploy

import (
	"context"
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
}

func (r orderedRunner) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if s := c.String(); strings.Contains(s, "--cap-add CHOWN") {
		r.comp.mu.Lock()
		r.comp.calls = append(r.comp.calls, "CHOWN "+s)
		r.comp.mu.Unlock()
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
	rig.deployer.Run = orderedRunner{Runner: run, comp: rig.compose}
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
