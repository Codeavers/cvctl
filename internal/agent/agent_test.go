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
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type fakeDeployer struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeDeployer) Deploy(ctx context.Context, inst string, want state.Desired, opts deploy.Options) (deploy.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, inst+":"+want.InfraCommit)
	return deploy.Result{Status: state.StatusOK, ID: "test-deploy"}, nil
}

type fakeSource struct {
	desired state.Desired
	err     error
}

func (f *fakeSource) Desired(ctx context.Context, d *instance.Decisions, p *contract.Project) (state.Desired, error) {
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

	ctx := context.Background()
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("agent.Run failed: %v", err)
	}

	dep.mu.Lock()
	calls := dep.calls
	dep.mu.Unlock()

	if len(calls) != 1 || calls[0] != "good-app:commit-new" {
		t.Fatalf("expected good-app to deploy despite bad-app error, got %v", calls)
	}

	notif.mu.Lock()
	notifCount := len(notif.messages)
	notif.mu.Unlock()
	if notifCount != 1 {
		t.Fatalf("expected 1 notification for bad-app, got %d", notifCount)
	}

	// Run second tick with same error to verify notification/error log deduplication
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatalf("second agent.Run failed: %v", err)
	}
	notif.mu.Lock()
	notifCountAfter := len(notif.messages)
	notif.mu.Unlock()
	if notifCountAfter != 1 {
		t.Fatalf("expected deduplication: notification count should still be 1, got %d", notifCountAfter)
	}
}
