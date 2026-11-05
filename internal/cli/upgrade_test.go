package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/internal/upgrade"
)

const (
	upNew   = "v0.8.3"
	upTmp   = "/usr/local/bin/.cvctl-upgrade-v0.8.3"
	upLock  = "/run/cvctl-upgrade.lock"
	upPlanA = `{"ok":true,"data":{"schema":1,"instances":{"cute":{"data":{"media":{"backup":"volume"}}}}}}`
	upPlanB = `{"ok":true,"data":{"schema":1,"instances":{"cute":{"data":{"media":{"backup":"none"}}}}}}`
)

type recNotifier struct {
	msgs   []notify.Message
	err    error
	onSend func()
}

func (r *recNotifier) Send(_ context.Context, m notify.Message) error {
	r.msgs = append(r.msgs, m)
	if r.onSend != nil {
		r.onSend()
	}
	return r.err
}

type upFixture struct {
	run    *systest.Runner
	fs     *systest.FS
	notif  *recNotifier
	euid   int
	tty    bool
	busy   []string
	logBuf *bytes.Buffer
	lock   *systest.FakeLocker
	// onDeploying runs each time the upgrade asks for deploys in progress.
	onDeploying func()
}

func newUpFixture(t *testing.T, planNew string) *upFixture {
	t.Helper()
	bin := []byte("new binary")
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/dl/bin":
			w.Write(bin)
		case "/dl/sums":
			fmt.Fprintf(w, "%x  cvctl-linux-amd64\n", sha256.Sum256(bin))
		default:
			fmt.Fprintf(w, `{"tag_name":%q,"body":"notas de la release","assets":[
{"name":"cvctl-linux-amd64","browser_download_url":"%s/dl/bin"},
{"name":"sha256sums.txt","browser_download_url":"%s/dl/sums"}]}`, upNew, srv.URL, srv.URL)
		}
	}))
	t.Cleanup(srv.Close)

	f := &upFixture{run: &systest.Runner{}, fs: systest.NewFS(), notif: &recNotifier{}, logBuf: &bytes.Buffer{}, lock: systest.NewFakeLocker()}
	if err := f.fs.WriteFile(host.AgentBinaryPath, []byte("old binary"), 0o755, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	f.run.On(upTmp+" version --json", sys.Result{Stdout: []byte(`{"ok":true,"data":{"version":"` + upNew + `"}}`)})
	f.run.On(upTmp+" plan --json", sys.Result{Stdout: []byte(planNew)})
	f.run.On(host.AgentBinaryPath+" plan --json", sys.Result{Stdout: []byte(upPlanA)})
	f.run.On("systemctl show -p NRestarts --value cvctl-agent", sys.Result{Stdout: []byte("0\n")})
	f.run.On("systemctl is-active cvctl-agent", sys.Result{Stdout: []byte("active\n")})
	f.run.On("systemctl restart cvctl-agent", sys.Result{})

	oldEnv, oldLog, oldLock, oldEUID := newUpgradeEnv, notifyLog, sys.Lock, upgradeEUID
	upgradeEUID = func() int { return f.euid }
	notifyLog = f.logBuf
	sys.Lock = f.lock.Lock
	newUpgradeEnv = func(current string) (*upgradeEnv, error) {
		return &upgradeEnv{
			Upgrader: &upgrade.Upgrader{
				Run: f.run, FS: f.fs,
				Client:  upgrade.Client{BaseURL: srv.URL, Repo: "o/r"},
				Arch:    "amd64",
				Current: current,
				Exe:     host.AgentBinaryPath,
				Deploying: func() ([]string, error) {
					if f.onDeploying != nil {
						f.onDeploying()
					}
					return f.busy, nil
				},
				Sleep: func(_ time.Duration) {},
			},
			Interactive: f.tty,
			Notifier:    func() notify.Notifier { return f.notif },
		}, nil
	}
	t.Cleanup(func() { newUpgradeEnv, notifyLog, sys.Lock, upgradeEUID = oldEnv, oldLog, oldLock, oldEUID })
	return f
}

