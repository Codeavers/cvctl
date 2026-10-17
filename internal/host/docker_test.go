package host

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func newDockerTestEnv() (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	return &Env{Run: r, FS: fs, EUID: 0}, r, fs
}

func TestDaemonMergeKeepsKeys(t *testing.T) {
	env, runner, fs := newDockerTestEnv()
	initialJSON := `{"data-root":"/x"}`
	_ = fs.WriteFile("/etc/docker/daemon.json", []byte(initialJSON), 0o644, sys.Owner{User: "root", Group: "root"})

	runner.On("docker ps -q", sys.Result{Stdout: []byte(""), ExitCode: 0})
	runner.On("systemctl restart docker", sys.Result{ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes to update daemon.json, got none")
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("unexpected Apply result: pending=%q err=%v", pending, err)
	}

	data, err := fs.ReadFile("/etc/docker/daemon.json")
	if err != nil {
		t.Fatalf("failed to read /etc/docker/daemon.json: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("daemon.json is not valid JSON: %v", err)
	}

	if m["data-root"] != "/x" {
		t.Fatalf("expected data-root to be preserved as /x, got %v", m["data-root"])
	}
	if m["log-driver"] != "local" {
		t.Fatalf("expected log-driver local, got %v", m["log-driver"])
	}
	if m["live-restore"] != true {
		t.Fatalf("expected live-restore true, got %v", m["live-restore"])
	}
	logOpts, ok := m["log-opts"].(map[string]any)
	if !ok {
		t.Fatalf("expected log-opts to be map[string]any, got %T", m["log-opts"])
	}
	if logOpts["max-size"] != "20m" || logOpts["max-file"] != "5" {
		t.Fatalf("unexpected log-opts: %v", logOpts)
	}

	fi, err := fs.Stat("/etc/docker/daemon.json")
	if err != nil {
		t.Fatalf("stat failed: %v", err)
	}
	if fi.Mode != 0o644 {
		t.Fatalf("expected mode 0644, got %o", fi.Mode)
	}
}

func TestDaemonInvalidJSON(t *testing.T) {
	env, _, fs := newDockerTestEnv()
	raw := []byte(`{invalid-json: true`)
	_ = fs.WriteFile("/etc/docker/daemon.json", raw, 0o644, sys.Owner{User: "root", Group: "root"})

	step := DockerDaemonStep{}
	_, err := step.Check(context.Background(), env)
	if err == nil {
		t.Fatal("expected error on invalid daemon.json, got nil")
	}
	if c := code(t, err); c != "E_DOCKER_DAEMON_JSON" {
		t.Fatalf("expected error code E_DOCKER_DAEMON_JSON, got %s", c)
	}

	data, err := fs.ReadFile("/etc/docker/daemon.json")
	if err != nil {
		t.Fatalf("failed to read /etc/docker/daemon.json: %v", err)
	}
	if string(data) != string(raw) {
		t.Fatal("file should not have been overwritten")
	}
}

func TestDaemonPendingWithContainersNonInteractive(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	env.Interactive = false
	runner.On("docker ps -q", sys.Result{Stdout: []byte("c123456\n"), ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	wantPending := "daemon.json updated; run: systemctl restart docker (running containers will restart)"
	if pending != wantPending {
		t.Fatalf("got pending %q, want %q", pending, wantPending)
	}

	for _, c := range runner.Calls {
		if c.String() == "systemctl restart docker" {
			t.Fatal("systemctl restart docker should not have been called")
		}
	}
}

func TestDaemonRestartsWhenNoContainers(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	runner.On("docker ps -q", sys.Result{Stdout: []byte(""), ExitCode: 0})
	runner.On("systemctl restart docker", sys.Result{ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending, got %q", pending)
	}

	found := false
	for _, c := range runner.Calls {
		if c.String() == "systemctl restart docker" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected systemctl restart docker to be called")
	}
}

func TestDaemonUnchanged(t *testing.T) {
	env, runner, fs := newDockerTestEnv()
	content := `{
  "log-driver": "local",
  "log-opts": {
    "max-file": "5",
    "max-size": "20m"
  },
  "live-restore": true
}
`
	_ = fs.WriteFile("/etc/docker/daemon.json", []byte(content), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("docker info --format {{.LoggingDriver}} {{.LiveRestoreEnabled}}", sys.Result{Stdout: []byte("local true\n"), ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected unchanged step with 0 changes, got %v", plan.Changes)
	}
	if plan.Pending != "" {
		t.Fatalf("expected empty pending, got %q", plan.Pending)
	}
}

func TestDaemonNeedsRestartWhenRunningStateMismatch(t *testing.T) {
	env, runner, fs := newDockerTestEnv()
	content := `{
  "log-driver": "local",
  "log-opts": {
    "max-file": "5",
    "max-size": "20m"
  },
  "live-restore": true
}
`
	_ = fs.WriteFile("/etc/docker/daemon.json", []byte(content), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("docker info --format {{.LoggingDriver}} {{.LiveRestoreEnabled}}", sys.Result{Stdout: []byte("json-file false\n"), ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected 0 changes, got %v", plan.Changes)
	}
	wantPending := "daemon.json updated; run: systemctl restart docker (running containers will restart)"
	if plan.Pending != wantPending {
		t.Fatalf("got pending %q, want %q", plan.Pending, wantPending)
	}
}

func TestDaemonInteractiveRestartYes(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	env.Interactive = true
	env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
		"confirm_docker_restart": "yes",
	})
	runner.On("docker ps -q", sys.Result{Stdout: []byte("c123\n"), ExitCode: 0})
	runner.On("systemctl restart docker", sys.Result{ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending on confirmation, got %q", pending)
	}

	if !slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool { return c.String() == "systemctl restart docker" }) {
		t.Fatal("expected systemctl restart docker to be called")
	}
}

func TestDaemonInteractiveRestartNo(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	env.Interactive = true
	env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
		"confirm_docker_restart": "no",
	})
	runner.On("docker ps -q", sys.Result{Stdout: []byte("c123\n"), ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	wantPending := "daemon.json updated; run: systemctl restart docker (running containers will restart)"
	if pending != wantPending {
		t.Fatalf("got pending %q, want %q", pending, wantPending)
	}

	if slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool { return c.String() == "systemctl restart docker" }) {
		t.Fatal("systemctl restart docker should not be called when user answers no")
	}
}

func TestDaemonAdoptNoContainersNonInteractive(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	env.Adopt = true
	env.Interactive = false
	runner.On("docker ps -q", sys.Result{Stdout: []byte(""), ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	wantPending := "daemon.json updated; run: systemctl restart docker (running containers will restart)"
	if pending != wantPending {
		t.Fatalf("got pending %q, want %q", pending, wantPending)
	}

	for _, c := range runner.Calls {
		if c.String() == "systemctl restart docker" {
			t.Fatal("systemctl restart docker should NOT be called on adopted host non-interactively")
		}
	}
}

func TestDaemonAdoptInteractiveRestartYes(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	env.Adopt = true
	env.Interactive = true
	env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
		"confirm_docker_restart": "yes",
	})
	runner.On("docker ps -q", sys.Result{Stdout: []byte(""), ExitCode: 0})
	runner.On("systemctl restart docker", sys.Result{ExitCode: 0})

	step := DockerDaemonStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending on confirmation, got %q", pending)
	}

	if !slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool { return c.String() == "systemctl restart docker" }) {
		t.Fatal("expected systemctl restart docker to be called on adopted host when confirmed")
	}
}

func TestDaemonDockerPsFailure(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	runner.On("docker ps -q", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n"),
	})

	step := DockerDaemonStep{}
	plan, _ := step.Check(context.Background(), env)
	_, err := step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected error on docker ps failure, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_DOCKER_PS_FAILED" {
		t.Errorf("got code %q, want E_DOCKER_PS_FAILED", oe.Code)
	}
	if oe.Hint != "systemctl status docker" {
		t.Errorf("got hint %q, want 'systemctl status docker'", oe.Hint)
	}
	if !strings.Contains(oe.Message, "Cannot connect to the Docker daemon") {
		t.Errorf("expected stderr in message, got %q", oe.Message)
	}
}

func TestDaemonSystemctlFailure(t *testing.T) {
	env, runner, _ := newDockerTestEnv()
	runner.On("docker ps -q", sys.Result{Stdout: []byte(""), ExitCode: 0})
	runner.On("systemctl restart docker", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("Job for docker.service failed because the control process exited with error code.\n"),
	})

	step := DockerDaemonStep{}
	plan, _ := step.Check(context.Background(), env)
	_, err := step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected error on systemctl failure, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_DOCKER_RESTART_FAILED" {
		t.Errorf("got code %q, want E_DOCKER_RESTART_FAILED", oe.Code)
	}
	if oe.Hint != "check systemctl status docker" {
		t.Errorf("got hint %q, want 'check systemctl status docker'", oe.Hint)
	}
	if !strings.Contains(oe.Message, "Job for docker.service failed") {
		t.Errorf("expected stderr in message, got %q", oe.Message)
	}
}

func TestDefaultStepsRegistration(t *testing.T) {
	steps := DefaultSteps()
	if len(steps) < 3 {
		t.Fatalf("expected at least 3 steps, got %d", len(steps))
	}
	if steps[0].Name() != "preflight" {
		t.Errorf("step 0: got %s, want preflight", steps[0].Name())
	}
	if steps[1].Name() != "packages" {
		t.Errorf("step 1: got %s, want packages", steps[1].Name())
	}
	if steps[2].Name() != "docker-daemon" {
		t.Errorf("step 2: got %s, want docker-daemon", steps[2].Name())
	}
}
