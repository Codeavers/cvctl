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
