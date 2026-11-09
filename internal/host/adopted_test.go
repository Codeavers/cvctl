package host

import (
	"context"
	"slices"
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

func rec(subnet string) *adopt.Rule {
	r := adopt.RuleFor(subnet, 9090)
	return &r
}

func TestAdoptedStepReconcilesFirewall(t *testing.T) {
	const (
		oldNet = "10.203.0.0/24"
		newNet = "172.30.4.0/22"
	)
	shown := func(subnet string) string {
		return "ufw allow from " + subnet + " to any port 9090 proto tcp\n"
	}
	allowNew := "allow from " + newNet + " to any port 9090 proto tcp"
	deleteOld := "delete allow from " + oldNet + " to any port 9090 proto tcp"
	cases := []struct {
		name     string
		manage   bool
		records  []*adopt.Rule // one adopted service per element, all on port 9090
		shown    string
		wantUfw  []string
		wantRecs []*adopt.Rule
	}{
		{"subnet change", true, []*adopt.Rule{rec(oldNet)}, shown(oldNet),
			[]string{allowNew, deleteOld}, []*adopt.Rule{rec(newNet)}},
		{"old rule already gone", true, []*adopt.Rule{rec(oldNet)}, "",
			[]string{allowNew}, []*adopt.Rule{rec(newNet)}},
		{"shared port", true, []*adopt.Rule{rec(oldNet), rec(oldNet)}, shown(oldNet),
			[]string{allowNew, deleteOld}, []*adopt.Rule{rec(newNet), rec(newNet)}},
		{"management enabled later", true, []*adopt.Rule{nil}, "",
			[]string{allowNew}, []*adopt.Rule{rec(newNet)}},
		{"operator rule kept", true, []*adopt.Rule{nil}, shown(newNet),
			nil, []*adopt.Rule{nil}},
		{"operator rule kept after subnet change", true, []*adopt.Rule{rec(oldNet)}, shown(oldNet) + shown(newNet),
			[]string{deleteOld}, []*adopt.Rule{nil}},
		{"recorded rule deleted by hand", true, []*adopt.Rule{rec(newNet)}, "",
			[]string{allowNew}, []*adopt.Rule{rec(newNet)}},
		{"in sync", true, []*adopt.Rule{rec(newNet)}, shown(newNet),
			nil, []*adopt.Rule{rec(newNet)}},
		{"not managed", false, []*adopt.Rule{rec(oldNet)}, shown(oldNet),
			nil, []*adopt.Rule{rec(oldNet)}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fs := systest.NewFS()
			r := &systest.Runner{}
			r.OnPrefix("sh -c command -v ufw", sys.Result{})
			r.OnPrefix("ufw", sys.Result{})
			r.OnPrefix("ufw show added", sys.Result{Stdout: []byte(c.shown)})
			cfg := &hostcfg.Config{EdgeMode: "edge", EdgeSubnet: newNet, FirewallManage: c.manage}
			e := &Env{FS: fs, Run: r, Cfg: cfg}
			for i, fw := range c.records {
				s := adopt.Service{Name: "svc" + string(rune('a'+i)), Domain: "svc" + string(rune('a'+i)) + ".example.com",
					Target: "http://host:9090", Firewall: fw}
				if err := adopt.Save(fs, adopt.Dir, s); err != nil {
					t.Fatal(err)
				}
			}
			if err := adopt.Sync(fs, adopt.Dir, dynamicDir, "edge", cfg.EdgeGateway()); err != nil {
				t.Fatal(err)
			}
			step := AdoptedStep{}
			p, err := step.Check(context.Background(), e)
			if err != nil {
				t.Fatal(err)
			}
			if (len(p.Changes) == 0) != (len(c.wantUfw) == 0 && slices.EqualFunc(c.records, c.wantRecs, sameRule)) {
				t.Fatalf("changes %v", p.Changes)
			}
			if len(p.Changes) > 0 {
				if _, err := step.Apply(context.Background(), e, p); err != nil {
					t.Fatal(err)
				}
			}
			var got []string
			for _, call := range r.Calls {
				if call.Name == "ufw" && !slices.Equal(call.Args, []string{"show", "added"}) {
					got = append(got, strings.Join(call.Args, " "))
				}
			}
			if !slices.Equal(got, c.wantUfw) {
				t.Fatalf("ufw %v, want %v", got, c.wantUfw)
			}
			svcs, err := adopt.Load(fs, adopt.Dir)
			if err != nil {
				t.Fatal(err)
			}
			for i, s := range svcs {
				if !sameRule(s.Firewall, c.wantRecs[i]) {
					t.Fatalf("%s record %+v, want %+v", s.Name, s.Firewall, c.wantRecs[i])
				}
			}
		})
	}
}

func sameRule(a, b *adopt.Rule) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}
