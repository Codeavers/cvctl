package deploy

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

type testRig struct {
	deployer *Deployer
	repo     *fakeDeployRepo
	compose  *fakeCompose
	notifier *fakeNotifier
	restore  *fakeRestore
	store    *state.Store
	fs       *systest.FS
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

type fakeCompose struct {
	mu         sync.Mutex
	calls      []string
	failConfig error
	failPull   error
	failUpData error
	failHook   map[string]struct {
		res sys.Result
		err error
	}
	failUp      error
	failLogs    error
	hookOutputs map[string]sys.Result
	upCount     int
}

func (c *fakeCompose) Config(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "config")
	return c.failConfig
}

func (c *fakeCompose) Pull(_ context.Context, services ...string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "pull:"+strings.Join(services, ","))
	return c.failPull
}

func (c *fakeCompose) UpData(_ context.Context, services []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, "updata:"+strings.Join(services, ","))
	return c.failUpData
}

func (c *fakeCompose) RunHook(_ context.Context, service, script string) (sys.Result, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := service + ":" + script
	c.calls = append(c.calls, "runhook:"+key)
	if f, ok := c.failHook[key]; ok {
		return f.res, f.err
	}
	if out, ok := c.hookOutputs[key]; ok {
		return out, nil
	}
	return sys.Result{ExitCode: 0, Stdout: []byte("hook ok")}, nil
}

func (c *fakeCompose) Up(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.upCount++
	c.calls = append(c.calls, "up")
	return c.failUp
}

func (c *fakeCompose) Logs(_ context.Context, service string, tail int) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, fmt.Sprintf("logs:%s:%d", service, tail))
	if c.failLogs != nil {
		return "", c.failLogs
	}
	return "service container logs here", nil
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
}

func (r *fakeRestore) Create(_ context.Context, inst, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, inst+":"+id)
	return nil
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

	fakeFs := systest.NewFS()
	fl := systest.NewFakeLocker()
	oldLockFile := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLockFile })
	sys.LockFile = fl.LockFile

	store := state.NewStore(fakeFs, stateDir)
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
		FS:         fakeFs,
		Store:      store,
		Compose: func(override string) ComposeAPI {
			return comp
		},
		Repo:    repo,
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
		fs:       fakeFs,
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

	// Assert no Up called
	if rig.compose.upCount > 0 {
		t.Fatalf("Up was called %d times; expected 0 because pre-deploy failed", rig.compose.upCount)
	}

	// Assert no final override promoted in FS
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

	// Assert Up ran twice: once for new deploy, once for rollback
	if rig.compose.upCount != 2 {
		t.Fatalf("expected compose.Up to run 2 times (deploy + rollback), got %d", rig.compose.upCount)
	}

	// Assert checkout of oldCommit occurred
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

	// State keeps previous Current
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

	// Health fails on both first deploy and rollback
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

	// Update env.spec.yml with missing prompt var
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

	// No pull called
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

	// Notification warn
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

func TestHookOutputRedacted(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")

	// Create app.env with a secret of length >= 8
	secretVal := "supersecret123456"
	appEnvPath := filepath.Join(rig.instDir, "app.env")
	if err := rig.fs.WriteFile(appEnvPath, []byte("APP_SECRET="+secretVal+"\n"), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write app.env: %v", err)
	}

	// Update env.spec.yml to recognize APP_SECRET
	specWithSecret := `vars:
  DATABASE_URL: { source: service, service: main-db }
  APP_SECRET:   { source: prompt, secret: true }
`
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte(specWithSecret), 0o644); err != nil {
		t.Fatalf("write env.spec.yml: %v", err)
	}

	// Pre-deploy hook outputs secret
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

	// Check that secretVal is replaced by *** in res.Detail
	if strings.Contains(res.Detail, secretVal) {
		t.Fatalf("res.Detail contains unredacted secret: %s", res.Detail)
	}
	if !strings.Contains(res.Detail, "***") {
		t.Fatalf("res.Detail does not contain redacted '***': %s", res.Detail)
	}

	// Check notifications
	for _, m := range rig.notifier.messages {
		if strings.Contains(m.Text, secretVal) {
			t.Fatalf("notification contains unredacted secret: %s", m.Text)
		}
	}

	// Check history events
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

func TestRemovedDataServiceKeepsVolume(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	want := defaultDesired()

	_, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy error: %v", err)
	}

	// Update compose.yml to remove the data service main-db
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

	_, err = rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Force: true, Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy without data service error: %v", err)
	}

	// Assert no calls contain down -v, volume rm or rm -v
	for _, c := range rig.compose.calls {
		if strings.Contains(c, "down -v") || strings.Contains(c, "volume rm") || strings.Contains(c, "rm -v") {
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
