package adopt

import (
	"slices"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestRuleArgs(t *testing.T) {
	r := RuleFor("10.203.0.0/24", 9090)
	want := []string{"from", "10.203.0.0/24", "to", "any", "port", "9090", "proto", "tcp"}
	if got := r.Args(); !slices.Equal(got, want) {
		t.Fatalf("args %v", got)
	}
}

func TestParseAdded(t *testing.T) {
	out := `Added user rules (see 'ufw status' for running firewall):
ufw allow 22/tcp
ufw allow from 10.203.0.0/24 to any port 9090 proto tcp
ufw allow in from 172.30.4.0/22 to any port 3000 proto tcp comment 'mine'
ufw allow from 10.0.0.0/24 to any port 4000
ufw deny from 10.203.0.0/24 to any port 5000 proto tcp
ufw allow out from 10.203.0.0/24 to any port 6000 proto tcp
ufw allow in on eth0 from 10.203.0.0/24 to any port 7000 proto tcp
ufw allow from 10.203.0.0/24 to 10.203.0.1 port 8000 proto tcp
ufw allow from 10.0.0.0/24 port 1234 to any port 9090 proto tcp
`
	got := ParseAdded([]byte(out))
	want := []Rule{RuleFor("10.203.0.0/24", 9090), RuleFor("172.30.4.0/22", 3000)}
	if !slices.Equal(got, want) {
		t.Fatalf("rules %+v", got)
	}
	if got := ParseAdded([]byte("(None)\n")); len(got) != 0 {
		t.Fatalf("rules %+v", got)
	}
}

func TestRecorded(t *testing.T) {
	r := RuleFor("10.203.0.0/24", 9090)
	svcs := []Service{
		{Name: "a", Target: "http://host:9090", Firewall: &r},
		{Name: "b", Target: "http://host:9090"},
	}
	cases := []struct {
		except string
		rule   Rule
		want   bool
	}{
		{"b", r, true},
		{"a", r, false},
		{"b", RuleFor("172.30.4.0/22", 9090), false},
	}
	for _, c := range cases {
		if got := Recorded(svcs, c.rule, c.except); got != c.want {
			t.Fatalf("except %s rule %+v: %v", c.except, c.rule, got)
		}
	}
}

func TestLoadFirewallRecord(t *testing.T) {
	fs := systest.NewFS()
	r := RuleFor("10.203.0.0/24", 9090)
	s := Service{Name: "panel", Domain: "panel.example.com", Target: "http://host:9090", Firewall: &r}
	if err := Save(fs, Dir, s); err != nil {
		t.Fatal(err)
	}
	legacy := "name: old\ndomain: old.example.com\ntarget: http://host:3000\n"
	if err := fs.WriteFile(Dir+"/old.yml", []byte(legacy), 0o644, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	svcs, err := Load(fs, Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(svcs) != 2 || svcs[0].Firewall != nil || svcs[1].Firewall == nil || *svcs[1].Firewall != r {
		t.Fatalf("registry %+v", svcs)
	}
}

func TestLoadRejectsBadFirewallRecord(t *testing.T) {
	for _, rec := range []string{
		"{subnet: nope, port: 9090, proto: tcp}",
		"{subnet: 10.203.0.0/24, port: 0, proto: tcp}",
		"{subnet: 10.203.0.0/24, port: 9090, proto: udp}",
		"{subnet: '10.203.0.0/24; reboot', port: 9090, proto: tcp}",
	} {
		fs := systest.NewFS()
		if err := fs.MkdirAll(Dir, 0o755, sys.Owner{}); err != nil {
			t.Fatal(err)
		}
		b := "name: panel\ndomain: panel.example.com\ntarget: http://host:9090\nfirewall: " + rec + "\n"
		if err := fs.WriteFile(Dir+"/panel.yml", []byte(b), 0o644, sys.Owner{}); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(fs, Dir); err == nil || errCode(t, err) != "E_ADOPT_REGISTRY" {
			t.Fatalf("%s: err %v", rec, err)
		}
	}
}
