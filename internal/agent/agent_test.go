package agent_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/agent"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type fakeDeployer struct {
	mu       sync.Mutex
	calls    []string
	onDeploy func(ctx context.Context, inst string, want state.Desired) (deploy.Result, error)
}

func (f *fakeDeployer) Deploy(ctx context.Context, inst string, want state.Desired, opts deploy.Options) (deploy.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, inst+":"+want.InfraCommit)
	if f.onDeploy != nil {
		return f.onDeploy(ctx, inst, want)
	}
	return deploy.Result{Status: state.StatusOK, ID: "test-deploy"}, nil
}

type fakeSource struct {
	desired state.Desired
	err     error
}

func (f *fakeSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	if f.err != nil {
		return state.Desired{}, f.err
	}
	return f.desired, nil
}

type fakeNotifier struct {
	mu       sync.Mutex
	messages []notify.Message
}

func (f *fakeNotifier) Send(ctx context.Context, msg notify.Message) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.messages = append(f.messages, msg)
	return nil
}

func TestAgentDeploysOnChange(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	dep := &fakeDeployer{}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	now := time.Now()
	st := &state.State{
		Instance: "demo-dev",
		Status:   state.StatusOK,
		Current: &state.Desired{
			InfraCommit: "commit-1",
		},
		UpdatedAt: now,
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{
				desired: state.Desired{InfraCommit: "commit-2"},
			}
		},
		Notify: notif,
		Out:    &out,
		Once:   true,
	}

	ctx := context.Background()
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) != 1 || calls[0] != "demo-dev:commit-2" {
		t.Fatalf("expected deploy of demo-dev:commit-2, got %v", calls)
	}

	logStr := out.String()
	if !strings.Contains(logStr, `"action":"deploy"`) {
		t.Fatalf("expected log to contain action:deploy, got:\n%s", logStr)
	}
	var logEntry agent.DecisionLog
	if err := json.Unmarshal([]byte(strings.TrimSpace(logStr)), &logEntry); err != nil {
		t.Fatalf("unmarshal decision log line: %v", err)
	}
	if logEntry.Action != "deploy" || logEntry.Instance != "demo-dev" {
		t.Fatalf("unexpected log entry: %+v", logEntry)
	}
}

func TestAgentSkipsPausedAndFailedHash(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	dep := &fakeDeployer{}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	wantPaused := state.Desired{InfraCommit: "commit-paused"}
	stPaused := &state.State{
		Instance:       "paused-app",
		Status:         state.StatusPaused,
		Current:        &wantPaused,
		PausedWantHash: wantPaused.Hash(),
		PausedReason:   "manual pause",
		UpdatedAt:      time.Now(),
	}
	if err := store.Save(stPaused); err != nil {
		t.Fatal(err)
	}

	wantFailed := state.Desired{InfraCommit: "commit-failed"}
	stFailed := &state.State{
		Instance:   "failed-app",
		Status:     state.StatusFailed,
		Current:    &state.Desired{InfraCommit: "commit-old"},
		FailedHash: wantFailed.Hash(),
		UpdatedAt:  time.Now(),
	}
	if err := store.Save(stFailed); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			if d.Instance == "paused-app" {
				return &fakeSource{desired: wantPaused}
			}
			return &fakeSource{desired: wantFailed}
		},
		Notify: notif,
		Out:    &out,
		Once:   true,
	}

	ctx := context.Background()
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) != 0 {
		t.Fatalf("expected 0 deploy calls, got %v", calls)
	}

	logStr := out.String()
	lines := strings.Split(strings.TrimSpace(logStr), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 log lines, got: %s", logStr)
	}
	for _, l := range lines {
		var entry agent.DecisionLog
		if err := json.Unmarshal([]byte(l), &entry); err != nil {
			t.Fatalf("unmarshal line %q: %v", l, err)
		}
		if entry.Action != "skip" {
			t.Fatalf("expected action:skip, got %+v", entry)
		}
	}
}

