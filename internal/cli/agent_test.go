package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestAgentCmdFlags(t *testing.T) {
	cmd := newAgentCmd()
	if cmd.Use != "agent" {
		t.Fatalf("expected use agent, got %q", cmd.Use)
	}
	if cmd.Flags().Lookup("once") == nil {
		t.Fatal("expected --once flag")
	}
	if cmd.Flags().Lookup("interval") == nil {
		t.Fatal("expected --interval flag")
	}
}

func TestAgentRegistryLogin(t *testing.T) {
	run := &systest.Runner{}
	run.On("docker login ghcr.io -u deployuser --password-stdin", sys.Result{ExitCode: 0})

	hcfg := &hostcfg.Config{
		Registries:   "ghcr.io/codeavers",
		RegistryUser: "deployuser",
	}
	sec := hostcfg.NewSecrets()
	sec.Set("registry_token", "ghp_secrettoken")

	notif := notify.NoopNotifier{}
	var out bytes.Buffer

	runAgentRegistryLogin(context.Background(), run, hcfg, sec, notif, &out)

	found := false
	for _, c := range run.Calls {
		if c.String() == "docker login ghcr.io -u deployuser --password-stdin" {
			found = true
			if string(c.Stdin) != "ghp_secrettoken" {
				t.Fatalf("got Stdin %q, want ghp_secrettoken", string(c.Stdin))
			}
			break
		}
	}
	if !found {
		t.Fatalf("expected docker login call, got %v", run.Calls)
	}
}

func TestAgentRegistryLoginFailure(t *testing.T) {
	run := &systest.Runner{}
	run.On("docker login ghcr.io -u deployuser --password-stdin", sys.Result{ExitCode: 1, Stderr: []byte("unauthorized")})

	hcfg := &hostcfg.Config{
		Registries:   "ghcr.io/codeavers",
		RegistryUser: "deployuser",
	}
	sec := hostcfg.NewSecrets()
	sec.Set("registry_token", "bad-token")

	notif := notify.NoopNotifier{}
	var out bytes.Buffer

	// Should log error and not panic / not block
	runAgentRegistryLogin(context.Background(), run, hcfg, sec, notif, &out)

	logStr := out.String()
	if !strings.Contains(logStr, `"action":"error"`) {
		t.Fatalf("expected error action in log, got: %s", logStr)
	}
}

func TestAgentCmdJSON(t *testing.T) {
	root := NewRoot("test")
	var stdout, stderr bytes.Buffer

	code := Execute(root, []string{"agent", "--help"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout.String(), "cvctl agent") {
		t.Fatalf("expected help text to mention cvctl agent, got: %s", stdout.String())
	}
}

func TestAgentReportJSONStructure(t *testing.T) {
	rep := agentReport{
		Decisions: nil,
	}
	data, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"decisions":`) {
		t.Fatalf("unexpected JSON: %s", string(data))
	}
}
