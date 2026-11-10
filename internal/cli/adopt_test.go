package cli

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/adopt"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type adoptFixture struct {
	env *OpsEnv
	run *systest.Runner
	dir string
	dyn string
}

func setupAdopt(t *testing.T, firewall bool) adoptFixture {
	t.Helper()
	tmp, env := setupOpsFixture(t, "myapp-dev")
	r := &systest.Runner{}
	r.OnPrefix("ufw", sys.Result{})
	r.OnPrefix("sh -c command -v ufw", sys.Result{})
	env.Run = r
	env.AdoptDir = filepath.Join(tmp, "adopted")
	env.Host.EdgeMode = "edge"
	env.Host.FirewallManage = firewall

	oldEnv, oldEUID, oldDial := newOpsEnv, adoptEUID, adoptDial
	t.Cleanup(func() { newOpsEnv, adoptEUID, adoptDial = oldEnv, oldEUID, oldDial })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	adoptEUID = func() int { return 0 }
	adoptDial = func(_, _ string, _ time.Duration) (net.Conn, error) {
		c, s := net.Pipe()
		_ = s.Close()
		return c, nil
	}
	return adoptFixture{env: env, run: r, dir: env.AdoptDir, dyn: env.Deployer.TraefikDir}
}

func (f adoptFixture) ufw() []string {
	var out []string
	for _, c := range f.run.Calls {
		if c.Name == "ufw" && !slices.Equal(c.Args, []string{"show", "added"}) {
			out = append(out, strings.Join(c.Args, " "))
		}
	}
	return out
}