func TestAgentRetriesBlocked(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	dep := &fakeDeployer{}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	want := state.Desired{InfraCommit: "commit-blocked"}
	stBlocked := &state.State{
		Instance:  "blocked-app",
		Status:    state.StatusBlocked,
		Blocked:   []string{"SECRET_KEY"},
		Current:   &want,
		UpdatedAt: time.Now(),
	}
	if err := store.Save(stBlocked); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: want}
		},
		Notify: notif,
		Out:    &out,
		Once:   true,
	}

	ctx := context.Background()
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) != 1 || calls[0] != "blocked-app:commit-blocked" {
		t.Fatalf("expected deploy call for blocked instance, got %v", calls)
	}

	logStr := out.String()
	if !strings.Contains(logStr, `"action":"deploy"`) {
		t.Fatalf("expected action:deploy in log, got: %s", logStr)
	}
}

func TestAgentOneInstanceErrorDoesNotStopOthers(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	dep := &fakeDeployer{}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	st1 := &state.State{
		Instance:  "bad-app",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-1"},
		UpdatedAt: time.Now(),
	}
	st2 := &state.State{
		Instance:  "good-app",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-old"},
		UpdatedAt: time.Now(),
	}
	if err := store.Save(st1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st2); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			if inst == "bad-app" {
				return nil, nil, errors.New("cannot read cvctl.env")
			}
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "commit-new"}}
		},
		Notify: notif,
		Out:    &out,
		Once:   true,
	}

	cfg.Interval = 5 * time.Millisecond
	cfg.Once = false

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) < 2 {
		t.Fatalf("expected at least 2 ticks to run for good-app, got %v", calls)
	}

	notif.mu.Lock()
	notifCount := len(notif.messages)
	notif.mu.Unlock()
	if notifCount != 1 {
		t.Fatalf("expected deduplication: notification count should be 1 across ticks, got %d", notifCount)
	}
}

func TestAgentPanicRecoveryPerInstance(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	dep := &fakeDeployer{}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	st1 := &state.State{
		Instance:  "panic-app",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-1"},
		UpdatedAt: time.Now(),
	}
	st2 := &state.State{
		Instance:  "good-app",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-old"},
		UpdatedAt: time.Now(),
	}
	if err := store.Save(st1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st2); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			if inst == "panic-app" {
				panic("database exploded")
			}
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "commit-new"}}
		},
		Notify: notif,
		Out:    &out,
		Once:   true,
	}

	ctx := context.Background()
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) != 1 || calls[0] != "good-app:commit-new" {
		t.Fatalf("expected good-app to deploy despite panic in panic-app, got %v", calls)
	}

	logStr := out.String()
	if !strings.Contains(logStr, `"action":"error"`) || !strings.Contains(logStr, `"reason":"panic: database exploded"`) {
		t.Fatalf("expected log to contain panic reason, got: %s", logStr)
	}

	notif.mu.Lock()
	msgs := notif.messages
	notif.mu.Unlock()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "panic: database exploded") {
		t.Fatalf("expected notification for panic, got: %v", msgs)
	}
}

func TestAgentDeployErrorHandledAndDeduped(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	dep := &fakeDeployer{
		onDeploy: func(ctx context.Context, inst string, want state.Desired) (deploy.Result, error) {
			return deploy.Result{}, errors.New("compose up failed: container died")
		},
	}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	st := &state.State{
		Instance:  "fail-app",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-1"},
		UpdatedAt: time.Now(),
	}
	if err := store.Save(st); err != nil {
		t.Fatal(err)
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "commit-2"}}
		},
		Notify: notif,
		Out:    &out,
		Once:   true,
	}

	ctx := context.Background()
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	logStr := out.String()
	if !strings.Contains(logStr, `"action":"error"`) || !strings.Contains(logStr, "compose up failed: container died") {
		t.Fatalf("expected deploy error in log, got: %s", logStr)
	}

	notif.mu.Lock()
	msgs := notif.messages
	notif.mu.Unlock()
	if len(msgs) != 1 || !strings.Contains(msgs[0].Text, "compose up failed: container died") {
		t.Fatalf("expected 1 notification for deploy error, got: %v", msgs)
	}

	// Verify success logs Result status
	out.Reset()
	dep.onDeploy = func(ctx context.Context, inst string, want state.Desired) (deploy.Result, error) {
		return deploy.Result{Status: state.StatusOK, ID: "deploy-123"}, nil
	}
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	logStr = out.String()
	if !strings.Contains(logStr, `"action":"deploy"`) || !strings.Contains(logStr, `"reason":"OK"`) {
		t.Fatalf("expected action:deploy with reason:OK, got: %s", logStr)
	}
}