func (f *upFixture) installed(t *testing.T) string {
	t.Helper()
	b, err := f.fs.ReadFile(host.AgentBinaryPath)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func runUpgrade(t *testing.T, stdin string, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	root := NewRoot("v0.8.2")
	root.SetIn(strings.NewReader(stdin))
	code = Execute(root, append([]string{"upgrade"}, args...), &out, &errb)
	return code, out.String(), errb.String()
}

func errCode(t *testing.T, stdout string) (code, hint string) {
	t.Helper()
	var doc struct {
		Error struct{ Code, Hint string } `json:"error"`
	}
	if err := json.Unmarshal([]byte(stdout), &doc); err != nil {
		t.Fatalf("not json: %q", stdout)
	}
	return doc.Error.Code, doc.Error.Hint
}

func TestUpgradeNeedsRoot(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.euid = 1000
	newUpgradeEnv = func(string) (*upgradeEnv, error) {
		return nil, errors.New("host config is not readable without root")
	}
	code, out, _ := runUpgrade(t, "", "--json", "--yes")
	c, h := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_ROOT" || h != "sudo cvctl upgrade" {
		t.Fatalf("code %d %s %q", code, c, h)
	}
	if f.installed(t) != "old binary" || len(f.run.Calls) != 0 || len(f.lock.Calls) != 0 {
		t.Fatal("must not touch anything without root")
	}
}

func TestUpgradeBusyWhenAnotherRunHoldsTheLock(t *testing.T) {
	for _, args := range [][]string{{"--yes", "--json"}, {"--check", "--json"}} {
		f := newUpFixture(t, upPlanA)
		unlock, err := f.lock.Lock(upLock)
		if err != nil {
			t.Fatal(err)
		}
		code, out, _ := runUpgrade(t, "", args...)
		unlock()
		c, h := errCode(t, out)
		if code == 0 || c != "E_UPGRADE_BUSY" || h == "" {
			t.Fatalf("%v: code %d %s %q", args, code, c, h)
		}
		if f.installed(t) != "old binary" || len(f.run.Calls) != 0 {
			t.Fatalf("%v: must not touch anything while locked: %v", args, f.run.Calls)
		}
	}
}

func TestUpgradeLockFailureIsNotBusy(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	sys.Lock = func(string) (func(), error) { return nil, os.ErrPermission }
	code, out, _ := runUpgrade(t, "", "--yes", "--json")
	c, _ := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_LOCK_FAILED" || f.installed(t) != "old binary" {
		t.Fatalf("code %d %s", code, c)
	}
}

func (f *upFixture) assertLocked(t *testing.T, when string) {
	t.Helper()
	unlock, err := f.lock.Lock(upLock)
	if err == nil {
		unlock()
		t.Errorf("lock free %s", when)
	} else if !errors.Is(err, sys.ErrLocked) {
		t.Error(err)
	}
}

func (f *upFixture) assertUnlocked(t *testing.T) {
	t.Helper()
	unlock, err := f.lock.Lock(upLock)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	unlock()
}

// Deploying runs in Prepare and in Apply, and the notice goes out after Apply.
func TestUpgradeHoldsLockFromPrepareUntilAfterApply(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	deploying := 0
	f.onDeploying = func() {
		deploying++
		f.assertLocked(t, "during Prepare or Apply")
	}
	noticed := 0
	f.notif.onSend = func() {
		noticed++
		f.assertLocked(t, "after Apply")
	}
	if code, _, errb := runUpgrade(t, "", "--yes"); code != 0 {
		t.Fatalf("code %d: %s", code, errb)
	}
	if deploying != 2 || noticed != 1 {
		t.Fatalf("deploying %d noticed %d", deploying, noticed)
	}
	f.assertUnlocked(t)
}

func TestUpgradeReleasesLockOnEveryExit(t *testing.T) {
	tests := []struct {
		name  string
		setup func(f *upFixture)
		args  []string
	}{
		{"check", func(*upFixture) {}, []string{"--check"}},
		{"refused", func(*upFixture) {}, []string{"--json"}},
		{"failed", func(f *upFixture) { f.run.On("systemctl restart cvctl-agent", sys.Result{ExitCode: 1}) }, []string{"--yes"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newUpFixture(t, upPlanA)
			tt.setup(f)
			runUpgrade(t, "", tt.args...)
			if len(f.lock.Calls) != 1 || f.lock.Calls[0].Path != upLock {
				t.Fatalf("locks: %+v", f.lock.Calls)
			}
			f.assertUnlocked(t)
		})
	}
}