func (f adoptFixture) registered(t *testing.T) []adopt.Service {
	t.Helper()
	svcs, err := adopt.Load(sys.NewOSFS(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	return svcs
}

func errorCode(t *testing.T, stdout string) string {
	t.Helper()
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &env); err != nil {
		t.Fatalf("%q: %v", stdout, err)
	}
	return env.Error.Code
}

var addCockpit = []string{"adopt", "add", "cockpit", "--domain", "server.example.com", "--target", "https://host:9090", "--insecure-tls"}

func TestAdoptAdd(t *testing.T) {
	f := setupAdopt(t, true)
	code, out, errb := run(t, addCockpit...)
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	if got := f.registered(t); len(got) != 1 || got[0].Domain != "server.example.com" || !got[0].InsecureTLS {
		t.Fatalf("registry %+v", got)
	}
	b, err := os.ReadFile(filepath.Join(f.dyn, adopt.RouteFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "https://10.203.0.1:9090") {
		t.Fatalf("routes %s", b)
	}
	want := "allow from 10.203.0.0/24 to any port 9090 proto tcp"
	if got := f.ufw(); len(got) != 1 || got[0] != want {
		t.Fatalf("ufw %v", got)
	}
}

func TestAdoptAddWithoutFirewall(t *testing.T) {
	f := setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if got := f.ufw(); len(got) != 0 {
		t.Fatalf("ufw %v", got)
	}
}

func TestAdoptAddNeedsRoot(t *testing.T) {
	f := setupAdopt(t, true)
	adoptEUID = func() int { return 1000 }
	code, out, _ := run(t, append(addCockpit, "--json")...)
	if code == 0 || errorCode(t, out) != "E_ADOPT_ROOT" {
		t.Fatalf("code %d: %s", code, out)
	}
	if got := f.registered(t); len(got) != 0 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptAddNeedsInit(t *testing.T) {
	f := setupAdopt(t, true)
	f.env.Host = nil
	code, out, _ := run(t, append(addCockpit, "--json")...)
	if code == 0 || errorCode(t, out) != "E_ADOPT_NO_HOST" {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestAdoptAddNameConflicts(t *testing.T) {
	f := setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	for _, name := range []string{"cockpit", "myapp-dev"} {
		code, out, _ := run(t, "adopt", "add", name, "--domain", "other.example.com", "--target", "http://host:1", "--json")
		if code == 0 || errorCode(t, out) != "E_ADOPT_EXISTS" {
			t.Fatalf("%s: code %d: %s", name, code, out)
		}
	}
	if got := f.registered(t); len(got) != 1 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptAddDomainInUseByAdopted(t *testing.T) {
	setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	code, out, _ := run(t, "adopt", "add", "other", "--domain", "server.example.com", "--target", "http://host:1", "--json")
	if code == 0 || errorCode(t, out) != "E_ADOPT_DOMAIN_IN_USE" || !strings.Contains(out, "cockpit") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestAdoptAddDomainInUseByInstance(t *testing.T) {
	f := setupAdopt(t, false)
	domains := f.env.projectListItem("myapp-dev").Domains
	var domain string
	for _, d := range domains {
		domain = d
	}
	if domain == "" {
		t.Fatalf("fixture has no domain: %v", domains)
	}
	code, out, _ := run(t, "adopt", "add", "other", "--domain", domain, "--target", "http://host:1", "--json")
	if code == 0 || errorCode(t, out) != "E_ADOPT_DOMAIN_IN_USE" || !strings.Contains(out, "myapp-dev") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestAdoptAddUnreachable(t *testing.T) {
	f := setupAdopt(t, true)
	adoptDial = func(_, addr string, _ time.Duration) (net.Conn, error) {
		if addr != "10.203.0.1:9090" {
			t.Errorf("dialed %s", addr)
		}
		return nil, errors.New("refused")
	}
	code, out, _ := run(t, append(addCockpit, "--json")...)
	if code == 0 || errorCode(t, out) != "E_ADOPT_UNREACHABLE" || !strings.Contains(out, "127.0.0.1") {
		t.Fatalf("code %d: %s", code, out)
	}
	if got := f.registered(t); len(got) != 0 {
		t.Fatalf("registry %+v", got)
	}
	if code, out, _ := run(t, append(addCockpit, "--force")...); code != 0 {
		t.Fatalf("force: code %d: %s", code, out)
	}
	if got := f.registered(t); len(got) != 1 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptAddKeepsNoRegistryWhenFirewallFails(t *testing.T) {
	f := setupAdopt(t, true)
	r := &systest.Runner{}
	r.OnPrefix("ufw", sys.Result{ExitCode: 1, Stderr: []byte("boom")})
	r.OnPrefix("sh -c command -v ufw", sys.Result{})
	f.env.Run = r
	if code, _, _ := run(t, addCockpit...); code == 0 {
		t.Fatal("expected failure")
	}
	if got := f.registered(t); len(got) != 0 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptRm(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if code, out, _ := run(t, "adopt", "rm", "cockpit"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if got := f.registered(t); len(got) != 0 {
		t.Fatalf("registry %+v", got)
	}
	if _, err := os.Stat(filepath.Join(f.dyn, adopt.RouteFile)); !os.IsNotExist(err) {
		t.Fatalf("routes file: %v", err)
	}
	got := f.ufw()
	want := "delete allow from 10.203.0.0/24 to any port 9090 proto tcp"
	if len(got) != 2 || got[1] != want {
		t.Fatalf("ufw %v", got)
	}
}

func TestAdoptRmKeepsSharedPortRule(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if code, out, _ := run(t, "adopt", "add", "other", "--domain", "o.example.com", "--target", "http://host:9090"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if code, out, _ := run(t, "adopt", "rm", "other"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	for _, c := range f.ufw() {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("ufw %v", f.ufw())
		}
	}
}

func TestAdoptRmNotFound(t *testing.T) {
	setupAdopt(t, true)
	code, out, _ := run(t, "adopt", "rm", "nope", "--json")
	if code == 0 || errorCode(t, out) != "E_ADOPT_NOT_FOUND" {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestAdoptList(t *testing.T) {
	setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	_, out, _ := run(t, "adopt", "list")
	for _, want := range []string{"NAME", "DOMAIN", "TARGET", "cockpit", "server.example.com", "https://host:9090"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in %s", want, out)
		}
	}
	_, out, _ = run(t, "adopt", "list", "--json")
	var env struct {
		Data struct {
			Adopted []adopt.Service `json:"adopted"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Data.Adopted) != 1 || env.Data.Adopted[0].Name != "cockpit" {
		t.Fatalf("got %s", out)
	}
}

func TestStatusShowsAdopted(t *testing.T) {
	setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	_, out, _ := run(t, "status")
	found := false
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) == 7 && f[0] == "cockpit" {
			found = f[1] == "-" && f[2] == "-" && f[3] == "ADOPTED" && f[4] == "-" && f[5] == "-" && f[6] == "-"
		}
	}
	if !found {
		t.Fatalf("no adopted row in %s", out)
	}
}

func TestStatusJSONMarksAdopted(t *testing.T) {
	setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	_, out, _ := run(t, "status", "--json")
	var env struct {
		Data struct {
			Instances []map[string]any `json:"instances"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]any{}
	for _, it := range env.Data.Instances {
		kinds[it["instance"].(string)] = it["kind"]
	}
	if kinds["cockpit"] != "adopted" {
		t.Fatalf("kinds %v", kinds)
	}
	if v, ok := kinds["myapp-dev"]; !ok || v != nil {
		t.Fatalf("instance kind %v in %v", v, kinds)
	}
}

func TestStatusByNameIgnoresAdopted(t *testing.T) {
	setupAdopt(t, false)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	code, out, _ := run(t, "status", "cockpit", "--json")
	if code == 0 || errorCode(t, out) != "E_INSTANCE_NOT_FOUND" {
		t.Fatalf("code %d: %s", code, out)
	}
}

// breakRoutes makes writing adopted.yml fail: its directory is a regular file.
func (f adoptFixture) breakRoutes(t *testing.T) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	f.env.Deployer.TraefikDir = p
}

func TestAdoptAddRollbackKeepsSharedPortRule(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	f.breakRoutes(t)
	if code, _, _ := run(t, "adopt", "add", "other", "--domain", "o.example.com", "--target", "http://host:9090"); code == 0 {
		t.Fatal("expected failure")
	}
	for _, c := range f.ufw() {
		if strings.HasPrefix(c, "delete") {
			t.Fatalf("ufw %v", f.ufw())
		}
	}
	if got := f.registered(t); len(got) != 1 || got[0].Name != "cockpit" {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptRmKeepsRegistryWhenRoutesFail(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	dyn := f.env.Deployer.TraefikDir
	f.breakRoutes(t)
	if code, _, _ := run(t, "adopt", "rm", "cockpit"); code == 0 {
		t.Fatal("expected failure")
	}
	if got := f.registered(t); len(got) != 1 {
		t.Fatalf("registry %+v", got)
	}
	f.env.Deployer.TraefikDir = dyn
	if code, out, _ := run(t, "adopt", "rm", "cockpit"); code != 0 {
		t.Fatalf("retry: code %d: %s", code, out)
	}
	if got := f.registered(t); len(got) != 0 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptRmKeepsRegistryWhenFirewallFails(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	r := &systest.Runner{}
	r.OnPrefix("ufw", sys.Result{ExitCode: 1, Stderr: []byte("boom")})
	r.OnPrefix("sh -c command -v ufw", sys.Result{})
	f.env.Run = r
	if code, _, _ := run(t, "adopt", "rm", "cockpit"); code == 0 {
		t.Fatal("expected failure")
	}
	if got := f.registered(t); len(got) != 1 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptAddRefusesWhenInstanceUnreadable(t *testing.T) {
	f := setupAdopt(t, false)
	if err := os.RemoveAll(filepath.Join(f.env.DeployRoot, "myapp-dev", "infra")); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "adopt", "add", "other", "--domain", "o.example.com", "--target", "http://host:1", "--json")
	if code == 0 || errorCode(t, out) != "E_ADOPT_INSTANCE_UNREADABLE" || !strings.Contains(out, "myapp-dev") {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestAdoptAddWithoutUfwInstalled(t *testing.T) {
	f := setupAdopt(t, true)
	r := &systest.Runner{}
	r.OnPrefix("sh -c command -v ufw", sys.Result{ExitCode: 1})
	f.env.Run = r
	code, out, errb := run(t, addCockpit...)
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	if !strings.Contains(errb, "ufw") {
		t.Fatalf("no warning in %q", errb)
	}
	if got := f.registered(t); len(got) != 1 {
		t.Fatalf("registry %+v", got)
	}
}

func TestAdoptAddReportsRollbackFailure(t *testing.T) {
	f := setupAdopt(t, true)
	r := &systest.Runner{}
	r.OnPrefix("ufw", sys.Result{})
	r.OnPrefix("ufw delete", sys.Result{ExitCode: 1, Stderr: []byte("boom")})
	r.OnPrefix("sh -c command -v ufw", sys.Result{})
	f.env.Run = r
	f.breakRoutes(t)
	code, _, errb := run(t, addCockpit...)
	if code == 0 {
		t.Fatal("expected failure")
	}
	if !strings.Contains(errb, "rollback") || !strings.Contains(errb, "9090") {
		t.Fatalf("no rollback warning in %q", errb)
	}
}

const cockpitRule = "ufw allow from 10.203.0.0/24 to any port 9090 proto tcp\n"

func hasDelete(calls []string) bool {
	return slices.ContainsFunc(calls, func(c string) bool { return strings.HasPrefix(c, "delete") })
}

func TestAdoptAddRecordsRule(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, errb := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errb)
	}
	want := adopt.RuleFor("10.203.0.0/24", 9090)
	if got := f.registered(t); len(got) != 1 || got[0].Firewall == nil || *got[0].Firewall != want {
		t.Fatalf("registry %+v", got)
	}
}

// An operator rule shows up in `ufw show added` or makes `ufw allow` skip; in
// both cases cvctl must not record it, so neither rm nor a rollback deletes it.
func TestAdoptLeavesOperatorRule(t *testing.T) {
	cases := []struct {
		name  string
		shown string
		allow string
	}{
		{"listed", cockpitRule, ""},
		{"allow skips", "", "Skipping adding existing rule\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupAdopt(t, true)
			f.run.OnPrefix("ufw show added", sys.Result{Stdout: []byte(c.shown)})
			f.run.OnPrefix("ufw allow", sys.Result{Stdout: []byte(c.allow)})
			if code, out, errb := run(t, addCockpit...); code != 0 {
				t.Fatalf("code %d: %s %s", code, out, errb)
			}
			if got := f.registered(t); len(got) != 1 || got[0].Firewall != nil {
				t.Fatalf("registry %+v", got)
			}
			if code, out, _ := run(t, "adopt", "rm", "cockpit"); code != 0 {
				t.Fatalf("rm: code %d: %s", code, out)
			}
			if hasDelete(f.ufw()) {
				t.Fatalf("ufw %v", f.ufw())
			}

			f.breakRoutes(t)
			if code, _, _ := run(t, addCockpit...); code == 0 {
				t.Fatal("expected failure")
			}
			if hasDelete(f.ufw()) {
				t.Fatalf("rollback: ufw %v", f.ufw())
			}
		})
	}
}

func TestAdoptSharedPortKeepsRuleUntilLast(t *testing.T) {
	f := setupAdopt(t, true)
	if code, out, _ := run(t, addCockpit...); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	f.run.OnPrefix("ufw show added", sys.Result{Stdout: []byte(cockpitRule)})
	if code, out, _ := run(t, "adopt", "add", "other", "--domain", "o.example.com", "--target", "http://host:9090"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	for _, s := range f.registered(t) {
		if s.Firewall == nil {
			t.Fatalf("%s did not record the shared cvctl rule", s.Name)
		}
	}
	if code, out, _ := run(t, "adopt", "rm", "cockpit"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	if hasDelete(f.ufw()) {
		t.Fatalf("ufw %v", f.ufw())
	}
	if code, out, _ := run(t, "adopt", "rm", "other"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	got := f.ufw()
	if want := "delete allow from 10.203.0.0/24 to any port 9090 proto tcp"; got[len(got)-1] != want {
		t.Fatalf("ufw %v", got)
	}
}

func TestAdoptRmDeletesOnlyRecordedRule(t *testing.T) {
	cases := []struct {
		name   string
		record *adopt.Rule
		want   []string
	}{
		{"legacy entry", nil, nil},
		{"old subnet", &adopt.Rule{Subnet: "172.30.4.0/22", Port: 9090, Proto: "tcp"},
			[]string{"delete allow from 172.30.4.0/22 to any port 9090 proto tcp"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := setupAdopt(t, true)
			s := adopt.Service{Name: "cockpit", Domain: "server.example.com", Target: "https://host:9090", Firewall: c.record}
			if err := adopt.Save(sys.NewOSFS(), f.dir, s); err != nil {
				t.Fatal(err)
			}
			if code, out, _ := run(t, "adopt", "rm", "cockpit"); code != 0 {
				t.Fatalf("code %d: %s", code, out)
			}
			if got := f.ufw(); !slices.Equal(got, c.want) {
				t.Fatalf("ufw %v, want %v", got, c.want)
			}
		})
	}
}

func TestAdoptAddWarnsWhenFirewallNotManaged(t *testing.T) {
	cases := []struct {
		firewall bool
		warn     bool
	}{{false, true}, {true, false}}
	for _, c := range cases {
		setupAdopt(t, c.firewall)
		code, out, errb := run(t, append(addCockpit, "--json")...)
		if code != 0 {
			t.Fatalf("code %d: %s %s", code, out, errb)
		}
		if got := strings.Contains(errb, "CVCTL_FIREWALL_MANAGE"); got != c.warn {
			t.Fatalf("firewall=%v: stderr %q", c.firewall, errb)
		}
	}
}