func TestAgentDeployCancelledOnContextDone(t *testing.T) {
	fsys := systest.NewFS()
	store := state.NewStore(fsys, "/var/lib/cvctl")
	notif := &fakeNotifier{}
	var out bytes.Buffer

	st1 := &state.State{
		Instance:  "app-1",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-1"},
		UpdatedAt: time.Now(),
	}
	st2 := &state.State{
		Instance:  "app-2",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-1"},
		UpdatedAt: time.Now(),
	}
	if err := store.Save(st1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(st2); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var deployCtxErr error
	dep := &fakeDeployer{
		onDeploy: func(dCtx context.Context, inst string, want state.Desired) (deploy.Result, error) {
			if inst == "app-1" {
				cancel()
				deployCtxErr = dCtx.Err()
			}
			return deploy.Result{Status: state.StatusOK}, nil
		},
	}

	cfg := agent.Config{
		Interval: time.Minute,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "commit-2"}}
		},
		Notify: notif,
		Out:    &out,
		Once:   false,
	}

	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	if !errors.Is(deployCtxErr, context.Canceled) {
		t.Fatalf("expected the deploy context to be cancelled with the parent, got: %v", deployCtxErr)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) != 1 || calls[0] != "app-1:commit-2" {
		t.Fatalf("expected only app-1 to deploy and loop to stop before app-2, got %v", calls)
	}
}

type fakeStoreWithLoadErr struct {
	agent.Store
	loadErr error
}

func (s *fakeStoreWithLoadErr) Load(inst string) (*state.State, error) {
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	return s.Store.Load(inst)
}

func TestAgentStateLoadErrorDeduped(t *testing.T) {
	fsys := systest.NewFS()
	baseStore := state.NewStore(fsys, "/var/lib/cvctl")
	st := &state.State{
		Instance:  "bad-load-app",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "commit-1"},
		UpdatedAt: time.Now(),
	}
	if err := baseStore.Save(st); err != nil {
		t.Fatal(err)
	}

	store := &fakeStoreWithLoadErr{
		Store:   baseStore,
		loadErr: errors.New("cannot decode state JSON"),
	}

	dep := &fakeDeployer{}
	notif := &fakeNotifier{}
	var out bytes.Buffer

	cfg := agent.Config{
		Interval: 5 * time.Millisecond,
		Store:    store,
		Deployer: dep,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "commit-2"}}
		},
		Notify: notif,
		Out:    &out,
		Once:   false,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	logStr := out.String()
	if !strings.Contains(logStr, `"action":"error"`) || !strings.Contains(logStr, "cannot load state: cannot decode state JSON") {
		t.Fatalf("expected state load error in log, got: %s", logStr)
	}

	notif.mu.Lock()
	count := len(notif.messages)
	notif.mu.Unlock()

	if count != 1 {
		t.Fatalf("expected state load error to be notified exactly once across ticks, got %d", count)
	}
}

