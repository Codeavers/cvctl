package host

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func newPackagesTestEnv() (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	return &Env{Run: r, FS: fs, EUID: 0}, r, fs
}

func markAllInstalled(r *systest.Runner) {
	for _, pkg := range []string{"fail2ban", "ufw", "unattended-upgrades", "curl"} {
		r.On("dpkg-query -W -f='${Status}' "+pkg, sys.Result{
			Stdout:   []byte("install ok installed\n"),
			ExitCode: 0,
		})
	}
}

func TestDockerSatisfiedByExisting(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker compose version --short", sys.Result{Stdout: []byte("2.29.1\n"), ExitCode: 0})
	runner.On("docker info", sys.Result{ExitCode: 0})
	markAllInstalled(runner)

	step := PackagesStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes, got %v", plan.Changes)
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("unexpected Apply result: pending=%q err=%v", pending, err)
	}

	for _, c := range runner.Calls {
		cmdStr := c.String()
		if strings.Contains(cmdStr, "keyrings") || strings.Contains(cmdStr, "docker.asc") ||
			strings.Contains(cmdStr, "docker-ce") || cmdStr == "apt-get update" {
			t.Fatalf("unexpected docker repo command executed: %s", cmdStr)
		}
	}
}

func TestDockerInstallWhenMissing(t *testing.T) {
	env, runner, fs := newPackagesTestEnv()
	markAllInstalled(runner)

	runner.On("install -m 0755 -d /etc/apt/keyrings", sys.Result{ExitCode: 0})
	runner.On("curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc", sys.Result{ExitCode: 0})
	runner.On("dpkg --print-architecture", sys.Result{Stdout: []byte("amd64\n"), ExitCode: 0})
	runner.On("apt-get update", sys.Result{ExitCode: 0})
	runner.On("apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin", sys.Result{ExitCode: 0})

	step := PackagesStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes for missing docker, got none")
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("unexpected pending: %q", pending)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"install -m 0755 -d /etc/apt/keyrings",
		"curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc",
		"dpkg --print-architecture",
		"apt-get update",
		"apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin",
	}

	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("expected %d apply calls, got %d:\nwant: %v\ngot:  %v", len(wantCmds), len(applyCalls), wantCmds, got)
	}

	for i, want := range wantCmds {
		if applyCalls[i].String() != want {
			t.Fatalf("call %d: got %q, want %q", i, applyCalls[i].String(), want)
		}
	}

	// Verify apt-get environment variables
	for _, c := range applyCalls {
		if strings.HasPrefix(c.Name, "apt-get") {
			if !slices.Contains(c.Env, "DEBIAN_FRONTEND=noninteractive") {
				t.Fatalf("expected DEBIAN_FRONTEND=noninteractive in %v", c.Env)
			}
		}
	}

	// Verify /etc/apt/sources.list.d/docker.list content
	data, err := fs.ReadFile("/etc/apt/sources.list.d/docker.list")
	if err != nil {
		t.Fatalf("failed to read /etc/apt/sources.list.d/docker.list: %v", err)
	}
	wantContent := "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n"
	if string(data) != wantContent {
		t.Fatalf("docker.list content mismatch:\ngot:  %q\nwant: %q", string(data), wantContent)
	}
}

func TestDockerOldCompose(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker compose version --short", sys.Result{Stdout: []byte("2.20.0\n"), ExitCode: 0})
	runner.On("docker info", sys.Result{ExitCode: 0})
	markAllInstalled(runner)

	runner.On("install -m 0755 -d /etc/apt/keyrings", sys.Result{ExitCode: 0})
	runner.On("curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc", sys.Result{ExitCode: 0})
	runner.On("dpkg --print-architecture", sys.Result{Stdout: []byte("arm64\n"), ExitCode: 0})
	runner.On("apt-get update", sys.Result{ExitCode: 0})
	runner.On("apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin", sys.Result{ExitCode: 0})

	step := PackagesStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes for old compose version 2.20.0, got none")
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	foundInstall := false
	for _, c := range runner.Calls {
		if c.String() == "apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin" {
			foundInstall = true
			break
		}
	}
	if !foundInstall {
		t.Fatal("expected docker packages install command to be executed")
	}
}

func TestPackagesInstallsOnlyMissing(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker compose version --short", sys.Result{Stdout: []byte("2.29.1\n"), ExitCode: 0})
	runner.On("docker info", sys.Result{ExitCode: 0})

	// fail2ban and ufw missing; unattended-upgrades and curl installed
	runner.On("dpkg-query -W -f='${Status}' fail2ban", sys.Result{ExitCode: 1, Stderr: []byte("dpkg-query: no packages found")})
	runner.On("dpkg-query -W -f='${Status}' ufw", sys.Result{ExitCode: 1, Stderr: []byte("dpkg-query: no packages found")})
	runner.On("dpkg-query -W -f='${Status}' unattended-upgrades", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f='${Status}' curl", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})

	runner.On("apt-get install -y fail2ban ufw", sys.Result{ExitCode: 0})

	step := PackagesStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes for missing packages, got none")
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("unexpected pending: %q", pending)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	if len(applyCalls) != 1 {
		t.Fatalf("expected 1 apply call, got %d: %v", len(applyCalls), applyCalls)
	}

	cmd := applyCalls[0]
	if cmd.String() != "apt-get install -y fail2ban ufw" {
		t.Fatalf("got %q, want 'apt-get install -y fail2ban ufw'", cmd.String())
	}
	if !slices.Contains(cmd.Env, "DEBIAN_FRONTEND=noninteractive") {
		t.Fatalf("expected DEBIAN_FRONTEND=noninteractive in %v", cmd.Env)
	}
}
