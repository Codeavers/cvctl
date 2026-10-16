package compose

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestCmdShape(t *testing.T) {
	tests := []struct {
		name     string
		project  Project
		subcmd   []string
		wantName string
		wantArgs []string
	}{
		{
			name: "defaults with empty infra path",
			project: Project{
				Instance: "proj-dev",
				Dir:      "/srv/deploy/proj-dev",
			},
			subcmd:   []string{"up", "-d"},
			wantName: "docker",
			wantArgs: []string{
				"compose", "-p", "proj-dev",
				"--env-file", "/srv/deploy/proj-dev/app.env",
				"-f", "/srv/deploy/proj-dev/infra/compose.yml",
				"-f", "/srv/deploy/proj-dev/compose.cvctl.yml",
				"up", "-d",
			},
		},
		{
			name: "dot infra path",
			project: Project{
				Instance:  "proj-dev",
				Dir:       "/srv/deploy/proj-dev",
				InfraPath: ".",
			},
			subcmd:   []string{"ps"},
			wantName: "docker",
			wantArgs: []string{
				"compose", "-p", "proj-dev",
				"--env-file", "/srv/deploy/proj-dev/app.env",
				"-f", "/srv/deploy/proj-dev/infra/compose.yml",
				"-f", "/srv/deploy/proj-dev/compose.cvctl.yml",
				"ps",
			},
		},
		{
			name: "nested infra path and custom override and env file",
			project: Project{
				Instance:  "proj-prod",
				Dir:       "/srv/deploy/proj-prod",
				InfraPath: "services/web",
				Override:  "/srv/deploy/proj-prod/staging.override.yml",
				EnvFile:   "/srv/deploy/proj-prod/staging.app.env",
			},
			subcmd:   []string{"config", "-q"},
			wantName: "docker",
			wantArgs: []string{
				"compose", "-p", "proj-prod",
				"--env-file", "/srv/deploy/proj-prod/staging.app.env",
				"-f", "/srv/deploy/proj-prod/infra/services/web/compose.yml",
				"-f", "/srv/deploy/proj-prod/staging.override.yml",
				"config", "-q",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd := tc.project.Cmd(tc.subcmd...)
			if cmd.Name != tc.wantName {
				t.Fatalf("cmd.Name = %q, want %q", cmd.Name, tc.wantName)
			}
			if !reflect.DeepEqual(cmd.Args, tc.wantArgs) {
				t.Fatalf("cmd.Args mismatch:\ngot:  %#v\nwant: %#v", cmd.Args, tc.wantArgs)
			}
		})
	}
}

func TestRunHookArgs(t *testing.T) {
	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	expectedCmd := p.Cmd("run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "api", "-c", "npm run migrate")
	runner.On(expectedCmd.String(), sys.Result{Stdout: []byte("migrated 5 tables\n"), ExitCode: 0})

	res, err := p.RunHook(context.Background(), "api", "npm run migrate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(res.Stdout) != "migrated 5 tables\n" {
		t.Fatalf("unexpected stdout: %q", string(res.Stdout))
	}
	if len(runner.Calls) != 1 || runner.Calls[0].String() != expectedCmd.String() {
		t.Fatalf("expected call %q, got: %+v", expectedCmd.String(), runner.Calls)
	}
}

func TestRunHookFailure(t *testing.T) {
	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	expectedCmd := p.Cmd("run", "--rm", "--no-deps", "-T", "--entrypoint", "sh", "api", "-c", "false")
	runner.On(expectedCmd.String(), sys.Result{Stderr: []byte("migration syntax error\n"), ExitCode: 1})

	res, err := p.RunHook(context.Background(), "api", "false")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_HOOK_FAILED" {
		t.Fatalf("expected code E_HOOK_FAILED, got %q", outErr.Code)
	}
	if string(res.Stderr) != "migration syntax error\n" {
		t.Fatalf("expected stderr in returned result: %q", string(res.Stderr))
	}
	if res.ExitCode != 1 {
		t.Fatalf("expected exit code 1, got %d", res.ExitCode)
	}
}