func TestUpgradeCheckInstallsNothing(t *testing.T) {
	f := newUpFixture(t, upPlanB)
	code, out, _ := runUpgrade(t, "", "--check")
	if code != 0 || f.installed(t) != "old binary" || len(f.notif.msgs) != 0 {
		t.Fatalf("code %d installed %q notices %d", code, f.installed(t), len(f.notif.msgs))
	}
	for _, want := range []string{"v0.8.2", upNew, "notas de la release", "instances.cute.data.media.backup: volume -> none"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if _, err := f.fs.Stat(upTmp); err == nil {
		t.Fatal("staged binary not cleaned up")
	}
}

func TestUpgradePlanFailedStillShowsChanges(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	from := `{"ok":true,"data":{"schema":1,"instances":{"broken-dev":{"error":"E_X: unreadable"},"cute-prod":{"data":{"media":{"backup":"volume"}}}}}}`
	to := `{"ok":true,"data":{"schema":1,"instances":{"broken-dev":{"error":"E_X: unreadable"},"cute-prod":{"data":{"media":{"backup":"none"}}}}}}`
	f.run.On(host.AgentBinaryPath+" plan --json", sys.Result{Stdout: []byte(from)})
	f.run.On(upTmp+" plan --json", sys.Result{Stdout: []byte(to)})
	code, out, errb := runUpgrade(t, "", "--check")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb)
	}
	for _, want := range []string{"No se han podido comparar", "instances.cute-prod.data.media.backup: volume -> none"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Sin diferencias") {
		t.Fatalf("a failed comparison must not claim there are no differences:\n%s", out)
	}
}

func TestUpgradeCheckJSONInstallsNothing(t *testing.T) {
	f := newUpFixture(t, upPlanB)
	code, out, errb := runUpgrade(t, "", "--check", "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb)
	}
	var doc struct {
		OK   bool          `json:"ok"`
		Data upgradeReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not json: %q", out)
	}
	rep := doc.Data
	if rep.Repo != "o/r" {
		t.Fatalf("repo %q", rep.Repo)
	}
	if !doc.OK || rep.From != "v0.8.2" || rep.To != upNew || rep.Installed || rep.PlanFailed || rep.UpToDate {
		t.Fatalf("report: %+v", doc)
	}
	want := upgradeChange{Path: "instances.cute.data.media.backup", From: "volume", To: "none"}
	if len(rep.Changes) != 1 || rep.Changes[0] != want {
		t.Fatalf("changes: %+v", rep.Changes)
	}
	if f.installed(t) != "old binary" || len(f.notif.msgs) != 0 {
		t.Fatalf("installed %q notices %d", f.installed(t), len(f.notif.msgs))
	}
	for _, c := range f.run.Calls {
		if strings.HasPrefix(c.String(), "systemctl") {
			t.Fatalf("--check touched the agent: %s", c)
		}
	}
	for _, p := range []string{upTmp, host.AgentBinaryPath + ".previous"} {
		if _, err := f.fs.Stat(p); err == nil {
			t.Fatalf("%s left behind", p)
		}
	}
}

func TestUpgradeRepoPrecedence(t *testing.T) {
	cases := []struct {
		name, env, hostEnv, want string
		wantErr                  bool
	}{
		{name: "default without config", want: defaultUpgradeRepo},
		{name: "host config", hostEnv: "CVCTL_UPGRADE_REPO=fork/cvctl\n", want: "fork/cvctl"},
		{name: "environment wins", env: "env/cvctl", hostEnv: "CVCTL_UPGRADE_REPO=fork/cvctl\n", want: "env/cvctl"},
		{name: "config without the key", hostEnv: "CVCTL_HOST_NAME=box\n", want: defaultUpgradeRepo},
		{name: "invalid config", hostEnv: "CVCTL_UPGRADE_REPO=nope\n", wantErr: true},
		{name: "invalid environment", env: "nope", wantErr: true},
		{name: "environment with a query", env: "o/r?x=1", wantErr: true},
		{name: "environment skips invalid config", env: "env/cvctl", hostEnv: "CVCTL_UPGRADE_REPO=nope\n", want: "env/cvctl"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fsys := systest.NewFS()
			if c.hostEnv != "" {
				if err := fsys.WriteFile(hostCfgDir+"/host.env", []byte(c.hostEnv), 0o640, sys.Owner{}); err != nil {
					t.Fatal(err)
				}
			}
			got, err := upgradeRepo(func(string) string { return c.env }, fsys)
			if (err != nil) != c.wantErr || got != c.want {
				t.Fatalf("got %q, %v", got, err)
			}
		})
	}
}

