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
	upPlanA = `{"ok":true,"data":{"schema":1,"instances":{"cute":{"data":{"media":{"backup":"volume"}}}}}}`
	upPlanB = `{"ok":true,"data":{"schema":1,"instances":{"cute":{"data":{"media":{"backup":"none"}}}}}}`
)

type recNotifier struct {
	msgs []notify.Message
	err  error
}

func (r *recNotifier) Send(_ context.Context, m notify.Message) error {
	r.msgs = append(r.msgs, m)
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

	f := &upFixture{run: &systest.Runner{}, fs: systest.NewFS(), notif: &recNotifier{}, logBuf: &bytes.Buffer{}}
	if err := f.fs.WriteFile(host.AgentBinaryPath, []byte("old binary"), 0o755, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	f.run.On(upTmp+" version --json", sys.Result{Stdout: []byte(`{"ok":true,"data":{"version":"` + upNew + `"}}`)})
	f.run.On(upTmp+" plan --json", sys.Result{Stdout: []byte(planNew)})
	f.run.On(host.AgentBinaryPath+" plan --json", sys.Result{Stdout: []byte(upPlanA)})
	f.run.On("systemctl show -p NRestarts --value cvctl-agent", sys.Result{Stdout: []byte("0\n")})
	f.run.On("systemctl is-active cvctl-agent", sys.Result{Stdout: []byte("active\n")})
	f.run.On("systemctl restart cvctl-agent", sys.Result{})

	oldEnv, oldLog := newUpgradeEnv, notifyLog
	notifyLog = f.logBuf
	newUpgradeEnv = func(current string) (*upgradeEnv, error) {
		return &upgradeEnv{
			Upgrader: &upgrade.Upgrader{
				Run: f.run, FS: f.fs,
				Client:    upgrade.Client{BaseURL: srv.URL, Repo: "o/r"},
				Arch:      "amd64",
				Current:   current,
				Exe:       host.AgentBinaryPath,
				Deploying: func() ([]string, error) { return f.busy, nil },
				Sleep:     func(_ time.Duration) {},
			},
			EUID:        f.euid,
			Interactive: f.tty,
			Notifier:    func() notify.Notifier { return f.notif },
		}, nil
	}
	t.Cleanup(func() { newUpgradeEnv, notifyLog = oldEnv, oldLog })
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
	code, out, _ := runUpgrade(t, "", "--json", "--yes")
	c, h := errCode(t, out)
	if code == 0 || c != "E_UPGRADE_ROOT" || h != "sudo cvctl upgrade" {
		t.Fatalf("code %d %s %q", code, c, h)
	}
	if f.installed(t) != "old binary" || len(f.run.Calls) != 0 {
		t.Fatal("must not touch anything without root")
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
