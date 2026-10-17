package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

type composeCall struct {
	op       string
	override string
	envFile  string
	service  string
	script   string
	services []string
}

type fakeCompose struct {
	mu          sync.Mutex
	calls       []string
	invocations []composeCall
	failConfig  error
	failPull    error
	failUpData  error
	failHook    map[string]struct {
		res sys.Result
		err error
	}
	failUp      error
	failLogs    error
	hookOutputs map[string]sys.Result
	upCount     int
}

type boundCompose struct {
	fake     *fakeCompose
	override string
	envFile  string
}

func (b *boundCompose) Config(_ context.Context) error {
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	b.fake.calls = append(b.fake.calls, "config")
	b.fake.invocations = append(b.fake.invocations, composeCall{op: "config", override: b.override, envFile: b.envFile})
	return b.fake.failConfig
}

func (b *boundCompose) Pull(_ context.Context, services ...string) error {
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	b.fake.calls = append(b.fake.calls, "pull:"+strings.Join(services, ","))
	b.fake.invocations = append(b.fake.invocations, composeCall{op: "pull", override: b.override, envFile: b.envFile, services: services})
	return b.fake.failPull
}

func (b *boundCompose) UpData(_ context.Context, services []string) error {
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	b.fake.calls = append(b.fake.calls, "updata:"+strings.Join(services, ","))
	b.fake.invocations = append(b.fake.invocations, composeCall{op: "updata", override: b.override, envFile: b.envFile, services: services})
	return b.fake.failUpData
}

func (b *boundCompose) RunHook(_ context.Context, service, script string) (sys.Result, error) {
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	key := service + ":" + script
	b.fake.calls = append(b.fake.calls, "runhook:"+key)
	b.fake.invocations = append(b.fake.invocations, composeCall{op: "runhook", override: b.override, envFile: b.envFile, service: service, script: script})
	if f, ok := b.fake.failHook[key]; ok {
		return f.res, f.err
	}
	if out, ok := b.fake.hookOutputs[key]; ok {
		return out, nil
	}
	return sys.Result{ExitCode: 0, Stdout: []byte("hook ok")}, nil
}

func (b *boundCompose) Up(_ context.Context) error {
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	b.fake.upCount++
	b.fake.calls = append(b.fake.calls, "up")
	b.fake.calls = append(b.fake.calls, "up:--remove-orphans")
	b.fake.invocations = append(b.fake.invocations, composeCall{op: "up", override: b.override, envFile: b.envFile})
	return b.fake.failUp
}

func (b *boundCompose) Logs(_ context.Context, service string, tail int) (string, error) {
	b.fake.mu.Lock()
	defer b.fake.mu.Unlock()
	b.fake.calls = append(b.fake.calls, fmt.Sprintf("logs:%s:%d", service, tail))
	b.fake.invocations = append(b.fake.invocations, composeCall{op: "logs", override: b.override, envFile: b.envFile, service: service})
	if b.fake.failLogs != nil {
		return "", b.fake.failLogs
	}
	return "service container logs here", nil
}

type pullCheckingCompose struct {
	*boundCompose
	onPull func()
}

func (p *pullCheckingCompose) Pull(ctx context.Context, services ...string) error {
	if p.onPull != nil {
		p.onPull()
	}
	return p.boundCompose.Pull(ctx, services...)
}

type errorFS struct {
	sys.FS
	failReadFile  map[string]error
	failWriteFile map[string]error
}

func (e *errorFS) ReadFile(path string) ([]byte, error) {
	if err, ok := e.failReadFile[path]; ok {
		return nil, err
	}
	return e.FS.ReadFile(path)
}

func (e *errorFS) WriteFile(path string, data []byte, mode os.FileMode, o sys.Owner) error {
	if err, ok := e.failWriteFile[path]; ok {
		return err
	}
	return e.FS.WriteFile(path, data, mode, o)
}

type testRig struct {
	deployer *Deployer
	repo     *fakeDeployRepo
	compose  *fakeCompose
	notifier *fakeNotifier
	restore  *fakeRestore
	store    *state.Store
	fs       sys.FS
	tempDir  string
	instDir  string
	infraDir string
	fixedNow time.Time
}

type fakeDeployRepo struct {
	mu        sync.Mutex
	checkouts []string
	failAt    map[string]error
}

func (r *fakeDeployRepo) Checkout(_ context.Context, commit string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkouts = append(r.checkouts, commit)
	if err, ok := r.failAt[commit]; ok {
		return err
	}
	return nil
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages []notify.Message
}

func (n *fakeNotifier) Send(_ context.Context, m notify.Message) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.messages = append(n.messages, m)
	return nil
}

type fakeRestore struct {
	mu    sync.Mutex
	calls []string
	fail  error
}

func (r *fakeRestore) Create(_ context.Context, inst, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, inst+":"+id)
	return r.fail
}

const sampleComposeYAML = `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }
  verify: { service: main-db, sql: "select count(*) from users" }
  hooks:
    pre-deploy:  [{ service: api, run: "node dist/migrate.js" }]
    post-deploy: [{ service: api, run: "node dist/warmup.js" }]

services:
  api:
    image: ghcr.io/org/project-api
    environment:
      DATABASE_URL: ${DATABASE_URL}
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy: { resources: { limits: { memory: 512m } } }
  main-db:
    labels: { cvctl.kind: postgres, cvctl.version: "16", cvctl.env: DATABASE }
`

const sampleEnvSpecYAML = `vars:
  DATABASE_URL: { source: service, service: main-db }
`

