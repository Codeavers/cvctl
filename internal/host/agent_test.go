package host_test

import (
	"context"
	"os"
	"testing"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const goldenUnitContent = `[Unit]
Description=cvctl deploy agent
After=network.target

[Service]
User=deployer
Group=deploy
ExecStart=/usr/local/bin/cvctl agent
Restart=always
RestartSec=10
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/srv/deploy /var/lib/cvctl /etc/traefik/dynamic
PrivateTmp=yes
SupplementaryGroups=docker
Environment=DOCKER_CONFIG=/var/lib/cvctl/docker
TimeoutStopSec=420

[Install]
WantedBy=multi-user.target
`

func TestAgentStepUnitContent(t *testing.T) {
	step := host.AgentStep{}
	content := step.UnitContent()
	if content != goldenUnitContent {
		t.Fatalf("unit content differs from golden:\ngot:\n%s\nwant:\n%s", content, goldenUnitContent)
	}
}

func TestAgentStepRestartsOnBinaryChange(t *testing.T) {
	fsys := systest.NewFS()
	run := &systest.Runner{}

	exePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	newBinary := []byte("new-cvctl-binary-v2")
	oldBinary := []byte("old-cvctl-binary-v1")

	// Binary on disk at os.Executable()
	if err := fsys.WriteFile(exePath, newBinary, 0o755, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	// Binary installed at /usr/local/bin/cvctl
	if err := fsys.WriteFile("/usr/local/bin/cvctl", oldBinary, 0o755, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	// Unit file already matches golden
	if err := fsys.WriteFile("/etc/systemd/system/cvctl-agent.service", []byte(goldenUnitContent), 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}

	// Service is enabled and active
	run.On("systemctl is-enabled cvctl-agent", sys.Result{Stdout: []byte("enabled\n"), ExitCode: 0})
	run.On("systemctl is-active cvctl-agent", sys.Result{Stdout: []byte("active\n"), ExitCode: 0})
	run.On("systemctl daemon-reload", sys.Result{ExitCode: 0})
	run.On("systemctl try-restart cvctl-agent", sys.Result{ExitCode: 0})

	e := &host.Env{
		Run: run,
		FS:  fsys,
	}

	step := host.AgentStep{}
	ctx := context.Background()

	plan, err := step.Check(ctx, e)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}

	if len(plan.Changes) == 0 {
		t.Fatal("expected changes due to binary difference")
	}

	pending, err := step.Apply(ctx, e, plan)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if pending != "" {
		t.Fatalf("unexpected pending note: %s", pending)
	}

	// Verify binary was updated
	installed, err := fsys.ReadFile("/usr/local/bin/cvctl")
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(installed) != string(newBinary) {
		t.Fatalf("binary not updated, got %q, want %q", string(installed), string(newBinary))
	}

	// Verify try-restart was called
	foundRestart := false
	for _, call := range run.Calls {
		if call.String() == "systemctl try-restart cvctl-agent" {
			foundRestart = true
			break
		}
	}
	if !foundRestart {
		t.Fatalf("expected systemctl try-restart cvctl-agent in calls: %v", run.Calls)
	}
}

func TestAgentStepEnsuresDockerDir(t *testing.T) {
	fsys := systest.NewFS()
	run := &systest.Runner{}

	exePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("cvctl-binary")
	if err := fsys.WriteFile(exePath, binary, 0o755, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/usr/local/bin/cvctl", binary, 0o755, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	if err := fsys.WriteFile("/etc/systemd/system/cvctl-agent.service", []byte(goldenUnitContent), 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}

	run.On("systemctl is-enabled cvctl-agent", sys.Result{Stdout: []byte("enabled\n"), ExitCode: 0})
	run.On("systemctl is-active cvctl-agent", sys.Result{Stdout: []byte("active\n"), ExitCode: 0})
	run.On("systemctl daemon-reload", sys.Result{ExitCode: 0})

	e := &host.Env{
		Run: run,
		FS:  fsys,
	}

	step := host.AgentStep{}
	ctx := context.Background()

	plan, err := step.Check(ctx, e)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}

	foundDirChange := false
	for _, c := range plan.Changes {
		if c == "directory /var/lib/cvctl/docker" {
			foundDirChange = true
			break
		}
	}
	if !foundDirChange {
		t.Fatalf("expected directory /var/lib/cvctl/docker in plan changes, got: %v", plan.Changes)
	}

	if _, err := step.Apply(ctx, e, plan); err != nil {
		t.Fatalf("Apply failed: %v", err)
	}

	fi, err := fsys.Stat("/var/lib/cvctl/docker")
	if err != nil {
		t.Fatalf("expected /var/lib/cvctl/docker to exist: %v", err)
	}
	if !fi.IsDir {
		t.Fatalf("expected /var/lib/cvctl/docker to be a directory")
	}
	if fi.Mode != 0o750 {
		t.Fatalf("expected mode 0750, got %#o", fi.Mode)
	}
	if fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("expected owner deployer:deploy, got %s:%s", fi.User, fi.Group)
	}
}

func agentUpToDate(t *testing.T) (*systest.FS, *systest.Runner) {
	t.Helper()
	fsys := systest.NewFS()
	run := &systest.Runner{}
	exePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := sys.Owner{User: "root", Group: "root"}
	for _, p := range []string{exePath, "/usr/local/bin/cvctl"} {
		if err := fsys.WriteFile(p, []byte("cvctl-binary"), 0o755, root); err != nil {
			t.Fatal(err)
		}
	}
	if err := fsys.WriteFile("/etc/systemd/system/cvctl-agent.service", []byte(goldenUnitContent), 0o644, root); err != nil {
		t.Fatal(err)
	}
	if err := fsys.MkdirAll("/var/lib/cvctl/docker", 0o750, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatal(err)
	}
	run.On("systemctl is-enabled cvctl-agent", sys.Result{Stdout: []byte("enabled\n")})
	run.On("systemctl is-active cvctl-agent", sys.Result{Stdout: []byte("active\n")})
	run.On("systemctl daemon-reload", sys.Result{})
	return fsys, run
}

func TestAgentStepRestartsOnNotifierChange(t *testing.T) {
	fsys, run := agentUpToDate(t)
	e := &host.Env{Run: run, FS: fsys}
	plan, err := host.AgentStep{}.Check(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("no notifier change: changes %v", plan.Changes)
	}

	committed := 0
	e.NotifierCommit = func() error { committed++; return nil }
	plan, err = host.AgentStep{}.Check(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0] != "restart cvctl-agent" {
		t.Fatalf("changes %v", plan.Changes)
	}
	run.On("systemctl try-restart cvctl-agent", sys.Result{})
	if _, err := (host.AgentStep{}).Apply(context.Background(), e, plan); err != nil {
		t.Fatal(err)
	}
	if committed != 1 || e.NotifierCommit != nil {
		t.Fatalf("committed %d times, pending %v", committed, e.NotifierCommit != nil)
	}
}

func TestAgentStepFailedRestartKeepsNotifierPending(t *testing.T) {
	fsys, run := agentUpToDate(t)
	committed := false
	e := &host.Env{Run: run, FS: fsys, NotifierCommit: func() error { committed = true; return nil }}
	plan, err := host.AgentStep{}.Check(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	run.On("systemctl try-restart cvctl-agent", sys.Result{ExitCode: 1})
	if _, err := (host.AgentStep{}).Apply(context.Background(), e, plan); err == nil {
		t.Fatal("want error from failed restart")
	}
	if committed {
		t.Fatal("marker committed although the agent did not restart")
	}
}