func TestPSParsesArrayAndNDJSON(t *testing.T) {
	t.Run("array format", func(t *testing.T) {
		runner := &systest.Runner{}
		p := Project{
			Run:      runner,
			Instance: "demo-dev",
			Dir:      "/srv/deploy/demo-dev",
		}
		psCmd := p.Cmd("ps", "--format", "json")
		runner.On(psCmd.String(), sys.Result{
			Stdout:   []byte(`[{"Service":"api","Name":"demo-dev-api-1","State":"running","Health":"healthy","Image":"ghcr.io/ex/api:v1"},{"Service":"worker","Name":"demo-dev-worker-1","State":"running","Health":"","Image":"ghcr.io/ex/worker:v1"}]`),
			ExitCode: 0,
		})

		containers, err := p.PS(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(containers) != 2 {
			t.Fatalf("expected 2 containers, got %d", len(containers))
		}
		if containers[0].Service != "api" || containers[0].Name != "demo-dev-api-1" || containers[0].State != "running" || containers[0].Health != "healthy" || containers[0].Image != "ghcr.io/ex/api:v1" {
			t.Fatalf("unexpected container[0]: %+v", containers[0])
		}
		if containers[1].Service != "worker" || containers[1].Name != "demo-dev-worker-1" || containers[1].State != "running" {
			t.Fatalf("unexpected container[1]: %+v", containers[1])
		}
	})

	t.Run("ndjson format", func(t *testing.T) {
		runner := &systest.Runner{}
		p := Project{
			Run:      runner,
			Instance: "demo-dev",
			Dir:      "/srv/deploy/demo-dev",
		}
		psCmd := p.Cmd("ps", "--format", "json")
		runner.On(psCmd.String(), sys.Result{
			Stdout:   []byte("{\"Service\":\"api\",\"Name\":\"demo-dev-api-1\",\"State\":\"running\",\"Health\":\"healthy\",\"Image\":\"ghcr.io/ex/api:v1\"}\n{\"Service\":\"worker\",\"Name\":\"demo-dev-worker-1\",\"State\":\"running\",\"Health\":\"\",\"Image\":\"ghcr.io/ex/worker:v1\"}\n"),
			ExitCode: 0,
		})

		containers, err := p.PS(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(containers) != 2 {
			t.Fatalf("expected 2 containers, got %d", len(containers))
		}
		if containers[0].Service != "api" || containers[0].Name != "demo-dev-api-1" {
			t.Fatalf("unexpected container[0]: %+v", containers[0])
		}
		if containers[1].Service != "worker" || containers[1].Name != "demo-dev-worker-1" {
			t.Fatalf("unexpected container[1]: %+v", containers[1])
		}
	})
}

func TestComposeHelpers(t *testing.T) {
	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	configCmd := p.Cmd("config", "-q")
	runner.On(configCmd.String(), sys.Result{ExitCode: 0})
	if err := p.Config(context.Background()); err != nil {
		t.Fatalf("Config failed: %v", err)
	}

	pullCmd := p.Cmd("pull", "api", "worker")
	runner.On(pullCmd.String(), sys.Result{ExitCode: 0})
	if err := p.Pull(context.Background(), "api", "worker"); err != nil {
		t.Fatalf("Pull failed: %v", err)
	}

	upDataCmd := p.Cmd("up", "-d", "--wait", "main-db", "cache")
	runner.On(upDataCmd.String(), sys.Result{ExitCode: 0})
	if err := p.UpData(context.Background(), []string{"main-db", "cache"}); err != nil {
		t.Fatalf("UpData failed: %v", err)
	}

	// Empty UpData is a no-op
	if err := p.UpData(context.Background(), nil); err != nil {
		t.Fatalf("empty UpData failed: %v", err)
	}

	upCmd := p.Cmd("up", "-d", "--remove-orphans")
	runner.On(upCmd.String(), sys.Result{ExitCode: 0})
	if err := p.Up(context.Background()); err != nil {
		t.Fatalf("Up failed: %v", err)
	}

	logsCmd := p.Cmd("logs", "--tail", "50", "api")
	runner.On(logsCmd.String(), sys.Result{Stdout: []byte("line1\nline2\n"), ExitCode: 0})
	logs, err := p.Logs(context.Background(), "api", 50)
	if err != nil {
		t.Fatalf("Logs failed: %v", err)
	}
	if logs != "line1\nline2\n" {
		t.Fatalf("unexpected logs: %q", logs)
	}
}

func TestComposeFailureErrors(t *testing.T) {
	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	configCmd := p.Cmd("config", "-q")
	runner.On(configCmd.String(), sys.Result{Stderr: []byte("invalid yaml"), ExitCode: 1})
	err := p.Config(context.Background())
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok || outErr.Code != "E_COMPOSE_FAILED" {
		t.Fatalf("expected E_COMPOSE_FAILED, got: %v", err)
	}
	wantHint := "see the command output; check the compose files in " + filepath.Clean(p.Dir)
	if outErr.Hint != wantHint {
		t.Fatalf("hint = %q, want %q", outErr.Hint, wantHint)
	}
}