const sampleCvctlEnv = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_STRATEGY=recreate
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_DOMAIN_API=api.example.com
CVCTL_DIGEST_API=sha256:0000000000000000000000000000000000000000000000000000000000000001
CVCTL_SVC_MAIN_DB_MODE=local
CVCTL_SVC_MAIN_DB_USER=main_db
CVCTL_SVC_MAIN_DB_DB=main_db
CVCTL_SVC_MAIN_DB_PASSWORD=supersecretpassword123456789012
`

func setupTestRig(t *testing.T, envName, instName string) *testRig {
	t.Helper()
	tempDir := t.TempDir()
	deployRoot := filepath.Join(tempDir, "deploy")
	traefikDir := filepath.Join(tempDir, "traefik")
	stateDir := filepath.Join(tempDir, "state")

	instDir := filepath.Join(deployRoot, instName)
	infraDir := filepath.Join(instDir, "infra")
	if err := os.MkdirAll(infraDir, 0o755); err != nil {
		t.Fatalf("mkdir infraDir: %v", err)
	}
	if err := os.MkdirAll(traefikDir, 0o755); err != nil {
		t.Fatalf("mkdir traefikDir: %v", err)
	}

	if err := os.WriteFile(filepath.Join(infraDir, "compose.yml"), []byte(sampleComposeYAML), 0o644); err != nil {
		t.Fatalf("write compose.yml: %v", err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "env.spec.yml"), []byte(sampleEnvSpecYAML), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	cvctlEnvContent := sampleCvctlEnv
	if envName != "dev" || instName != "demo-dev" {
		cvctlEnvContent = strings.ReplaceAll(cvctlEnvContent, "demo-dev", instName)
		cvctlEnvContent = strings.ReplaceAll(cvctlEnvContent, "CVCTL_ENV=dev", "CVCTL_ENV="+envName)
	}
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(cvctlEnvContent), 0o644); err != nil {
		t.Fatalf("write cvctl.env: %v", err)
	}

	osFS := sys.NewOSFS()
	fl := systest.NewFakeLocker()
	oldLockFile := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLockFile })
	sys.LockFile = fl.LockFile

	store := state.NewStore(osFS, stateDir)
	repo := &fakeDeployRepo{failAt: make(map[string]error)}
	comp := &fakeCompose{
		failHook: make(map[string]struct {
			res sys.Result
			err error
		}),
		hookOutputs: make(map[string]sys.Result),
	}
	notifier := &fakeNotifier{}
	restore := &fakeRestore{}
	cat, err := kinds.Load()
	if err != nil {
		t.Fatalf("kinds.Load: %v", err)
	}

	fixedNow := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

	deployer := &Deployer{
		DeployRoot: deployRoot,
		TraefikDir: traefikDir,
		FS:         osFS,
		Store:      store,
		Compose: func(inst, override, envFile string) ComposeAPI {
			return &boundCompose{fake: comp, override: override, envFile: envFile}
		},
		Repo: func(string) CheckoutAPI {
			return repo
		},
		Notify:  notifier,
		Restore: restore,
		Host: &hostcfg.Config{
			HostName: "server.example.com",
			EdgeMode: "edge",
		},
		Catalog: cat,
		Clock: func() time.Time {
			return fixedNow
		},
		Health: func(ctx context.Context, inst, override string) error {
			return nil
		},
	}

	return &testRig{
		deployer: deployer,
		repo:     repo,
		compose:  comp,
		notifier: notifier,
		restore:  restore,
		store:    store,
		fs:       osFS,
		tempDir:  tempDir,
		instDir:  instDir,
		infraDir: infraDir,
		fixedNow: fixedNow,
	}
}

func defaultDesired() state.Desired {
	return state.Desired{
		InfraCommit: "1111111111111111111111111111111111111111",
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		},
	}
}

func TestDeployHappyPath(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	healthCalled := false
	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		healthCalled = true
		return nil
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "cli:andres"})
	if err != nil {
		t.Fatalf("Deploy returned unexpected error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected status OK, got %v (detail: %s)", res.Status, res.Detail)
	}
	if !healthCalled {
		t.Fatalf("health check was not called")
	}

	// Verify order of calls: checkout -> config -> pull -> updata -> hooks -> up
	if len(rig.repo.checkouts) == 0 || rig.repo.checkouts[0] != want.InfraCommit {
		t.Fatalf("expected checkout of %s, got %v", want.InfraCommit, rig.repo.checkouts)
	}

	calls := rig.compose.calls
	findCall := func(prefix string) int {
		for i, c := range calls {
			if strings.HasPrefix(c, prefix) {
				return i
			}
		}
		return -1
	}

	idxConfig := findCall("config")
	idxPull := findCall("pull")
	idxUpData := findCall("updata")
	idxHookPre := findCall("runhook:api:node dist/migrate.js")
	idxUp := -1
	for i, c := range calls {
		if c == "up" {
			idxUp = i
			break
		}
	}
	idxHookPost := findCall("runhook:api:node dist/warmup.js")

	if idxConfig == -1 || idxPull == -1 || idxUpData == -1 || idxHookPre == -1 || idxUp == -1 || idxHookPost == -1 {
		t.Fatalf("missing one of expected calls in: %v", calls)
	}

	if !(idxConfig < idxPull && idxPull < idxUpData && idxUpData < idxHookPre && idxHookPre < idxUp && idxUp < idxHookPost) {
		t.Fatalf("calls out of expected order: %v", calls)
	}

	// State verify
	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if st.Status != state.StatusOK {
		t.Fatalf("expected state status OK, got %v", st.Status)
	}
	if st.Current == nil || st.Current.InfraCommit != want.InfraCommit {
		t.Fatalf("expected Current to be set to want, got %v", st.Current)
	}
	if st.FailedHash != "" {
		t.Fatalf("expected FailedHash to be cleared, got %q", st.FailedHash)
	}

	// Notifier verify
	if len(rig.notifier.messages) == 0 {
		t.Fatalf("expected notification message, got none")
	}
	lastMsg := rig.notifier.messages[len(rig.notifier.messages)-1]
	if lastMsg.Level != "info" || !strings.Contains(lastMsg.Title, "demo-dev deploy OK") {
		t.Fatalf("expected info notification with title 'demo-dev deploy OK', got %+v", lastMsg)
	}
}

func TestFirstDeployNoAppEnv(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	finalAppEnvPath := filepath.Join(rig.instDir, "app.env")
	_ = rig.fs.Remove(finalAppEnvPath)

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy returned unexpected error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected StatusOK, got %v (detail: %s)", res.Status, res.Detail)
	}

	stagingAppEnv := filepath.Join(rig.instDir, ".staging", "app.env")
	stagingOverride := filepath.Join(rig.instDir, ".staging", "compose.cvctl.yml")

	foundConfig := false
	foundPreHook := false
	for _, inv := range rig.compose.invocations {
		if inv.op == "config" {
			foundConfig = true
			if inv.envFile != stagingAppEnv {
				t.Fatalf("expected config envFile %s, got %s", stagingAppEnv, inv.envFile)
			}
			if inv.override != stagingOverride {
				t.Fatalf("expected config override %s, got %s", stagingOverride, inv.override)
			}
		}
		if inv.op == "runhook" && inv.script == "node dist/migrate.js" {
			foundPreHook = true
			if inv.envFile != stagingAppEnv {
				t.Fatalf("expected pre-deploy hook envFile %s, got %s", stagingAppEnv, inv.envFile)
			}
		}
	}
	if !foundConfig {
		t.Fatalf("expected config invocation, found none")
	}
	if !foundPreHook {
		t.Fatalf("expected pre-deploy hook invocation, found none")
	}

	if _, err := rig.fs.Stat(finalAppEnvPath); err != nil {
		t.Fatalf("final app.env does not exist after promote: %v", err)
	}
}

func TestAppEnvReadErrorOtherThanNotExist(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	finalAppEnvPath := filepath.Join(rig.instDir, "app.env")
	efs := &errorFS{
		FS: rig.fs,
		failReadFile: map[string]error{
			finalAppEnvPath: errors.New("permission denied"),
		},
	}
	rig.deployer.FS = efs

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err == nil {
		t.Fatalf("expected error from Deploy, got nil (res: %+v)", res)
	}
	oe, ok := err.(*output.Error)
	if !ok || oe.Code != "E_APPENV_READ" {
		t.Fatalf("expected E_APPENV_READ, got %v", err)
	}

	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if st.FailedHash != "" {
		t.Fatalf("expected FailedHash to be empty, got %q", st.FailedHash)
	}
}

func TestPreDeployFailsNothingTouched(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	rig.compose.failHook["api:node dist/migrate.js"] = struct {
		res sys.Result
		err error
	}{
		res: sys.Result{ExitCode: 1, Stderr: []byte("migration syntax error")},
		err: fmt.Errorf("exit 1"),
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy returned unexpected go error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	if rig.compose.upCount > 0 {
		t.Fatalf("Up was called %d times; expected 0 because pre-deploy failed", rig.compose.upCount)
	}

	finalOverride := filepath.Join(rig.instDir, "compose.cvctl.yml")
	if _, err := rig.fs.Stat(finalOverride); err == nil {
		t.Fatalf("final override %s was promoted; expected nothing touched", finalOverride)
	}

	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if st.Status != state.StatusFailed {
		t.Fatalf("expected state FAILED, got %v", st.Status)
	}
	if st.FailedHash != want.Hash() {
		t.Fatalf("expected FailedHash %q, got %q", want.Hash(), st.FailedHash)
	}
}

func TestFailedHashNotRetried(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	st := &state.State{
		Instance:   "demo-dev",
		Status:     state.StatusFailed,
		FailedHash: want.Hash(),
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: false})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed without retry, got %v", res.Status)
	}

	if len(rig.repo.checkouts) > 0 {
		t.Fatalf("repo.Checkout called on failed hash with !Force: %v", rig.repo.checkouts)
	}
	if len(rig.compose.calls) > 0 {
		t.Fatalf("compose called on failed hash with !Force: %v", rig.compose.calls)
	}
}

func TestForceRetriesFailed(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	st := &state.State{
		Instance:   "demo-dev",
		Status:     state.StatusFailed,
		FailedHash: want.Hash(),
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: true})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected StatusOK with Force: true, got %v", res.Status)
	}
	if len(rig.compose.calls) == 0 {
		t.Fatalf("expected compose calls with Force: true, got none")
	}
}

func TestHealthFailsRollsBack(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	want := defaultDesired()

	healthCount := 0
	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		healthCount++
		if healthCount == 1 {
			return fmt.Errorf("health timeout on :3000/health")
		}
		return nil
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected deploy status FAILED after health failure, got %v", res.Status)
	}

	if rig.compose.upCount != 2 {
		t.Fatalf("expected compose.Up to run 2 times (deploy + rollback), got %d", rig.compose.upCount)
	}

	foundOldCheckout := false
	for _, c := range rig.repo.checkouts {
		if c == oldCommit {
			foundOldCheckout = true
			break
		}
	}
	if !foundOldCheckout {
		t.Fatalf("rollback did not checkout previous commit %s: checkouts = %v", oldCommit, rig.repo.checkouts)
	}

	stAfter, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if stAfter.Current == nil || stAfter.Current.InfraCommit != oldCommit {
		t.Fatalf("expected Current to remain oldCommit, got %v", stAfter.Current)
	}
	if stAfter.FailedHash != want.Hash() {
		t.Fatalf("expected FailedHash to be set to failed want hash, got %q", stAfter.FailedHash)
	}
	if stAfter.RollbackFailed {
		t.Fatalf("expected RollbackFailed to be false when rollback succeeds")
	}
}

func TestRollbackFailsMarksRollbackFailed(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	want := defaultDesired()

	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		return fmt.Errorf("persistent health failure")
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	stAfter, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if !stAfter.RollbackFailed {
		t.Fatalf("expected RollbackFailed to be true when rollback health fails")
	}
}

func TestBlockedOnMissingPrompt(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")

	specWithPrompt := `vars:
  DATABASE_URL: { source: service, service: main-db }
  API_SECRET:   { source: prompt, secret: true }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(specWithPrompt), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusBlocked {
		t.Fatalf("expected StatusBlocked, got %v (detail: %s)", res.Status, res.Detail)
	}
	if len(res.Missing) == 0 || res.Missing[0] != "API_SECRET" {
		t.Fatalf("expected Missing [API_SECRET], got %v", res.Missing)
	}

	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "pull") {
			t.Fatalf("pull was called while blocked: %v", c)
		}
	}

	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if st.Status != state.StatusBlocked {
		t.Fatalf("expected state BLOCKED, got %v", st.Status)
	}
	if st.FailedHash != "" {
		t.Fatalf("expected FailedHash to be empty on blocked, got %q", st.FailedHash)
	}

	var warnMsg *notify.Message
	for _, m := range rig.notifier.messages {
		if m.Level == "warn" {
			warnMsg = &m
			break
		}
	}
	if warnMsg == nil {
		t.Fatalf("expected warn notification message, got %v", rig.notifier.messages)
	}
	if !strings.Contains(warnMsg.Text, "cvctl set-env demo-dev API_SECRET") {
		t.Fatalf("expected warn text to contain cvctl set-env command, got %q", warnMsg.Text)
	}
}