func agentHost(t *testing.T, kv ...string) *hostcfg.Config {
	t.Helper()
	h := hostcfg.NewConfig()
	for i := 0; i < len(kv); i += 2 {
		if err := h.Set(kv[i], kv[i+1]); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

func seedOK(t *testing.T, store *state.Store, insts ...string) {
	t.Helper()
	for _, inst := range insts {
		if err := store.Save(&state.State{Instance: inst, Status: state.StatusOK, Current: &state.Desired{InfraCommit: "commit-1"}}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAgentDeployTimeoutDoesNotStopNextInstance(t *testing.T) {
	store := state.NewStore(systest.NewFS(), "/var/lib/cvctl")
	seedOK(t, store, "app-a", "app-b")
	var out bytes.Buffer
	notif := &fakeNotifier{}
	dep := &fakeDeployer{
		onDeploy: func(ctx context.Context, inst string, want state.Desired) (deploy.Result, error) {
			if inst == "app-a" {
				<-ctx.Done()
				return deploy.Result{}, errors.New("killed")
			}
			return deploy.Result{Status: state.StatusOK}, nil
		},
	}
	cfg := agent.Config{
		Store: store, Deployer: dep, Notify: notif, Out: &out, Once: true,
		Host: agentHost(t, "TIMEOUT_DEPLOY", "50ms"),
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "commit-2"}}
		},
	}
	if err := agent.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(dep.calls) != 2 || dep.calls[1] != "app-b:commit-2" {
		t.Fatalf("app-b must deploy after app-a timed out, got %v", dep.calls)
	}
	if !strings.Contains(out.String(), "E_TIMEOUT: deploy timed out after 50ms") {
		t.Fatalf("expected E_TIMEOUT naming the deploy step, got:\n%s", out.String())
	}
	if len(notif.messages) != 1 || !strings.Contains(notif.messages[0].Text, "E_TIMEOUT") {
		t.Fatalf("expected one E_TIMEOUT notification, got %+v", notif.messages)
	}
}

type blockingSource struct{}

func (blockingSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	<-ctx.Done()
	return state.Desired{}, errors.New("killed")
}

func TestAgentFetchTimeout(t *testing.T) {
	store := state.NewStore(systest.NewFS(), "/var/lib/cvctl")
	seedOK(t, store, "app-a")
	var out bytes.Buffer
	dep := &fakeDeployer{}
	cfg := agent.Config{
		Store: store, Deployer: dep, Out: &out, Once: true,
		Host: agentHost(t, "TIMEOUT_FETCH", "50ms"),
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "dev"}, &contract.Project{}, nil
		},
		Sources: func(d *instance.Decisions) deploy.Source { return blockingSource{} },
	}
	if err := agent.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(dep.calls) != 0 {
		t.Fatalf("no deploy expected, got %v", dep.calls)
	}
	if !strings.Contains(out.String(), "E_TIMEOUT: fetch timed out after 50ms") {
		t.Fatalf("expected E_TIMEOUT naming the fetch step, got:\n%s", out.String())
	}
}

func TestAgentRecoversDeployingOnStartup(t *testing.T) {
	oldLock := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLock })
	sys.LockFile = systest.NewFakeLocker().LockFile
	store := state.NewStore(systest.NewFS(), "/var/lib/cvctl")
	cur := state.Desired{InfraCommit: "commit-1"}
	if err := store.Save(&state.State{Instance: "with-current", Status: state.StatusDeploying, Current: &cur}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(&state.State{Instance: "fresh", Status: state.StatusDeploying, PriorStatus: state.StatusBlocked}); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cfg := agent.Config{
		Store: store, Deployer: &fakeDeployer{}, Out: &out, Once: true,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return nil, nil, errors.New("skip")
		},
	}
	if err := agent.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for inst, want := range map[string]state.Status{"with-current": state.StatusOK, "fresh": state.StatusBlocked} {
		st, err := store.Load(inst)
		if err != nil {
			t.Fatal(err)
		}
		if st.Status != want {
			t.Fatalf("%s: status %s, want %s", inst, st.Status, want)
		}
		hist, err := store.History(inst, 5)
		if err != nil || len(hist) != 1 || hist[0].Kind != "interrupted" {
			t.Fatalf("%s: expected one interrupted history entry, got %v (%v)", inst, hist, err)
		}
	}
}
