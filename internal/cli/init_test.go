package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type stubStep struct {
	name string
	fail bool
	pend string
}

func (s stubStep) Name() string { return s.name }
func (s stubStep) Check(context.Context, *host.Env) (host.Plan, error) {
	return host.Plan{Changes: []string{"do " + s.name}}, nil
}
func (s stubStep) Apply(context.Context, *host.Env, host.Plan) (string, error) {
	if s.fail {
		return "", errors.New("boom")
	}
	return s.pend, nil
}

type failPrompter struct{ t *testing.T }

func (f failPrompter) Ask(q hostcfg.Question, _ string) (string, error) {
	f.t.Fatalf("unexpected question %q", q.Key)
	return "", nil
}

var initAnswers = map[string]string{
	"HOST_NAME": "box", "EDGE_MODE": "behind-proxy", "PROXY_PORT": "8081", "ADMIN_USER": "andres",
	"SSH_MANAGE": "true", "FIREWALL_MANAGE": "true", "STORAGE": "local", "NOTIFY": "none",
	"REGISTRIES": "ghcr.io/x", "REGISTRY_USER": "u", "registry_token": "tok",
}

var testEUID = 0

func stubInit(t *testing.T, fs *systest.FS, p hostcfg.Prompter, interactive bool, steps ...host.Step) {
	t.Helper()
	if _, err := fs.ReadFile("/etc/os-release"); err != nil {
		_ = fs.WriteFile("/etc/os-release", []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\n"), 0o644, sys.Owner{User: "root", Group: "root"})
	}
	oldEnv, oldSteps := newInitEnv, initSteps
	t.Cleanup(func() { newInitEnv, initSteps = oldEnv, oldSteps })
	newInitEnv = func(o initOpts) (*host.Env, hostcfg.Facts, error) {
		return &host.Env{FS: fs, Prompt: p, Interactive: interactive && !o.NonInteractive, Adopt: o.Adopt, EUID: testEUID},
			hostcfg.Facts{Hostname: "box", SudoUser: "andres", Adopt: o.Adopt}, nil
	}
	initSteps = func() []host.Step { return steps }
}

func TestInitSavesAnswersBeforeSteps(t *testing.T) {
	fs := systest.NewFS()
	stubInit(t, fs, hostcfgtest.NewPrompter(t, initAnswers), true, stubStep{name: "boom", fail: true})
	code, _, _ := run(t, "init")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, err := fs.ReadFile("/etc/cvctl/host.env")
	if err != nil || !strings.Contains(string(b), "CVCTL_HOST_NAME=box") {
		t.Fatalf("host.env: %q %v", b, err)
	}

	stubInit(t, fs, failPrompter{t}, true, stubStep{name: "boom", fail: true})
	if code, _, _ := run(t, "init"); code != 1 {
		t.Fatalf("second init exit %d", code)
	}
}

func seeded() *systest.FS {
	fs := systest.NewFS()
	_ = fs.WriteFile("/etc/cvctl/host.env", []byte("CVCTL_HOST_NAME=box\nCVCTL_EDGE_MODE=behind-proxy\nCVCTL_PROXY_PORT=8081\nCVCTL_ADMIN_USER=andres\nCVCTL_SSH_MANAGE=true\nCVCTL_FIREWALL_MANAGE=true\nCVCTL_STORAGE=local\nCVCTL_NOTIFY=none\n"), 0o640, sys.Owner{User: "root", Group: "root"})
	return fs
}

func TestInitJSONShape(t *testing.T) {
	fs := seeded()
	stubInit(t, fs, failPrompter{t}, true, stubStep{name: "a"}, stubStep{name: "b", pend: "open the port"})
	code, out, _ := run(t, "init", "--json")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	var r struct {
		OK   bool `json:"ok"`
		Data struct {
			Host    string           `json:"host"`
			Steps   []map[string]any `json:"steps"`
			Pending []string         `json:"pending"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Count(out, "\n") != 1 || !r.OK || r.Data.Host != "box" {
		t.Fatalf("bad document: %q", out)
	}
	if len(r.Data.Steps) != 2 || r.Data.Steps[0]["status"] != "changed" || r.Data.Steps[1]["status"] != "pending" {
		t.Fatalf("steps: %+v", r.Data.Steps)
	}
	if len(r.Data.Pending) != 1 || r.Data.Pending[0] != "open the port" {
		t.Fatalf("pending: %+v", r.Data.Pending)
	}
}

func TestInitJSONFailedStep(t *testing.T) {
	fs := seeded()
	stubInit(t, fs, failPrompter{t}, true, stubStep{name: "a", fail: true}, stubStep{name: "b"})
	code, out, _ := run(t, "init", "--json", "--non-interactive")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	var r struct {
		OK    bool                                 `json:"ok"`
		Error struct{ Code, Message, Hint string } `json:"error"`
		Data  struct {
			Steps   []map[string]any `json:"steps"`
			Pending []string         `json:"pending"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Count(out, "\n") != 1 || r.OK || r.Error.Code != "E_INIT_STEP_FAILED" || r.Error.Hint == "" ||
		!strings.Contains(r.Error.Message, "step a failed") {
		t.Fatalf("bad envelope: %s", out)
	}
	if len(r.Data.Steps) != 2 || r.Data.Steps[0]["status"] != "failed" || r.Data.Steps[1]["status"] != "skipped" || r.Data.Pending == nil {
		t.Fatalf("data: %s", out)
	}
}

func TestInitFailedStepHuman(t *testing.T) {
	stubInit(t, seeded(), failPrompter{t}, true, stubStep{name: "a", fail: true})
	code, out, errs := run(t, "init", "--non-interactive")
	if code != 1 || !strings.Contains(out, "failed") || !strings.Contains(errs, "error E_INIT_STEP_FAILED") || !strings.Contains(errs, "hint:") {
		t.Fatalf("code %d out %q err %q", code, out, errs)
	}
}

func TestInitNotRootFailsBeforeConfig(t *testing.T) {
	fs := systest.NewFS()
	testEUID = 1000
	t.Cleanup(func() { testEUID = 0 })
	stubInit(t, fs, failPrompter{t}, true, stubStep{name: "a"})
	code, out, _ := run(t, "init", "--json")
	if code != 1 || !strings.Contains(out, `"E_HOST_NOT_ROOT"`) {
		t.Fatalf("code %d out %s", code, out)
	}
	if _, err := fs.ReadFile("/etc/cvctl/host.env"); err == nil {
		t.Fatal("host.env written")
	}
	if len(fs.Files()) != 1 { // only the seeded os-release
		t.Fatalf("unexpected writes: %v", fs.Files())
	}
}

func TestInitDryRunDoesNotSave(t *testing.T) {
	fs := systest.NewFS()
	stubInit(t, fs, hostcfgtest.NewPrompter(t, initAnswers), true, stubStep{name: "a", pend: "x"})
	code, out, _ := run(t, "init", "--dry-run")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if _, err := fs.ReadFile("/etc/cvctl/host.env"); err == nil {
		t.Fatal("dry-run wrote host.env")
	}
	if !strings.Contains(out, "planned") {
		t.Fatalf("out %q", out)
	}
}

func TestInitDryRunPendingAggregated(t *testing.T) {
	stubInit(t, seeded(), failPrompter{t}, true, pendPlanStep{})
	_, out, _ := run(t, "init", "--json", "--dry-run")
	if !strings.Contains(out, `"pending":["confirm ssh"]`) {
		t.Fatalf("out %s", out)
	}
}

type pendPlanStep struct{}

func (pendPlanStep) Name() string { return "p" }
func (pendPlanStep) Check(context.Context, *host.Env) (host.Plan, error) {
	return host.Plan{Changes: []string{"c"}, Pending: "confirm ssh"}, nil
}
func (pendPlanStep) Apply(context.Context, *host.Env, host.Plan) (string, error) { return "", nil }

func TestConfirmSSHCommand(t *testing.T) {
	fs := systest.NewFS()
	_ = fs.WriteFile("/etc/os-release", []byte("ID=ubuntu\nVERSION_ID=\"24.04\"\n"), 0o644, sys.Owner{User: "root", Group: "root"})
	_ = fs.WriteFile("/etc/ssh/sshd_config.d/00-cvctl.conf", []byte("PasswordAuthentication no\n"), 0o644, sys.Owner{User: "root", Group: "root"})
	runner := &systest.Runner{}
	runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{ExitCode: 0})
	runner.On("systemctl is-active cvctl-ssh-revert.timer", sys.Result{Stdout: []byte("inactive\n"), ExitCode: 3})

	oldEnv := newInitEnv
	t.Cleanup(func() { newInitEnv = oldEnv })
	newInitEnv = func(o initOpts) (*host.Env, hostcfg.Facts, error) {
		return &host.Env{Run: runner, FS: fs, EUID: testEUID}, hostcfg.Facts{}, nil
	}

	code, out, errs := run(t, "init", "confirm-ssh")
	if code != 0 {
		t.Fatalf("exit %d: out=%q err=%q", code, out, errs)
	}
	if !strings.Contains(out, "ssh change confirmed") {
		t.Fatalf("expected 'ssh change confirmed' in stdout, got %q", out)
	}
	if len(runner.Calls) == 0 || !strings.HasPrefix(runner.Calls[0].String(), "systemctl stop cvctl-ssh-revert.timer") {
		t.Fatalf("expected systemctl stop cvctl-ssh-revert.timer call, got %v", runner.Calls)
	}
}