func TestCheckoutGuardRestoresOnBlocked(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current: &state.Desired{
			InfraCommit: oldCommit,
		},
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	specWithPrompt := `vars:
  DATABASE_URL: { source: service, service: main-db }
  MISSING_PROMPT: { source: prompt }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(specWithPrompt), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusBlocked {
		t.Fatalf("expected StatusBlocked, got %v", res.Status)
	}

	if len(rig.repo.checkouts) < 2 {
		t.Fatalf("expected at least 2 checkouts, got %v", rig.repo.checkouts)
	}
	lastCheckout := rig.repo.checkouts[len(rig.repo.checkouts)-1]
	if lastCheckout != oldCommit {
		t.Fatalf("expected checkout to be restored to %s, got %s (checkouts: %v)", oldCommit, lastCheckout, rig.repo.checkouts)
	}
}

func TestCheckoutGuardRestoresOnErrorAfterCheckout(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current: &state.Desired{
			InfraCommit: oldCommit,
		},
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte("invalid: yaml: ["), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	want := defaultDesired()
	res, _ := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	if len(rig.repo.checkouts) < 2 {
		t.Fatalf("expected at least 2 checkouts, got %v", rig.repo.checkouts)
	}
	lastCheckout := rig.repo.checkouts[len(rig.repo.checkouts)-1]
	if lastCheckout != oldCommit {
		t.Fatalf("expected checkout to be restored to %s, got %s", oldCommit, lastCheckout)
	}
}

func TestCheckoutGuardAppendsError(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current: &state.Desired{
			InfraCommit: oldCommit,
		},
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	rig.repo.failAt[oldCommit] = errors.New("cannot restore old commit")

	specWithPrompt := `vars:
  DATABASE_URL: { source: service, service: main-db }
  MISSING_PROMPT: { source: prompt }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(specWithPrompt), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if !strings.Contains(res.Detail, "restore checkout failed: cannot restore old commit") {
		t.Fatalf("expected Detail to include checkout restore failure, got %q", res.Detail)
	}
}

func TestTransientErrorsDoNotSetFailedHash(t *testing.T) {
	t.Run("PullFailure", func(t *testing.T) {
		rig := setupTestRig(t, "dev", "demo-dev")
		want := defaultDesired()
		rig.compose.failPull = errors.New("pull network timeout")

		res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
		if err != nil {
			t.Fatalf("Deploy error: %v", err)
		}
		if res.Status != state.StatusFailed {
			t.Fatalf("expected StatusFailed, got %v", res.Status)
		}
		if !strings.HasSuffix(res.Detail, "will retry next cycle") {
			t.Fatalf("expected Detail to end with 'will retry next cycle', got %q", res.Detail)
		}
		st, _ := rig.store.Load("demo-dev")
		if st.FailedHash != "" {
			t.Fatalf("expected FailedHash to NOT be set, got %q", st.FailedHash)
		}
	})

	t.Run("UpDataFailure", func(t *testing.T) {
		rig := setupTestRig(t, "dev", "demo-dev")
		want := defaultDesired()
		rig.compose.failUpData = errors.New("data service failed to start")

		res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
		if err != nil {
			t.Fatalf("Deploy error: %v", err)
		}
		if res.Status != state.StatusFailed {
			t.Fatalf("expected StatusFailed, got %v", res.Status)
		}
		if !strings.HasSuffix(res.Detail, "will retry next cycle") {
			t.Fatalf("expected Detail to end with 'will retry next cycle', got %q", res.Detail)
		}
		st, _ := rig.store.Load("demo-dev")
		if st.FailedHash != "" {
			t.Fatalf("expected FailedHash to NOT be set, got %q", st.FailedHash)
		}
	})

	t.Run("RestorePointFailureProd", func(t *testing.T) {
		rig := setupTestRig(t, "prod", "demo-prod")
		want := defaultDesired()
		rig.restore.fail = errors.New("wal-g snapshot failure")

		res, err := rig.deployer.Deploy(context.Background(), "demo-prod", want, Options{Actor: "agent"})
		if err != nil {
			t.Fatalf("Deploy error: %v", err)
		}
		if res.Status != state.StatusFailed {
			t.Fatalf("expected StatusFailed, got %v", res.Status)
		}
		if !strings.HasSuffix(res.Detail, "will retry next cycle") {
			t.Fatalf("expected Detail to end with 'will retry next cycle', got %q", res.Detail)
		}
		st, _ := rig.store.Load("demo-prod")
		if st.FailedHash != "" {
			t.Fatalf("expected FailedHash to NOT be set, got %q", st.FailedHash)
		}
	})
}

func TestPromoteOrderAndFailure(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current: &state.Desired{
			InfraCommit: oldCommit,
		},
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	want := defaultDesired()

	finalRoutePath := filepath.Join(rig.deployer.traefikDir(), "demo-dev.yml")
	efs := &errorFS{
		FS: rig.fs,
		failWriteFile: map[string]error{
			finalRoutePath: errors.New("disk full on traefik dir"),
		},
	}
	rig.deployer.FS = efs

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	stAfter, _ := rig.store.Load("demo-dev")
	if stAfter.FailedHash != want.Hash() {
		t.Fatalf("expected FailedHash to be set to want.Hash(), got %q", stAfter.FailedHash)
	}

	foundErrorNotif := false
	for _, m := range rig.notifier.messages {
		if m.Level == "error" && strings.Contains(m.Title, "demo-dev deploy FAILED") {
			foundErrorNotif = true
			break
		}
	}
	if !foundErrorNotif {
		t.Fatalf("expected error notification, got %v", rig.notifier.messages)
	}

	lastCheckout := rig.repo.checkouts[len(rig.repo.checkouts)-1]
	if lastCheckout != oldCommit {
		t.Fatalf("expected checkout restored to %s, got %s", oldCommit, lastCheckout)
	}
}

func TestDeployingStateSavedBeforePull(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	pullCalled := false
	rig.deployer.Compose = func(inst, override, envFile string) ComposeAPI {
		b := &boundCompose{fake: rig.compose, override: override, envFile: envFile}
		return &pullCheckingCompose{
			boundCompose: b,
			onPull: func() {
				pullCalled = true
				st, err := rig.store.Load("demo-dev")
				if err != nil {
					t.Errorf("store.Load in Pull: %v", err)
				}
				if st.Status != state.StatusDeploying {
					t.Errorf("expected StatusDeploying before Pull, got %v", st.Status)
				}
			},
		}
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected StatusOK, got %v", res.Status)
	}
	if !pullCalled {
		t.Fatalf("Pull was not called")
	}
}

