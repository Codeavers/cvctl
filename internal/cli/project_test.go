package cli

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/project"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

type cliStubRepo struct {
	dir         string
	infraSource string
	lsRemoteErr error
}

func (s *cliStubRepo) Clone(_ context.Context, _, _ string) error {
	if s.infraSource != "" && s.dir != "" {
		_ = os.MkdirAll(s.dir, 0o755)
		if data, err := os.ReadFile(filepath.Join(s.infraSource, "compose.yml")); err == nil {
			_ = os.WriteFile(filepath.Join(s.dir, "compose.yml"), data, 0o644)
		}
		if data, err := os.ReadFile(filepath.Join(s.infraSource, "env.spec.yml")); err == nil {
			_ = os.WriteFile(filepath.Join(s.dir, "env.spec.yml"), data, 0o644)
		}
	}
	return nil
}

func (s *cliStubRepo) LsRemote(_ context.Context, _ string) error {
	return s.lsRemoteErr
}

func (s *cliStubRepo) Fetch(_ context.Context, _ string) (string, error) {
	return "commit1234567", nil
}

func (s *cliStubRepo) Show(_ context.Context, _, _ string) ([]byte, error) {
	return nil, os.ErrNotExist
}

func (s *cliStubRepo) Checkout(_ context.Context, _ string) error {
	return nil
}

const cliValidCompose = `
services:
  web:
    image: ghcr.io/codeavers/web:develop
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy:
      resources:
        limits:
          memory: 256M
  db:
    image: postgres:16
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: DB
x-cvctl:
  health:
    service: web
    http: /healthz
`

const cliValidSpec = `
vars:
  DB_URL:
    source: service
    service: db
`

func writeCLIInfra(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(cliValidCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "env.spec.yml"), []byte(cliValidSpec), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectAddCLIJSON(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra-source")
	writeCLIInfra(t, infraDir)

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	stub := &cliStubRepo{infraSource: infraDir}

	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })

	newProjectEnv = func(o projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:          sys.NewOSFS(),
			DeployRoot:  tmp,
			StateRoot:   filepath.Join(tmp, "state"),
			Catalog:     cat,
			Interactive: false,
			RepoFactory: func(dir, _ string) project.RepoLike {
				r := *stub
				r.dir = dir
				return &r
			},
		}, nil
	}

	code, out, errb := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Instance     string `json:"instance"`
			Status       string `json:"status"`
			DeployKeyPub string `json:"deploy_key_pub"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json unmarshal: %v (out: %s)", err, out)
	}
	if !env.OK || env.Data.Instance != "my-app-dev" || env.Data.Status != "OK" {
		t.Fatalf("unexpected data: %+v", env)
	}
	if env.Data.DeployKeyPub == "" {
		t.Error("expected deploy key pub to be present in json output")
	}
}

func TestProjectAddCLIDeployKeyPending(t *testing.T) {
	tmp := t.TempDir()
	stub := &cliStubRepo{lsRemoteErr: errors.New("unauthorized")}

	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })

	newProjectEnv = func(o projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:          sys.NewOSFS(),
			DeployRoot:  tmp,
			StateRoot:   filepath.Join(tmp, "state"),
			Interactive: false,
			RepoFactory: func(dir, _ string) project.RepoLike {
				r := *stub
				r.dir = dir
				return &r
			},
		}, nil
	}

	code, out, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--non-interactive", "--json")
	if code != 1 {
		t.Fatalf("code %d, want 1", code)
	}

	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
			Hint string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json unmarshal: %v (out: %s)", err, out)
	}
	if env.OK || env.Error.Code != "E_DEPLOY_KEY_PENDING" {
		t.Fatalf("unexpected error response: %+v", env)
	}
}

func TestProjectUpdateCLI(t *testing.T) {
	tmp := t.TempDir()
	instDir := filepath.Join(tmp, "my-app-dev")
	infraDir := filepath.Join(instDir, "infra")
	writeCLIInfra(t, infraDir)

	cvctlEnv := `CVCTL_INSTANCE=my-app-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_SVC_DB_MODE=local