func TestUpgradeReviewShowsRepo(t *testing.T) {
	newUpFixture(t, upPlanB)
	code, out, errb := runUpgrade(t, "", "--check")
	if code != 0 || !strings.Contains(out, "Repo de releases: o/r\n") {
		t.Fatalf("code %d out %q err %q", code, out, errb)
	}
}

func TestUpgradeExamplesUseSudo(t *testing.T) {
	for _, l := range strings.Split(newUpgradeCmd("v0.8.2").Example, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "sudo ") {
			t.Errorf("example without sudo: %q", l)
		}
	}
}

func TestUpgradeYesWithoutChanges(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	code, _, errb := runUpgrade(t, "", "--yes")
	if code != 0 || f.installed(t) != "new binary" {
		t.Fatalf("code %d installed %q: %s", code, f.installed(t), errb)
	}
}

func TestUpgradeYesWithChangesRefuses(t *testing.T) {
	f := newUpFixture(t, upPlanB)
	code, out, _ := runUpgrade(t, "", "--yes", "--json")
	c, _ := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_CONFIRM" || f.installed(t) != "old binary" {
		t.Fatalf("code %d %s installed %q", code, c, f.installed(t))
	}
	if _, err := f.fs.Stat(upTmp); err == nil {
		t.Fatal("staged binary not cleaned up")
	}
}

func TestUpgradePlanFailedNeedsStrongConfirmation(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.run.On(upTmp+" plan --json", sys.Result{ExitCode: 1})
	code, out, _ := runUpgrade(t, "", "--yes", "--json")
	c, _ := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_CONFIRM" || f.installed(t) != "old binary" {
		t.Fatalf("code %d %s", code, c)
	}
}

func TestUpgradeAcceptChanges(t *testing.T) {
	f := newUpFixture(t, upPlanB)
	code, _, errb := runUpgrade(t, "", "--yes", "--accept-changes")
	if code != 0 || f.installed(t) != "new binary" {
		t.Fatalf("code %d installed %q: %s", code, f.installed(t), errb)
	}
}

func TestUpgradeTypedVersionConfirms(t *testing.T) {
	f := newUpFixture(t, upPlanB)
	f.tty = true
	code, _, _ := runUpgrade(t, "y\n")
	if code == 0 || f.installed(t) != "old binary" {
		t.Fatalf("y must not confirm a change: code %d", code)
	}
	code, _, errb := runUpgrade(t, upNew+"\n")
	if code != 0 || f.installed(t) != "new binary" {
		t.Fatalf("typed version must confirm: code %d: %s", code, errb)
	}
}

func TestUpgradeTerminalYesWithChangesStillNeedsTypedVersion(t *testing.T) {
	f := newUpFixture(t, upPlanB)
	f.tty = true
	code, _, errb := runUpgrade(t, "y\n", "--yes")
	if code == 0 || f.installed(t) != "old binary" {
		t.Fatalf("--yes must not confirm a change: code %d installed %q", code, f.installed(t))
	}
	if !strings.Contains(errb, "Escribe "+upNew) {
		t.Fatalf("prompt lacks the version: %q", errb)
	}
	code, _, errb = runUpgrade(t, upNew+"\n", "--yes")
	if code != 0 || f.installed(t) != "new binary" {
		t.Fatalf("typed version must confirm: code %d: %s", code, errb)
	}
}

func TestUpgradeYesAnswersPlainPrompt(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.tty = true
	if code, _, _ := runUpgrade(t, "n\n"); code == 0 || f.installed(t) != "old binary" {
		t.Fatal("n must cancel")
	}
	if code, _, _ := runUpgrade(t, "y\n"); code != 0 || f.installed(t) != "new binary" {
		t.Fatal("y must confirm")
	}
}

func TestUpgradeNoTerminalRefuses(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	code, out, _ := runUpgrade(t, "y\n", "--json")
	c, _ := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_CONFIRM" || f.installed(t) != "old binary" {
		t.Fatalf("code %d %s", code, c)
	}
}