func TestStoreSaveFailurePropagated(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	stateFile := filepath.Join(rig.tempDir, "state", "demo-dev", "state.json")
	efs := &errorFS{
		FS: rig.fs,
		failWriteFile: map[string]error{
			stateFile: errors.New("disk I/O error on state save"),
		},
	}
	rig.store = state.NewStore(efs, filepath.Join(rig.tempDir, "state"))
	rig.deployer.Store = rig.store

	_, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err == nil {
		t.Fatalf("expected Store.Save error to be propagated, got nil")
	}
}

func TestPausedSkips(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	st := &state.State{
		Instance:     "demo-dev",
		Status:       state.StatusPaused,
		PausedReason: "operator paused",
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: false})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusPaused {
		t.Fatalf("expected StatusPaused, got %v", res.Status)
	}
	if len(rig.compose.calls) > 0 {
		t.Fatalf("compose calls executed while paused: %v", rig.compose.calls)
	}
	if len(rig.repo.checkouts) > 0 {
		t.Fatalf("repo checkouts executed while paused: %v", rig.repo.checkouts)
	}
}

func TestManualRollbackSetsPausedWantHash(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	currCommit := "1111111111111111111111111111111111111111"
	oldCommit := "0000000000000000000000000000000000000000"
	currDesired := state.Desired{InfraCommit: currCommit, Digests: map[string]string{"api": "sha256:111"}}
	oldDesired := state.Desired{InfraCommit: oldCommit, Digests: map[string]string{"api": "sha256:000"}}

	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &currDesired,
		Previous: &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "cli:admin")
	if err != nil {
		t.Fatalf("Rollback error: %v", err)
	}
	if res.Status != state.StatusPaused {
		t.Fatalf("expected StatusPaused, got %v", res.Status)
	}

	stAfter, _ := rig.store.Load("demo-dev")
	if stAfter.PausedWantHash != currDesired.Hash() {
		t.Fatalf("expected PausedWantHash to be currDesired hash %q, got %q", currDesired.Hash(), stAfter.PausedWantHash)
	}
}

func TestPausedSkipsWhenSameWantHash(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	st := &state.State{
		Instance:       "demo-dev",
		Status:         state.StatusPaused,
		PausedReason:   "manual rollback",
		PausedWantHash: want.Hash(),
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: false})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusPaused {
		t.Fatalf("expected StatusPaused, got %v", res.Status)
	}
}

func TestPausedSkipsWhenPausedWantHashEmpty(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	st := &state.State{
		Instance:       "demo-dev",
		Status:         state.StatusPaused,
		PausedReason:   "manual pause",
		PausedWantHash: "",
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: false})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusPaused {
		t.Fatalf("expected StatusPaused, got %v", res.Status)
	}
}

func TestPausedAutoResumesOnNewWant(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldHash := "some-other-hash-12345"

	st := &state.State{
		Instance:       "demo-dev",
		Status:         state.StatusPaused,
		PausedReason:   "manual rollback",
		PausedWantHash: oldHash,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: false})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected StatusOK on auto-resume, got %v", res.Status)
	}

	events, err := rig.store.History("demo-dev", 10)
	if err != nil {
		t.Fatalf("store.History: %v", err)
	}
	foundResume := false
	for _, ev := range events {
		if ev.Kind == "resume" {
			foundResume = true
			break
		}
	}
	if !foundResume {
		t.Fatalf("expected 'resume' event in history, got %v", events)
	}
}

func TestHookOutputRedacted(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")

	secretVal := "supersecret123456"
	appEnvPath := filepath.Join(rig.instDir, "app.env")
	if err := rig.fs.WriteFile(appEnvPath, []byte("APP_SECRET="+secretVal+"\n"), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write app.env: %v", err)
	}

	specWithSecret := `vars:
  DATABASE_URL: { source: service, service: main-db }
  APP_SECRET:   { source: prompt, secret: true }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(specWithSecret), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	hookOut := fmt.Sprintf("Error connecting with credentials: password=%s to database", secretVal)
	rig.compose.failHook["api:node dist/migrate.js"] = struct {
		res sys.Result
		err error
	}{
		res: sys.Result{ExitCode: 1, Stderr: []byte(hookOut)},
		err: fmt.Errorf("exit status 1"),
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	if strings.Contains(res.Detail, secretVal) {
		t.Fatalf("res.Detail contains unredacted secret: %s", res.Detail)
	}
	if !strings.Contains(res.Detail, "***") {
		t.Fatalf("res.Detail does not contain redacted '***': %s", res.Detail)
	}

	for _, m := range rig.notifier.messages {
		if strings.Contains(m.Text, secretVal) {
			t.Fatalf("notification contains unredacted secret: %s", m.Text)
		}
	}

	events, err := rig.store.History("demo-dev", 10)
	if err != nil {
		t.Fatalf("store.History: %v", err)
	}
	for _, ev := range events {
		if strings.Contains(ev.Detail, secretVal) {
			t.Fatalf("history event detail contains unredacted secret: %s", ev.Detail)
		}
	}
}

func TestRollbackFailureNotificationAndEvent(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current: &state.Desired{
			InfraCommit: oldCommit,
		},
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	want := defaultDesired()

	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		return errors.New("fatal container crash")
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	foundRollbackFailedNotif := false
	for _, m := range rig.notifier.messages {
		if m.Title == "demo-dev ROLLBACK FAILED" {
			foundRollbackFailedNotif = true
			if !strings.Contains(m.Text, "fatal container crash") {
				t.Fatalf("expected notification text to contain rollback error, got %q", m.Text)
			}
			break
		}
	}
	if !foundRollbackFailedNotif {
		t.Fatalf("expected 'demo-dev ROLLBACK FAILED' notification, got %v", rig.notifier.messages)
	}

	events, err := rig.store.History("demo-dev", 10)
	if err != nil {
		t.Fatalf("store.History: %v", err)
	}
	foundRollbackFailedEvent := false
	for _, ev := range events {
		if ev.Kind == "rollback" && ev.Result == "rollback_failed" {
			foundRollbackFailedEvent = true
			break
		}
	}
	if !foundRollbackFailedEvent {
		t.Fatalf("expected event with Kind 'rollback' and Result 'rollback_failed', got %+v", events)
	}
}

func TestDeployMisconfigured(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	t.Run("NilComposeDeploy", func(t *testing.T) {
		d := *rig.deployer
		d.Compose = nil
		_, err := d.Deploy(context.Background(), "demo-dev", want, Options{})
		var oe *output.Error
		if err == nil || !errors.As(err, &oe) || oe.Code != "E_DEPLOY_MISCONFIGURED" {
			t.Fatalf("expected E_DEPLOY_MISCONFIGURED, got %v", err)
		}
	})

	t.Run("NilHealthDeploy", func(t *testing.T) {
		d := *rig.deployer
		d.Health = nil
		_, err := d.Deploy(context.Background(), "demo-dev", want, Options{})
		var oe *output.Error
		if err == nil || !errors.As(err, &oe) || oe.Code != "E_DEPLOY_MISCONFIGURED" {
			t.Fatalf("expected E_DEPLOY_MISCONFIGURED, got %v", err)
		}
	})

	t.Run("NilComposeRollback", func(t *testing.T) {
		d := *rig.deployer
		d.Compose = nil
		_, err := d.Rollback(context.Background(), "demo-dev", nil, "agent")
		var oe *output.Error
		if err == nil || !errors.As(err, &oe) || oe.Code != "E_DEPLOY_MISCONFIGURED" {
			t.Fatalf("expected E_DEPLOY_MISCONFIGURED, got %v", err)
		}
	})

	t.Run("NilHealthRollback", func(t *testing.T) {
		d := *rig.deployer
		d.Health = nil
		_, err := d.Rollback(context.Background(), "demo-dev", nil, "agent")
		var oe *output.Error
		if err == nil || !errors.As(err, &oe) || oe.Code != "E_DEPLOY_MISCONFIGURED" {
			t.Fatalf("expected E_DEPLOY_MISCONFIGURED, got %v", err)
		}
	})
}

func TestRemovedDataServiceKeepsVolume(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	_, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}

	rig.compose.mu.Lock()
	rig.compose.calls = nil
	rig.compose.invocations = nil
	rig.compose.mu.Unlock()

	noDataCompose := `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }
services:
  api:
    image: ghcr.io/org/project-api
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy: { resources: { limits: { memory: 512m } } }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "compose.yml"), []byte(noDataCompose), 0o644); err != nil {
		t.Fatalf("rewrite compose.yml: %v", err)
	}
	noDataSpec := `vars: {}`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(noDataSpec), 0o644); err != nil {
		t.Fatalf("rewrite env.spec.yml: %v", err)
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: true, Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy without data service error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected status OK, got %v", res.Status)
	}

	upCalls := 0
	for _, c := range rig.compose.calls {
		if c == "up" || c == "up:--remove-orphans" {
			upCalls++
		}
	}
	if upCalls == 0 {
		t.Fatalf("expected Up to be called on second deploy")
	}

	for _, c := range rig.compose.calls {
		if strings.Contains(c, "-v") || strings.Contains(c, "volume rm") || strings.Contains(c, "down") {
			t.Fatalf("prohibited destructive volume command executed: %s", c)
		}
	}
}

func TestManualRollbackPauses(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	currCommit := "1111111111111111111111111111111111111111"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	currDesired := state.Desired{
		InfraCommit: currCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000001",
		},
	}

	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &currDesired,
		Previous: &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "cli:admin")
	if err != nil {
		t.Fatalf("Rollback error: %v", err)
	}
	if res.Status != state.StatusPaused {
		t.Fatalf("expected status PAUSED, got %v", res.Status)
	}

	stAfter, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatalf("store.Load: %v", err)
	}
	if stAfter.Status != state.StatusPaused {
		t.Fatalf("expected state status PAUSED, got %v", stAfter.Status)
	}
	expectedReason := "manual rollback; resume with cvctl resume demo-dev"
	if !strings.Contains(stAfter.PausedReason, "manual rollback; resume with cvctl resume") {
		t.Fatalf("expected PausedReason %q, got %q", expectedReason, stAfter.PausedReason)
	}
	if stAfter.Current == nil || stAfter.Current.InfraCommit != oldCommit {
		t.Fatalf("expected Current to be rolled back to %s, got %v", oldCommit, stAfter.Current)
	}
	if stAfter.Previous == nil || stAfter.Previous.InfraCommit != currCommit {
		t.Fatalf("expected Previous to be %s, got %v", currCommit, stAfter.Previous)
	}
}

func TestProdCreatesRestorePoint(t *testing.T) {
	rig := setupTestRig(t, "prod", "demo-prod")
	want := defaultDesired()

	res, err := rig.deployer.Deploy(context.Background(), "demo-prod", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected StatusOK, got %v", res.Status)
	}

	if len(rig.restore.calls) == 0 {
		t.Fatalf("expected restore point to be created in prod, got none")
	}
	expectedPrefix := "demo-prod:deploy-"
	if !strings.HasPrefix(rig.restore.calls[0], expectedPrefix) {
		t.Fatalf("expected restore call with prefix %s, got %s", expectedPrefix, rig.restore.calls[0])
	}
}

func TestDevNoRestorePoint(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected StatusOK, got %v", res.Status)
	}

	if len(rig.restore.calls) > 0 {
		t.Fatalf("restore point created in dev environment: %v", rig.restore.calls)
	}
}

func TestDeployFailThenRollbackPausedSkipsRetry(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	initialCommit := "0000000000000000000000000000000000000000"
	initialDesired := state.Desired{
		InfraCommit: initialCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &initialDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	// 1. Deploy Y fails (pre-deploy hook fails)
	wantY := state.Desired{
		InfraCommit: "1111111111111111111111111111111111111111",
		Digests: map[string]string{
			"api": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
		},
	}
	rig.compose.failHook["api:node dist/migrate.js"] = struct {
		res sys.Result
		err error
	}{
		res: sys.Result{ExitCode: 1, Stderr: []byte("migration syntax error")},
		err: fmt.Errorf("exit status 1"),
	}

	resY, err := rig.deployer.Deploy(context.Background(), "demo-dev", wantY, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy Y error: %v", err)
	}
	if resY.Status != state.StatusFailed {
		t.Fatalf("expected deploy Y status FAILED, got %v", resY.Status)
	}

	stAfterY, _ := rig.store.Load("demo-dev")
	if stAfterY.FailedHash != wantY.Hash() {
		t.Fatalf("expected FailedHash %q, got %q", wantY.Hash(), stAfterY.FailedHash)
	}

	// 2. Operator runs Rollback
	rbRes, err := rig.deployer.Rollback(context.Background(), "demo-dev", &initialDesired, "cli:admin")
	if err != nil {
		t.Fatalf("Rollback error: %v", err)
	}
	if rbRes.Status != state.StatusPaused {
		t.Fatalf("expected Rollback status PAUSED, got %v", rbRes.Status)
	}

	stAfterRb, _ := rig.store.Load("demo-dev")
	if stAfterRb.FailedHash != wantY.Hash() {
		t.Fatalf("expected FailedHash NOT to be cleared, got %q", stAfterRb.FailedHash)
	}
	if stAfterRb.PausedWantHash != wantY.Hash() {
		t.Fatalf("expected PausedWantHash to be failed hash %q, got %q", wantY.Hash(), stAfterRb.PausedWantHash)
	}

	// 3. Next Deploy(want=Y) stays PAUSED and runs no compose calls
	rig.compose.mu.Lock()
	rig.compose.calls = nil
	rig.compose.mu.Unlock()

	resRetryY, err := rig.deployer.Deploy(context.Background(), "demo-dev", wantY, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy retry Y error: %v", err)
	}
	if resRetryY.Status != state.StatusPaused {
		t.Fatalf("expected retry Y to stay PAUSED, got %v", resRetryY.Status)
	}
	if len(rig.compose.calls) > 0 {
		t.Fatalf("expected no compose calls when retry Y is skipped, got %v", rig.compose.calls)
	}

	stAfterRetryY, _ := rig.store.Load("demo-dev")
	if stAfterRetryY.Status != state.StatusPaused {
		t.Fatalf("expected status to stay PAUSED, got %v", stAfterRetryY.Status)
	}

	// 4. A new want Z auto-resumes
	wantZ := state.Desired{
		InfraCommit: "2222222222222222222222222222222222222222",
		Digests: map[string]string{
			"api": "sha256:2222222222222222222222222222222222222222222222222222222222222222",
		},
	}
	delete(rig.compose.failHook, "api:node dist/migrate.js")

	resZ, err := rig.deployer.Deploy(context.Background(), "demo-dev", wantZ, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy Z error: %v", err)
	}
	if resZ.Status != state.StatusOK {
		t.Fatalf("expected want Z to auto-resume with StatusOK, got %v", resZ.Status)
	}

	stAfterZ, _ := rig.store.Load("demo-dev")
	if stAfterZ.Status != state.StatusOK {
		t.Fatalf("expected final status StatusOK, got %v", stAfterZ.Status)
	}
	if stAfterZ.Current == nil || stAfterZ.Current.Hash() != wantZ.Hash() {
		t.Fatalf("expected Current hash to be wantZ, got %+v", stAfterZ.Current)
	}
}

func TestRollbackFailureStoreSaveErrorStillNotifies(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	currCommit := "1111111111111111111111111111111111111111"
	oldDesired := state.Desired{InfraCommit: oldCommit, Digests: map[string]string{"api": "sha256:000"}}
	currDesired := state.Desired{InfraCommit: currCommit, Digests: map[string]string{"api": "sha256:111"}}

	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &currDesired,
		Previous: &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	// Make health check fail so executeSwap in Rollback fails
	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		return errors.New("swap healthcheck failed")
	}

	// Make store save fail on state.json
	stateDir := filepath.Join(rig.tempDir, "state")
	statePath := filepath.Join(stateDir, "demo-dev", "state.json")
	efs := &errorFS{
		FS: rig.fs,
		failWriteFile: map[string]error{
			statePath: errors.New("read-only state filesystem"),
		},
	}
	rig.deployer.FS = efs
	rig.deployer.Store = state.NewStore(efs, stateDir)

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "cli:admin")
	if err == nil {
		t.Fatalf("expected error from failed Store.Save, got nil")
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected res.Status FAILED, got %v", res.Status)
	}

	foundRollbackFailedNotif := false
	for _, m := range rig.notifier.messages {
		if m.Title == "demo-dev ROLLBACK FAILED" {
			foundRollbackFailedNotif = true
			break
		}
	}
	if !foundRollbackFailedNotif {
		t.Fatalf("expected 'demo-dev ROLLBACK FAILED' notification to be sent even when Store.Save fails, got %v", rig.notifier.messages)
	}
}

