package cli

import (
	"bytes"
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

func stubInit(t *testing.T, fs *systest.FS, p hostcfg.Prompter, interactive bool, steps ...host.Step) {
	t.Helper()
	oldEnv, oldSteps := newInitEnv, initSteps
	t.Cleanup(func() { newInitEnv, initSteps = oldEnv, oldSteps })
	newInitEnv = func(o initOpts) (*host.Env, hostcfg.Facts, error) {
		return &host.Env{FS: fs, Prompt: p, Interactive: interactive && !o.NonInteractive, Adopt: o.Adopt},
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
	_ = code
	var r struct {
		OK   bool `json:"ok"`
		Data struct {
			Host    *string          `json:"host"`
			Steps   []map[string]any `json:"steps"`
			Pending []string         `json:"pending"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if bytes.Count([]byte(out), []byte("\n")) != 1 {
		t.Fatalf("not one document: %q", out)
	}
	if len(r.Data.Steps) != 2 || r.Data.Steps[0]["status"] != "changed" || r.Data.Steps[1]["status"] != "pending" {
		t.Fatalf("steps: %+v", r.Data.Steps)
	}
	if len(r.Data.Pending) != 1 || r.Data.Pending[0] != "open the port" {
		t.Fatalf("pending: %+v", r.Data.Pending)
	}
}

func TestInitJSONFailedExitAndNonNullArrays(t *testing.T) {
	fs := seeded()
	stubInit(t, fs, failPrompter{t}, true, stubStep{name: "a", fail: true})
	code, out, _ := run(t, "init", "--json", "--non-interactive")
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, `"pending":[]`) || !strings.Contains(out, `"code":"E_STEP_FAILED"`) {
		t.Fatalf("out: %s", out)
	}
}