func TestUpgradeNotifiesVersions(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	if code, _, _ := runUpgrade(t, "", "--yes"); code != 0 {
		t.Fatal("upgrade failed")
	}
	if len(f.notif.msgs) != 1 {
		t.Fatalf("notices: %+v", f.notif.msgs)
	}
	m := f.notif.msgs[0]
	if m.Status != "upgraded v0.8.2 -> v0.8.3" || m.Level != "info" || m.Project != "" || m.Deploy != nil {
		t.Fatalf("notice: %+v", m)
	}
}

func TestUpgradeNotifyFailureIsNotFatal(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.notif.err = errors.New("telegram down")
	code, _, _ := runUpgrade(t, "", "--yes")
	if code != 0 || f.installed(t) != "new binary" {
		t.Fatalf("code %d", code)
	}
	if !strings.Contains(f.logBuf.String(), "telegram down") {
		t.Fatalf("failure not logged: %q", f.logBuf.String())
	}
}

func TestUpgradeSaysAgentWasNotStarted(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.run.On("systemctl is-active cvctl-agent", sys.Result{ExitCode: 3, Stdout: []byte("inactive\n")})
	code, out, errb := runUpgrade(t, "", "--yes")
	if code != 0 || f.installed(t) != "new binary" {
		t.Fatalf("code %d installed %q: %s", code, f.installed(t), errb)
	}
	if !strings.Contains(out, "no se ha arrancado") {
		t.Fatalf("output lacks the agent state:\n%s", out)
	}
	for _, c := range f.run.Calls {
		if c.String() == "systemctl restart cvctl-agent" {
			t.Fatal("restarted a stopped agent")
		}
	}
	if len(f.notif.msgs) != 1 {
		t.Fatalf("notices: %+v", f.notif.msgs)
	}
	if m := f.notif.msgs[0]; !strings.Contains(m.Status, "upgraded v0.8.2 -> v0.8.3") || !strings.Contains(m.Status, "not started") {
		t.Fatalf("notice: %+v", m)
	}
}

func TestUpgradeJSONReportsStoppedAgent(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.run.On("systemctl is-active cvctl-agent", sys.Result{ExitCode: 3, Stdout: []byte("inactive\n")})
	code, out, errb := runUpgrade(t, "", "--yes", "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, errb)
	}
	var doc struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not json: %q", out)
	}
	if doc.Data["installed"] != true || doc.Data["agent_stopped"] != true {
		t.Fatalf("report: %s", out)
	}
}

func TestUpgradeFailureNotifiesError(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.run.On("systemctl restart cvctl-agent", sys.Result{ExitCode: 1})
	code, out, _ := runUpgrade(t, "", "--yes", "--json")
	c, _ := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_FAILED" || f.installed(t) != "old binary" {
		t.Fatalf("code %d %s installed %q", code, c, f.installed(t))
	}
	if len(f.notif.msgs) != 1 {
		t.Fatalf("notices: %+v", f.notif.msgs)
	}
	m := f.notif.msgs[0]
	if m.Level != "error" || m.Project != "" || !strings.Contains(m.Status, "v0.8.2") ||
		!strings.Contains(m.Status, upNew) || !strings.Contains(m.Status, "restor") {
		t.Fatalf("notice: %+v", m)
	}
}

func TestUpgradeInstallFailureDoesNotClaimRestore(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	f.run.On("systemctl show -p NRestarts --value cvctl-agent", sys.Result{ExitCode: 1})
	code, out, _ := runUpgrade(t, "", "--yes", "--json")
	c, _ := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_INSTALL" || len(f.notif.msgs) != 1 {
		t.Fatalf("code %d %s notices %d", code, c, len(f.notif.msgs))
	}
	m := f.notif.msgs[0]
	if m.Level != "error" || strings.Contains(m.Status, "restor") || !strings.Contains(m.Status, "nothing installed") {
		t.Fatalf("notice: %+v", m)
	}
}

func TestUpgradeAlreadyCurrent(t *testing.T) {
	f := newUpFixture(t, upPlanA)
	var out, errb bytes.Buffer
	root := NewRoot(upNew)
	code := Execute(root, []string{"upgrade", "--yes"}, &out, &errb)
	if code != 0 || f.installed(t) != "old binary" || len(f.notif.msgs) != 0 {
		t.Fatalf("code %d", code)
	}
}