func TestDeployRenderErrorFailPermanent(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	rig.deployer.RenderOverride = func(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o render.Options) ([]byte, error) {
		return nil, errors.New("synthetic render override error")
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}
	if !strings.Contains(res.Detail, "synthetic render override error") {
		t.Fatalf("expected Detail to contain render error, got %q", res.Detail)
	}

	st, _ := rig.store.Load("demo-dev")
	if st.FailedHash != want.Hash() {
		t.Fatalf("expected FailedHash %q, got %q", want.Hash(), st.FailedHash)
	}
	if st.Status != state.StatusFailed {
		t.Fatalf("expected store status FAILED, got %v", st.Status)
	}

	foundErrorNotif := false
	for _, m := range rig.notifier.messages {
		if m.Level == "error" && strings.Contains(m.Text, "synthetic render override error") {
			foundErrorNotif = true
			break
		}
	}
	if !foundErrorNotif {
		t.Fatalf("expected error notification with render error, got %v", rig.notifier.messages)
	}

	events, _ := rig.store.History("demo-dev", 10)
	foundFailedEvent := false
	for _, ev := range events {
		if ev.Kind == "failed" && strings.Contains(ev.Detail, "synthetic render override error") {
			foundFailedEvent = true
			break
		}
	}
	if !foundFailedEvent {
		t.Fatalf("expected failed event with render error in history, got %v", events)
	}
}

func TestDeployNilFactoryAfterDeployingFailPermanent(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	// Compose returns nil on staging
	rig.deployer.Compose = func(inst, override, envFile string) ComposeAPI {
		return nil
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}
	if !strings.Contains(res.Detail, "Compose returned nil") {
		t.Fatalf("expected Detail to mention Compose returned nil, got %q", res.Detail)
	}

	st, _ := rig.store.Load("demo-dev")
	if st.FailedHash != want.Hash() {
		t.Fatalf("expected FailedHash %q, got %q", want.Hash(), st.FailedHash)
	}
	if st.Status != state.StatusFailed {
		t.Fatalf("expected store status FAILED, got %v", st.Status)
	}

	foundErrorNotif := false
	for _, m := range rig.notifier.messages {
		if m.Level == "error" && strings.Contains(m.Text, "Compose returned nil") {
			foundErrorNotif = true
			break
		}
	}
	if !foundErrorNotif {
		t.Fatalf("expected error notification, got %v", rig.notifier.messages)
	}
}

type failOnceFS struct {
	sys.FS
	failPath string
	failed   bool
}

func (f *failOnceFS) WriteFile(path string, data []byte, mode os.FileMode, owner sys.Owner) error {
	if path == f.failPath && !f.failed {
		f.failed = true
		return errors.New("override disk full")
	}
	return f.FS.WriteFile(path, data, mode, owner)
}

func TestPromoteFailureOverrideWriteRestoresPreviousState(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}

	appEnvPath := filepath.Join(rig.instDir, "app.env")
	prevAppEnv := "DATABASE_URL=postgres://main_db:secret@demo-dev-main-db:5432/main_db\nOLD_VAR=previous_value\n"
	if err := rig.fs.WriteFile(appEnvPath, []byte(prevAppEnv), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write initial app.env: %v", err)
	}

	routePath := filepath.Join(rig.deployer.traefikDir(), "demo-dev.yml")
	cat, err := kinds.Load()
	if err != nil {
		t.Fatalf("kinds.Load: %v", err)
	}
	p, err := contract.Load(rig.infraDir, cat)
	if err != nil {
		t.Fatalf("contract.Load: %v", err)
	}
	dec, err := instance.Read(filepath.Join(rig.instDir, "cvctl.env"), p)
	if err != nil {
		t.Fatalf("instance.Read: %v", err)
	}
	for k, v := range oldDesired.Digests {
		dec.Digests[k] = v
	}
	prevRoute, err := render.Traefik(p, dec, render.Options{
		InstanceDir: rig.instDir,
		Edge:        "edge",
		AppEnvPath:  appEnvPath,
	})
	if err != nil {
		t.Fatalf("render.Traefik: %v", err)
	}
	if err := rig.fs.WriteFile(routePath, prevRoute, 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write initial route: %v", err)
	}

	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	finalOverridePath := filepath.Join(rig.instDir, "compose.cvctl.yml")
	fofs := &failOnceFS{
		FS:       rig.fs,
		failPath: finalOverridePath,
	}
	rig.deployer.FS = fofs

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}
	if !strings.Contains(res.Detail, "promote override failed: override disk full") {
		t.Fatalf("expected Detail to contain promote override failure, got %q", res.Detail)
	}
	if !strings.Contains(res.Detail, "restored to") {
		t.Fatalf("expected Detail to confirm restore succeeded, got %q", res.Detail)
	}

	stAfter, _ := rig.store.Load("demo-dev")
	if stAfter.FailedHash != want.Hash() {
		t.Fatalf("expected FailedHash to be set, got %q", stAfter.FailedHash)
	}
	if stAfter.Status != state.StatusFailed {
		t.Fatalf("expected store status FAILED, got %v", stAfter.Status)
	}

	// Verify app.env was restored to previous content
	currAppEnvBytes, err := rig.fs.ReadFile(appEnvPath)
	if err != nil {
		t.Fatalf("read restored app.env: %v", err)
	}
	if !strings.Contains(string(currAppEnvBytes), "OLD_VAR=previous_value") {
		t.Fatalf("expected app.env to be restored to previous content containing OLD_VAR=previous_value, got %q", string(currAppEnvBytes))
	}

	// Verify route was restored exactly
	currRouteBytes, err := rig.fs.ReadFile(routePath)
	if err != nil {
		t.Fatalf("read restored route: %v", err)
	}
	if !bytes.Equal(currRouteBytes, prevRoute) {
		t.Fatalf("expected route file content to equal previous route content exactly:\ngot:\n%s\nwant:\n%s", string(currRouteBytes), string(prevRoute))
	}

	// Verify executeSwap ran (e.g. compose fake saw an Up bound to the final override for the previous digests)
	var foundSwapUp bool
	for _, inv := range rig.compose.invocations {
		if inv.op == "up" && inv.override == finalOverridePath {
			foundSwapUp = true
			break
		}
	}
	if !foundSwapUp {
		t.Fatalf("expected executeSwap to run Compose.Up on %s", finalOverridePath)
	}

	currOverrideBytes, err := rig.fs.ReadFile(finalOverridePath)
	if err != nil {
		t.Fatalf("read restored override: %v", err)
	}
	if !strings.Contains(string(currOverrideBytes), oldDesired.Digests["api"]) {
		t.Fatalf("expected restored override to contain old digest %s, got %s", oldDesired.Digests["api"], string(currOverrideBytes))
	}

	// Verify checkout restored to oldCommit
	lastCheckout := rig.repo.checkouts[len(rig.repo.checkouts)-1]
	if lastCheckout != oldCommit {
		t.Fatalf("expected checkout restored to %s, got %s", oldCommit, lastCheckout)
	}
}

type failCountingFS struct {
	sys.FS
	targetPath  string
	writeCount  int
	failOnCount int
}

func (f *failCountingFS) WriteFile(path string, data []byte, mode os.FileMode, o sys.Owner) error {
	if path == f.targetPath {
		f.writeCount++
		if f.writeCount == f.failOnCount {
			return errors.New("state write error on auto-rollback save")
		}
	}
	return f.FS.WriteFile(path, data, mode, o)
}