CVCTL_SVC_DB_VERSION=16
CVCTL_SVC_DB_USER=db
CVCTL_SVC_DB_PASSWORD=mypassword123456789012345678901
CVCTL_SVC_DB_DB=db
CVCTL_DOMAIN_WEB=web.example.com
`
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(cvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "app.env"), []byte(""), 0o640); err != nil {
		t.Fatal(err)
	}

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	stub := &cliStubRepo{dir: infraDir}

	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })

	newProjectEnv = func(o projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:          sys.NewOSFS(),
			DeployRoot:  tmp,
			StateRoot:   filepath.Join(tmp, "state"),
			Catalog:     cat,
			Interactive: false,
			RepoFactory: func(dir, _ string) project.RepoLike {
				r := *stub
				r.dir = dir
				return &r
			},
		}, nil
	}

	code, out, errb := run(t, "project", "update", "my-app-dev", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Instance string `json:"instance"`
			Status   string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json unmarshal: %v (out: %s)", err, out)
	}
	if !env.OK || env.Data.Instance != "my-app-dev" || env.Data.Status != "OK" {
		t.Fatalf("unexpected data: %+v", env)
	}
}

func TestProjectAddInvalidSet(t *testing.T) {
	code, _, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--set", "NOEQUALS")
	if code != 2 {
		t.Fatalf("code %d, want exit 2 for bad flag", code)
	}
}

func TestProjectAddDomain(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra-source")
	writeCLIInfra(t, infraDir)

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	stub := &cliStubRepo{infraSource: infraDir}

	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })

	newProjectEnv = func(o projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:          sys.NewOSFS(),
			DeployRoot:  tmp,
			StateRoot:   filepath.Join(tmp, "state"),
			Catalog:     cat,
			Interactive: false,
			RepoFactory: func(dir, _ string) project.RepoLike {
				r := *stub
				r.dir = dir
				return &r
			},
		}, nil
	}

	code, out, errb := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--domain", "web=web.example.com", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}

	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Instance string `json:"instance"`
			Status   string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json unmarshal: %v (out: %s)", err, out)
	}
	if !env.OK || env.Data.Instance != "my-app-dev" || env.Data.Status != "OK" {
		t.Fatalf("unexpected data: %+v", env)
	}

	cvctlEnvData, err := os.ReadFile(filepath.Join(tmp, "my-app-dev", "cvctl.env"))
	if err != nil {
		t.Fatalf("read cvctl.env: %v", err)
	}
	if !strings.Contains(string(cvctlEnvData), "CVCTL_DOMAIN_WEB=web.example.com") {
		t.Errorf("expected CVCTL_DOMAIN_WEB in cvctl.env, got:\n%s", string(cvctlEnvData))
	}
}

func TestProjectAddDomainUnknownService(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra-source")
	writeCLIInfra(t, infraDir)

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	stub := &cliStubRepo{infraSource: infraDir}

	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })

	newProjectEnv = func(o projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:          sys.NewOSFS(),
			DeployRoot:  tmp,
			StateRoot:   filepath.Join(tmp, "state"),
			Catalog:     cat,
			Interactive: false,
			RepoFactory: func(dir, _ string) project.RepoLike {
				r := *stub
				r.dir = dir
				return &r
			},
		}, nil
	}

	code, out, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--domain", "unknownsvc=web.example.com", "--json")
	if code != 2 {
		t.Fatalf("code %d, want exit 2 for unknown service", code)
	}
	if !strings.Contains(out, "unknown") && !strings.Contains(out, "usage") {
		t.Errorf("expected unknown service error in output, got: %s", out)
	}
}

func TestProjectAddInvalidDomain(t *testing.T) {
	code, _, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--domain", "NOEQUALS")
	if code != 2 {
		t.Fatalf("code %d, want exit 2 for bad --domain flag", code)
	}
}

func TestProjectUsageErrorsCarryCodes(t *testing.T) {
	code, out, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--set", "NOEQUALS", "--json")
	if code != 2 || !strings.Contains(out, "E_USAGE_SET") {
		t.Fatalf("--set: code %d out=%s", code, out)
	}
	code, out, _ = run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--domain", "NOEQUALS", "--json")
	if code != 2 || !strings.Contains(out, "E_USAGE_DOMAIN") {
		t.Fatalf("--domain: code %d out=%s", code, out)
	}
}

func TestSetEnvValueRequiredCode(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) {
		env.Interactive = false
		return env, nil
	}
	code, out, _ := run(t, "set-env", "myapp-dev", "API_KEY", "--json")
	if code != 2 || !strings.Contains(out, "E_USAGE_SET") {
		t.Fatalf("code %d out=%s", code, out)
	}
}

func TestNotifierForFallsBackToNoop(t *testing.T) {
	if _, ok := notifierFor(nil, nil).(notify.NoopNotifier); !ok {
		t.Fatal("nil config must yield NoopNotifier")
	}
	bad := &hostcfg.Config{Notify: "telegram"}
	if _, ok := notifierFor(bad, hostcfg.NewSecrets()).(notify.NoopNotifier); !ok {
		t.Fatal("misconfigured notifier must fall back to NoopNotifier")
	}
	good := &hostcfg.Config{Notify: "telegram", TelegramChatID: "1"}
	sec := hostcfg.NewSecrets()
	sec.Set("telegram_token", "tok")
	if _, ok := notifierFor(good, sec).(notify.NoopNotifier); ok {
		t.Fatal("configured notifier must not be Noop")
	}
}
