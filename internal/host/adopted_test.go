package host

import (
	"context"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/adopt"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const adoptedRoutes = "/etc/traefik/dynamic/" + adopt.RouteFile

func adoptedEnv(t *testing.T, mode string, adopted bool) (*Env, *systest.FS) {
	t.Helper()
	fs := systest.NewFS()
	if adopted {
		s := adopt.Service{Name: "panel", Domain: "panel.example.com", Target: "http://host:9090"}
		if err := adopt.Save(fs, adopt.Dir, s); err != nil {
			t.Fatal(err)
		}
	}
	return &Env{FS: fs, Cfg: &hostcfg.Config{EdgeMode: mode}}, fs
}

func TestAdoptedStepRegeneratesAfterModeChange(t *testing.T) {
	e, fs := adoptedEnv(t, "behind-proxy", true)
	if err := adopt.Sync(fs, adopt.Dir, "/etc/traefik/dynamic", "behind-proxy", e.Cfg.EdgeGateway()); err != nil {
		t.Fatal(err)
	}
	e.Cfg.EdgeMode = "edge"
	step := AdoptedStep{}
	p, err := step.Check(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) == 0 {
		t.Fatal("expected a planned change")
	}
	if _, err := step.Apply(context.Background(), e, p); err != nil {
		t.Fatal(err)
	}
	b, err := fs.ReadFile(adoptedRoutes)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "websecure") {
		t.Fatalf("routes %s", b)
	}
}

func TestAdoptedStepCheckNoChange(t *testing.T) {
	for _, adopted := range []bool{true, false} {
		e, fs := adoptedEnv(t, "edge", adopted)
		if err := adopt.Sync(fs, adopt.Dir, "/etc/traefik/dynamic", "edge", e.Cfg.EdgeGateway()); err != nil {
			t.Fatal(err)
		}
		p, err := AdoptedStep{}.Check(context.Background(), e)
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Changes) != 0 {
			t.Fatalf("adopted=%v: changes %v", adopted, p.Changes)
		}
	}
}

func TestAdoptedStepRemovesRoutesWithEmptyRegistry(t *testing.T) {
	e, fs := adoptedEnv(t, "edge", false)
	if err := fs.WriteFile(adoptedRoutes, []byte("old"), 0o644, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	p, err := AdoptedStep{}.Check(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Changes) == 0 {
		t.Fatal("expected a planned change")
	}
}