func TestRollbackAppEnvReadErrorAbortsTouchingNothing(t *testing.T) {
	t.Run("permission error", func(t *testing.T) {
		rig := setupTestRig(t, "dev", "demo-dev")
		oldDesired := state.Desired{
			InfraCommit: "0000000000000000000000000000000000000000",
			Digests: map[string]string{
				"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
		}
		st := &state.State{
			Instance: "demo-dev",
			Status:   state.StatusOK,
			Current:  &oldDesired,
			Previous: &oldDesired,
		}
		if err := rig.store.Save(st); err != nil {
			t.Fatalf("store.Save: %v", err)
		}

		appEnvPath := filepath.Join(rig.instDir, "app.env")
		efs := &errorFS{
			FS: rig.fs,
			failReadFile: map[string]error{
				appEnvPath: errors.New("permission denied"),
			},
		}
		rig.deployer.FS = efs

		_, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "operator")
		if err == nil {
			t.Fatalf("expected error from Rollback, got nil")
		}
		var outErr *output.Error
		if !errors.As(err, &outErr) || outErr.Code != "E_APPENV_READ" {
			t.Fatalf("expected E_APPENV_READ error, got %v", err)
		}
		if len(rig.repo.checkouts) > 0 {
			t.Fatalf("expected no checkouts, got %v", rig.repo.checkouts)
		}
		if rig.compose.upCount > 0 {
			t.Fatalf("expected no Up calls, got %d", rig.compose.upCount)
		}
		if len(rig.notifier.messages) > 0 {
			t.Fatalf("expected no notifications, got %v", rig.notifier.messages)
		}
		stAfter, _ := rig.store.Load("demo-dev")
		if stAfter.Status != state.StatusOK {
			t.Fatalf("expected store status unchanged, got %v", stAfter.Status)
		}
	})

	t.Run("parse error", func(t *testing.T) {
		rig := setupTestRig(t, "dev", "demo-dev")
		oldDesired := state.Desired{
			InfraCommit: "0000000000000000000000000000000000000000",
			Digests: map[string]string{
				"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
			},
		}
		st := &state.State{
			Instance: "demo-dev",
			Status:   state.StatusOK,
			Current:  &oldDesired,
			Previous: &oldDesired,
		}
		if err := rig.store.Save(st); err != nil {
			t.Fatalf("store.Save: %v", err)
		}

		appEnvPath := filepath.Join(rig.instDir, "app.env")
		if err := rig.fs.WriteFile(appEnvPath, []byte("FOO=\"unclosed quote\n"), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
			t.Fatalf("write invalid app.env: %v", err)
		}

		_, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "operator")
		if err == nil {
			t.Fatalf("expected error from Rollback, got nil")
		}
		var outErr *output.Error
		if !errors.As(err, &outErr) || outErr.Code != "E_APPENV_READ" {
			t.Fatalf("expected E_APPENV_READ error, got %v", err)
		}
		if len(rig.repo.checkouts) > 0 {
			t.Fatalf("expected no checkouts, got %v", rig.repo.checkouts)
		}
		if rig.compose.upCount > 0 {
			t.Fatalf("expected no Up calls, got %d", rig.compose.upCount)
		}
		if len(rig.notifier.messages) > 0 {
			t.Fatalf("expected no notifications, got %v", rig.notifier.messages)
		}
		stAfter, _ := rig.store.Load("demo-dev")
		if stAfter.Status != state.StatusOK {
			t.Fatalf("expected store status unchanged, got %v", stAfter.Status)
		}
	})
}

func TestExecuteSwapEmptyEnvAppEnvUntouched(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	target := &state.Desired{
		InfraCommit: "0000000000000000000000000000000000000000",
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	appEnvPath := filepath.Join(rig.instDir, "app.env")
	initialContent := "EXISTING_KEY=existing_val\n"
	if err := rig.fs.WriteFile(appEnvPath, []byte(initialContent), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write app.env: %v", err)
	}

	err := rig.deployer.executeSwap(context.Background(), "demo-dev", target, map[string]string{}, false)
	if err != nil {
		t.Fatalf("executeSwap: %v", err)
	}

	content, err := rig.fs.ReadFile(appEnvPath)
	if err != nil {
		t.Fatalf("read app.env: %v", err)
	}
	if string(content) != initialContent {
		t.Fatalf("expected app.env untouched (%q), got %q", initialContent, string(content))
	}
}

func TestPromoteFailureNoPriorAppEnvRemovesAppEnv(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	appEnvPath := filepath.Join(rig.instDir, "app.env")
	_ = rig.fs.Remove(appEnvPath)

	finalOverridePath := filepath.Join(rig.instDir, "compose.cvctl.yml")
	fofs := &failOnceFS{
		FS:       rig.fs,
		failPath: finalOverridePath,
	}
	rig.deployer.FS = fofs

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	_, err = rig.fs.ReadFile(appEnvPath)
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, filepath.SkipDir) && err == nil {
		t.Fatalf("expected app.env to be removed when absent prior to deploy, but it exists")
	}
}

func TestPromoteFailureWithPriorAppEnvRestoresPriorContent(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	appEnvPath := filepath.Join(rig.instDir, "app.env")
	priorContent := "PRIOR_KEY=prior_value_12345\nDATABASE_URL=postgres://main_db:secret@demo-dev-main-db:5432/main_db\n"
	if err := rig.fs.WriteFile(appEnvPath, []byte(priorContent), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write initial app.env: %v", err)
	}

	finalOverridePath := filepath.Join(rig.instDir, "compose.cvctl.yml")
	fofs := &failOnceFS{
		FS:       rig.fs,
		failPath: finalOverridePath,
	}
	rig.deployer.FS = fofs

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", res.Status)
	}

	currContent, err := rig.fs.ReadFile(appEnvPath)
	if err != nil {
		t.Fatalf("read restored app.env: %v", err)
	}
	if !strings.Contains(string(currContent), "PRIOR_KEY=prior_value_12345") {
		t.Fatalf("expected app.env to be restored to prior content, got %q", string(currContent))
	}
}

