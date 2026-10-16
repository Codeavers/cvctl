package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
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
		r.On("dpkg-query -W -f=${Status} "+pkg, sys.Result{
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

	step := PackagesStep{}
	_, err := step.Check(context.Background(), env)
	if err == nil {
		t.Fatal("expected error for old compose version 2.20.0, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_DOCKER_COMPOSE_OLD" {
		t.Errorf("got code %q, want E_DOCKER_COMPOSE_OLD", oe.Code)
	}
	if oe.Hint != "install the compose plugin: apt-get install docker-compose-v2 (or docker-compose-plugin from Docker's repo)" {
		t.Errorf("got hint %q", oe.Hint)
	}
}

func TestDockerComposeMissingWhenDockerRunning(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker info", sys.Result{ExitCode: 0})
	// compose missing (runner returns 127)
	markAllInstalled(runner)

	step := PackagesStep{}
	_, err := step.Check(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when compose is missing but docker info succeeds, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_DOCKER_COMPOSE_OLD" {
		t.Errorf("got code %q, want E_DOCKER_COMPOSE_OLD", oe.Code)
	}
	if oe.Hint != "install the compose plugin: apt-get install docker-compose-v2 (or docker-compose-plugin from Docker's repo)" {
		t.Errorf("got hint %q", oe.Hint)
	}

	for _, c := range runner.Calls {
		if strings.Contains(c.String(), "docker-ce") {
			t.Fatalf("unexpected docker-ce install call: %s", c.String())
		}
	}
}

func TestPackagesInstallsOnlyMissing(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker compose version --short", sys.Result{Stdout: []byte("2.29.1\n"), ExitCode: 0})
	runner.On("docker info", sys.Result{ExitCode: 0})

	// fail2ban and ufw missing; unattended-upgrades and curl installed
	runner.On("dpkg-query -W -f=${Status} fail2ban", sys.Result{ExitCode: 1, Stderr: []byte("dpkg-query: no packages found")})
	runner.On("dpkg-query -W -f=${Status} ufw", sys.Result{ExitCode: 1, Stderr: []byte("dpkg-query: no packages found")})
	runner.On("dpkg-query -W -f=${Status} unattended-upgrades", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} curl", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})

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

func TestDockerDaemonDownWhenInstalled(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker compose version --short", sys.Result{Stdout: []byte("2.29.1\n"), ExitCode: 0})
	runner.On("docker info", sys.Result{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock\n")})
	markAllInstalled(runner)

	step := PackagesStep{}
	_, err := step.Check(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when compose is installed but daemon is down, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_DOCKER_DAEMON_DOWN" {
		t.Errorf("got code %q, want E_DOCKER_DAEMON_DOWN", oe.Code)
	}
	if oe.Message != "docker is installed but the daemon is not responding" {
		t.Errorf("got message %q, want 'docker is installed but the daemon is not responding'", oe.Message)
	}
	if oe.Hint != "systemctl start docker" {
		t.Errorf("got hint %q, want 'systemctl start docker'", oe.Hint)
	}
}

func TestPackagesInstallBaseBeforeDockerRepo(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	// Docker missing
	runner.On("docker compose version --short", sys.Result{ExitCode: 127})
	// curl missing, other base packages installed
	runner.On("dpkg-query -W -f=${Status} fail2ban", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} ufw", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} unattended-upgrades", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} curl", sys.Result{ExitCode: 1, Stderr: []byte("not installed")})

	runner.On("apt-get install -y curl", sys.Result{ExitCode: 0})
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

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("Apply failed: pending=%q err=%v", pending, err)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	var curlInstallIdx, curlDownloadIdx = -1, -1
	for i, c := range applyCalls {
		if c.String() == "apt-get install -y curl" {
			curlInstallIdx = i
		}
		if strings.HasPrefix(c.String(), "curl ") {
			curlDownloadIdx = i
		}
	}

	if curlInstallIdx == -1 {
		t.Fatal("apt-get install -y curl was not called")
	}
	if curlDownloadIdx == -1 {
		t.Fatal("curl download was not called")
	}
	if curlInstallIdx >= curlDownloadIdx {
		t.Fatalf("expected curl package install (index %d) before curl download (index %d)", curlInstallIdx, curlDownloadIdx)
	}
}

func TestPackagesAptFailureReturnsCodedError(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	runner.On("docker compose version --short", sys.Result{Stdout: []byte("2.29.1\n"), ExitCode: 0})
	runner.On("docker info", sys.Result{ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} fail2ban", sys.Result{ExitCode: 1})
	runner.On("dpkg-query -W -f=${Status} ufw", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} unattended-upgrades", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})
	runner.On("dpkg-query -W -f=${Status} curl", sys.Result{Stdout: []byte("install ok installed\n"), ExitCode: 0})

	runner.On("apt-get install -y fail2ban", sys.Result{ExitCode: 100, Stderr: []byte("E: Unable to locate package fail2ban\n")})

	step := PackagesStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected error on apt-get failure, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_APT_FAILED" {
		t.Errorf("got code %q, want E_APT_FAILED", oe.Code)
	}
	if oe.Hint != "check apt sources and network, then re-run cvctl init" {
		t.Errorf("got hint %q, want apt hint", oe.Hint)
	}
	if !strings.Contains(oe.Message, "Unable to locate package fail2ban") {
		t.Errorf("expected stderr in message, got %q", oe.Message)
	}
}

func TestPackagesCurlFailureReturnsCodedError(t *testing.T) {
	env, runner, _ := newPackagesTestEnv()
	markAllInstalled(runner)
	runner.On("install -m 0755 -d /etc/apt/keyrings", sys.Result{ExitCode: 0})
	runner.On("curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc", sys.Result{
		ExitCode: 6,
		Stderr:   []byte("curl: (6) Could not resolve host: download.docker.com\n"),
	})

	step := PackagesStep{}
	plan, _ := step.Check(context.Background(), env)
	_, err := step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected error on curl failure, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_DOCKER_REPO" {
		t.Errorf("got code %q, want E_DOCKER_REPO", oe.Code)
	}
	if oe.Hint != "check network access to download.docker.com" {
		t.Errorf("got hint %q, want docker repo hint", oe.Hint)
	}
	if !strings.Contains(oe.Message, "Could not resolve host") {
		t.Errorf("expected stderr in message, got %q", oe.Message)
	}
}

func TestComposeVersionParsing(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"v2.29.1", true},
		{"2.29.1-desktop.1", true},
		{"2.24.0", true},
		{"2.24.1", true},
		{"3.0.0", true},
		{"2.20.0", false},
		{"2.23.9", false},
		{"1.29.2", false},
		{"garbage", false},
		{"", false},
	}

	for _, tc := range cases {
		got := isComposeSatisfied(tc.input)
		if got != tc.want {
			t.Errorf("isComposeSatisfied(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}
