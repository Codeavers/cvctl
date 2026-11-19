package agent_test

import (
	"bytes"
	"context"
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
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func edgeConfig(t *testing.T, mode string) *hostcfg.Config {
	t.Helper()
	c := hostcfg.NewConfig()
	if err := c.Set("EDGE_MODE", mode); err != nil {
		t.Fatal(err)
	}
	return c
}

func reloadFixture(t *testing.T) (*state.Store, agent.Config) {
	t.Helper()
	store := state.NewStore(systest.NewFS(), "/var/lib/cvctl")
	if err := store.Save(&state.State{Instance: "demo-prod", Status: state.StatusOK, Current: &state.Desired{InfraCommit: "c1"}}); err != nil {
		t.Fatal(err)
	}
	return store, agent.Config{
		Interval: time.Minute,
		Store:    store,
		Load: func(inst string) (*instance.Decisions, *contract.Project, error) {
			return &instance.Decisions{Instance: inst, Env: "prod"}, &contract.Project{}, nil
		},
		Sources: func(*instance.Decisions) deploy.Source {
			return &fakeSource{desired: state.Desired{InfraCommit: "c2"}}
		},
		Once: true,
	}
}

// CODEA-49: an agent started before cvctl init switched the host to edge kept
// deploying with behind-proxy. The host config must be re-read before every
// tick's deploys.
func TestAgentReloadsHostBeforeDeploy(t *testing.T) {
	_, cfg := reloadFixture(t)
	var out bytes.Buffer
	cfg.Out = &out
	cfg.Host = edgeConfig(t, "behind-proxy")
	current := cfg.Host
	cfg.ReloadHost = func() (*hostcfg.Config, *hostcfg.Secrets, notify.Notifier, error) {
		current = edgeConfig(t, "edge")
		return current, hostcfg.NewSecrets(), nil, nil
	}
	dep := &fakeDeployer{onDeploy: func(context.Context, string, state.Desired) (deploy.Result, error) {
		if current.EdgeMode != "edge" {
			t.Errorf("deploy ran before the host config was reloaded")
		}
		return deploy.Result{Status: state.StatusOK}, nil
	}}
	cfg.Deployer = dep
	if err := agent.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if len(dep.calls) != 1 {
		t.Fatalf("deploys %v", dep.calls)
	}
	if !strings.Contains(out.String(), `"action":"reload","reason":"host config changed"`) {
		t.Fatalf("expected a reload decision, got:\n%s", out.String())
	}
}

func TestAgentNoReloadLogWhenHostUnchanged(t *testing.T) {
	_, cfg := reloadFixture(t)
	var out bytes.Buffer
	cfg.Out = &out
	cfg.Host = edgeConfig(t, "edge")
	cfg.Deployer = &fakeDeployer{}
	cfg.ReloadHost = func() (*hostcfg.Config, *hostcfg.Secrets, notify.Notifier, error) {
		return edgeConfig(t, "edge"), hostcfg.NewSecrets(), nil, nil
	}
	if err := agent.Run(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), `"action":"reload"`) {
		t.Fatalf("unexpected reload decision:\n%s", out.String())
	}
}

func TestAgentSkipsTickWhenReloadFails(t *testing.T) {
	_, cfg := reloadFixture(t)
	var out syncBuffer
	cfg.Out = &out
	cfg.Once = false
	cfg.Interval = 5 * time.Millisecond
	cfg.Host = edgeConfig(t, "behind-proxy")
	notif := &fakeNotifier{}
	cfg.Notify = notif
	dep := &fakeDeployer{}
	cfg.Deployer = dep

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	calls := 0
	cfg.ReloadHost = func() (*hostcfg.Config, *hostcfg.Secrets, notify.Notifier, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if calls >= 3 {
			cancel()
		}
		return nil, nil, nil, errors.New("E_HOSTCFG_READ: bad host.env")
	}
	if err := agent.Run(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	if len(dep.calls) != 0 {
		t.Fatalf("deployed with an unreadable host config: %v", dep.calls)
	}
	if n := strings.Count(out.String(), "cannot reload host config"); n != 1 {
		t.Fatalf("expected the reload error logged once, got %d:\n%s", n, out.String())
	}
	notif.mu.Lock()
	defer notif.mu.Unlock()
	if len(notif.messages) != 1 || notif.messages[0].Level != "error" {
		t.Fatalf("notifications %+v", notif.messages)
	}
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