func TestHealthFailureRestoresPreDeployAppEnv(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	appEnvPath := filepath.Join(rig.instDir, "app.env")
	preDeployContent := "DATABASE_URL=postgres://main_db:secret@demo-dev-main-db:5432/main_db\nPRE_VAR=pre_value_12345\n"
	if err := rig.fs.WriteFile(appEnvPath, []byte(preDeployContent), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write initial app.env: %v", err)
	}

	envSpecContent := `vars:
  DATABASE_URL: { source: service, service: main-db }
  NEW_VAR: { source: fixed, value: "resolved_new_val" }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(envSpecContent), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	cvctlEnvPath := filepath.Join(rig.instDir, "cvctl.env")
	currCvctlEnv, _ := os.ReadFile(cvctlEnvPath)
	_ = os.WriteFile(cvctlEnvPath, append(currCvctlEnv, []byte("CVCTL_ENV_NEW_VAR=resolved_new_val\n")...), 0o644)

	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		if rig.compose.upCount == 1 {
			return errors.New("unhealthy container")
		}
		return nil
	}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected StatusFailed after health failure, got %v", res.Status)
	}

	restoredAppEnvBytes, err := rig.fs.ReadFile(appEnvPath)
	if err != nil {
		t.Fatalf("read app.env after auto-rollback: %v", err)
	}
	restoredStr := string(restoredAppEnvBytes)
	if !strings.Contains(restoredStr, "PRE_VAR=pre_value_12345") {
		t.Fatalf("expected restored app.env to contain pre-deploy variable, got %q", restoredStr)
	}
	if strings.Contains(restoredStr, "NEW_VAR=resolved_new_val") {
		t.Fatalf("expected restored app.env to NOT contain newly resolved variable, got %q", restoredStr)
	}
}

func TestAutoRollbackSuccessSaveFailureStillNotifies(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	oldCommit := "0000000000000000000000000000000000000000"
	oldDesired := state.Desired{
		InfraCommit: oldCommit,
		Digests: map[string]string{
			"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000",
		},
	}
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current:  &oldDesired,
	}
	if err := rig.store.Save(st); err != nil {
		t.Fatalf("store.Save: %v", err)
	}

	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		if rig.compose.upCount == 1 {
			return errors.New("health check failure")
		}
		return nil
	}

	stateDir := filepath.Join(rig.tempDir, "state")
	statePath := filepath.Join(stateDir, "demo-dev", "state.json")
	fcfs := &failCountingFS{
		FS:          rig.fs,
		targetPath:  statePath,
		failOnCount: 2, // 1st is DEPLOYING, 2nd is post-rollback save
	}
	rig.deployer.FS = fcfs
	rig.deployer.Store = state.NewStore(fcfs, stateDir)

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err == nil {
		t.Fatalf("expected error from Deploy when Save fails, got nil")
	}
	if !strings.Contains(err.Error(), "state write error on auto-rollback save") {
		t.Fatalf("expected error to contain state write error on auto-rollback save, got %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected res.Status = StatusFailed, got %v", res.Status)
	}

	foundFailNotif := false
	for _, m := range rig.notifier.messages {
		if m.Level == "error" && strings.Contains(m.Title, "demo-dev deploy FAILED") {
			foundFailNotif = true
			break
		}
	}
	if !foundFailNotif {
		t.Fatalf("expected deploy FAILED notification sent, got %v", rig.notifier.messages)
	}
}

func TestDeployAppEnvMalformedHidesSecrets(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	appEnvPath := filepath.Join(rig.instDir, "app.env")
	if err := rig.fs.WriteFile(appEnvPath, []byte("SECRET=\"topsecretvalue\n"), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write invalid app.env: %v", err)
	}

	_, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err == nil {
		t.Fatal("expected error from Deploy, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_APPENV_READ" {
		t.Fatalf("expected E_APPENV_READ error, got %v", err)
	}
	if strings.Contains(oe.Message, "topsecretvalue") {
		t.Errorf("error message leaked secret: %s", oe.Message)
	}
	if strings.Contains(oe.Hint, "topsecretvalue") {
		t.Errorf("error hint leaked secret: %s", oe.Hint)
	}
	expectedMsg := fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", appEnvPath)
	if oe.Message != expectedMsg {
		t.Errorf("expected message %q, got %q", expectedMsg, oe.Message)
	}
}

func TestDeployRejectsDisallowedImageAfterCheckout(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Allowed = []string{"ghcr.io/other"}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("expected FAILED, got %v", res.Status)
	}
	if !strings.Contains(res.Detail, "E_REGISTRY_NOT_ALLOWED") {
		t.Fatalf("expected E_REGISTRY_NOT_ALLOWED in detail, got %q", res.Detail)
	}
	if len(rig.compose.calls) != 0 {
		t.Fatalf("no compose call may run for a rejected image, got %v", rig.compose.calls)
	}
}

func TestDeployAcceptsAllowedImage(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Allowed = []string{"ghcr.io/org"}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("expected OK, got %v err=%v", res.Status, err)
	}
}

func TestDeployLockedAssumesCallerHoldsLock(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")

	unlock, err := rig.store.Lock("demo-dev")
	if err != nil {
		t.Fatalf("Lock: %v", err)
	}
	defer unlock()

	res, err := rig.deployer.DeployLocked(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("expected OK while caller holds lock, got %v err=%v", res.Status, err)
	}

	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Force: true}); err == nil {
		t.Fatal("Deploy must take the lock itself and fail while it is held")
	}
}

func withRandomSpec(t *testing.T, rig *testRig) {
	t.Helper()
	spec := `vars:
  DATABASE_URL: { source: service, service: main-db }
  JWT_SECRET: { source: random, length: 24 }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readEnvFile(t *testing.T, rig *testRig) map[string]string {
	t.Helper()
	raw, err := rig.fs.ReadFile(filepath.Join(rig.instDir, "app.env"))
	if err != nil {
		t.Fatalf("read app.env: %v", err)
	}
	vals, err := godotenv.Unmarshal(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	return vals
}

func TestGeneratedRandomPersistsWhenHookFails(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	withRandomSpec(t, rig)
	rig.compose.failHook["api:node dist/migrate.js"] = struct {
		res sys.Result
		err error
	}{res: sys.Result{ExitCode: 1, Stderr: []byte("boom")}, err: fmt.Errorf("exit 1")}

	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{})
	if err != nil || res.Status != state.StatusFailed {
		t.Fatalf("expected failed deploy, got %v %v", res.Status, err)
	}
	first := readEnvFile(t, rig)["JWT_SECRET"]
	if len(first) != 24 {
		t.Fatalf("expected persisted random, got %q", first)
	}
	delete(rig.compose.failHook, "api:node dist/migrate.js")
	if res, err = rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: true}); err != nil || res.Status != state.StatusOK {
		t.Fatalf("redeploy: %v %v", res.Status, err)
	}
	if got := readEnvFile(t, rig)["JWT_SECRET"]; got != first {
		t.Fatalf("random changed across deploys: %q vs %q", first, got)
	}
}

func TestGeneratedRandomSurvivesAutoRollback(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	withRandomSpec(t, rig)
	old := state.Desired{InfraCommit: "0000000000000000000000000000000000000000"}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &old}); err != nil {
		t.Fatal(err)
	}
	pre := "DATABASE_URL=postgres://x\nPRE_VAR=pre_value_12345\n"
	if err := rig.fs.WriteFile(filepath.Join(rig.instDir, "app.env"), []byte(pre), 0o640, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	var seen string
	rig.deployer.Health = func(ctx context.Context, inst, override string) error {
		if rig.compose.upCount == 1 {
			seen = readEnvFile(t, rig)["JWT_SECRET"]
			return errors.New("unhealthy")
		}
		return nil
	}
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err != nil || res.Status != state.StatusFailed {
		t.Fatalf("expected failed deploy, got %v %v", res.Status, err)
	}
	vals := readEnvFile(t, rig)
	if vals["PRE_VAR"] != "pre_value_12345" || seen == "" || vals["JWT_SECRET"] != seen {
		t.Fatalf("rollback env lost keys: %v (generated %q)", vals, seen)
	}
}

func TestIdenticalTransientFailureNotifiesOnce(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.compose.failPull = errors.New("pull network timeout")
	want := defaultDesired()
	for i := 0; i < 2; i++ {
		if res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{}); err != nil || res.Status != state.StatusFailed {
			t.Fatalf("run %d: %v %v", i, res.Status, err)
		}
	}
	if n := len(rig.notifier.messages); n != 1 {
		t.Fatalf("expected 1 notification, got %d", n)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.LastTransient == "" {
		t.Fatal("expected LastTransient to be stored")
	}

	rig.compose.failPull = nil
	if res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{}); err != nil || res.Status != state.StatusOK {
		t.Fatalf("success run: %v %v", res.Status, err)
	}
	st, _ = rig.store.Load("demo-dev")
	if st.LastTransient != "" {
		t.Fatalf("LastTransient not cleared: %q", st.LastTransient)
	}
}

func TestDeployAtCurrentRecoversBlockedOrFailed(t *testing.T) {
	for _, status := range []state.Status{state.StatusBlocked, state.StatusFailed} {
		rig := setupTestRig(t, "dev", "demo-dev")
		want := defaultDesired()
		cur := want
		if err := rig.store.Save(&state.State{
			Instance: "demo-dev", Status: status, Current: &cur,
			Blocked: []string{"X"}, FailedHash: "stale", LastTransient: "old",
		}); err != nil {
			t.Fatal(err)
		}
		res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{})
		if err != nil || res.Status != state.StatusOK {
			t.Fatalf("%s: %v %v", status, res.Status, err)
		}
		st, _ := rig.store.Load("demo-dev")
		if st.Status != state.StatusOK || st.Blocked != nil || st.FailedHash != "" || st.LastTransient != "" {
			t.Fatalf("%s: state not recovered: %+v", status, st)
		}
		hist, _ := rig.store.History("demo-dev", 0)
		if len(hist) != 1 || hist[0].Kind != "recovered" {
			t.Fatalf("%s: expected recovered event, got %+v", status, hist)
		}
		if rig.compose.upCount != 0 {
			t.Fatalf("%s: unexpected redeploy", status)
		}
	}
}

func TestDeploySuccessSaveFailureStillNotifies(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	stateDir := filepath.Join(rig.tempDir, "state")
	fcfs := &failCountingFS{
		FS:          rig.fs,
		targetPath:  filepath.Join(stateDir, "demo-dev", "state.json"),
		failOnCount: 2, // 1st is DEPLOYING, 2nd is the success save
	}
	rig.deployer.FS = fcfs
	rig.deployer.Store = state.NewStore(fcfs, stateDir)

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err == nil {
		t.Fatal("expected error when the success save fails")
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected OK result, got %v", res.Status)
	}
	found := false
	for _, m := range rig.notifier.messages {
		if m.Level == "info" && strings.Contains(m.Title, "deploy OK") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected deploy OK notification, got %v", rig.notifier.messages)
	}
}
