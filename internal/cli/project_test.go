package cli

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
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
	code, out, _ = run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--ext", "NOEQUALS", "--json")
	if code != 2 || !strings.Contains(out, "E_USAGE_EXT") {
		t.Fatalf("--ext: code %d out=%s", code, out)
	}
}

func TestProjectAddExtValidation(t *testing.T) {
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

	// Unknown service gives exit 2 and E_PROJECT_UNKNOWN_SERVICE
	code, out, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--ext", "unknown.region=auto", "--json")
	if code != 2 || !strings.Contains(out, "E_PROJECT_UNKNOWN_SERVICE") {
		t.Fatalf("expected exit 2 and E_PROJECT_UNKNOWN_SERVICE, got code %d out: %s", code, out)
	}

	// Service in local mode gives exit 2 and E_USAGE_INVALID
	code, out, _ = run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--ext", "db.region=auto", "--json")
	if code != 2 || !strings.Contains(out, "E_USAGE_INVALID") {
		t.Fatalf("expected exit 2 and E_USAGE_INVALID, got code %d out: %s", code, out)
	}
}

func TestParseExtSecretAbsentFromError(t *testing.T) {
	// Secret value passed after "="
	_, err := parseExt([]string{"secrettoken=SUPERSECRETVALUE123"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var ue *output.UsageError
	if !errors.As(err, &ue) {
		t.Fatalf("expected *output.UsageError, got %T", err)
	}
	var oe *output.Error
	if !errors.As(ue.Err, &oe) {
		t.Fatalf("expected *output.Error, got %T", ue.Err)
	}
	if oe.Code != "E_USAGE_EXT" {
		t.Fatalf("expected code E_USAGE_EXT, got %q", oe.Code)
	}
	if strings.Contains(oe.Message, "SUPERSECRETVALUE123") {
		t.Fatalf("error message echoes secret value: %s", oe.Message)
	}
	if strings.Contains(oe.Hint, "SUPERSECRETVALUE123") {
		t.Fatalf("error hint echoes secret value: %s", oe.Hint)
	}
	if !strings.Contains(oe.Message, "secrettoken") {
		t.Fatalf("expected target in error message, got: %s", oe.Message)
	}

	// Secret-like value without "="
	_, err = parseExt([]string{"TOPSECRETTOKENWITHOUTPARAM"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.As(err, &ue) || !errors.As(ue.Err, &oe) {
		t.Fatalf("expected UsageError with output.Error, got %v", err)
	}
	if oe.Code != "E_USAGE_EXT" {
		t.Fatalf("expected code E_USAGE_EXT, got %q", oe.Code)
	}
	if strings.Contains(oe.Message, "TOPSECRETTOKENWITHOUTPARAM") {
		t.Fatalf("error message echoes secret value: %s", oe.Message)
	}
	if strings.Contains(oe.Hint, "TOPSECRETTOKENWITHOUTPARAM") {
		t.Fatalf("error hint echoes secret value: %s", oe.Hint)
	}

	// Also verify via CLI run
	code, out, _ := run(t, "project", "add", "git@github.com:myorg/my-app-infra.git", "--ext", "secrettoken=SUPERSECRETVALUE123", "--json")
	if code != 2 {
		t.Fatalf("expected code 2, got %d", code)
	}
	if strings.Contains(out, "SUPERSECRETVALUE123") {
		t.Fatalf("CLI output echoed secret value: %s", out)
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

func TestProjectRestoreCLIRefusesLocalStorage(t *testing.T) {
	tmp := t.TempDir()
	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })
	newProjectEnv = func(projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:         sys.NewOSFS(),
			DeployRoot: tmp,
			StateRoot:  filepath.Join(tmp, "state"),
			Host:       &hostcfg.Config{Storage: "local"},
		}, nil
	}
	code, out, _ := run(t, "project", "restore", "demo-dev", "--json")
	if code != 1 || !strings.Contains(out, "E_RESTORE_LOCAL_STORAGE") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

type cliFakeData struct{}

func (cliFakeData) RestoreFresh(context.Context, string, string, *contract.Project, *instance.Decisions, string) error {
	return nil
}
func (cliFakeData) RemoveVolume(context.Context, string) error { return nil }
func (cliFakeData) BucketGenerations(context.Context, string, string) (int, int, error) {
	return -1, -1, nil
}
func (cliFakeData) BaseBackup(context.Context, string, string) error {
	return nil
}
func (cliFakeData) BackupService(context.Context, string, string) error { return nil }

type cliNoopCompose struct{}

func (cliNoopCompose) Config(context.Context) error                          { return nil }
func (cliNoopCompose) Pull(context.Context, ...string) error                 { return nil }
func (cliNoopCompose) UpData(context.Context, []string, time.Duration) error { return nil }
func (cliNoopCompose) ExecScript(context.Context, string, string) (sys.Result, error) {
	return sys.Result{}, nil
}
func (cliNoopCompose) RunHook(context.Context, string, string) (sys.Result, error) {
	return sys.Result{}, nil
}
func (cliNoopCompose) Up(context.Context) error                          { return nil }
func (cliNoopCompose) Logs(context.Context, string, int) (string, error) { return "", nil }

func setupRestoreDeployFailure(t *testing.T) {
	t.Helper()
	tmp := t.TempDir()
	instDir := filepath.Join(tmp, "demo-dev")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envBody := "CVCTL_INSTANCE=demo-dev\nCVCTL_ENV=dev\nCVCTL_INFRA_BRANCH=develop\nCVCTL_TRACK_TAG=develop\nCVCTL_INFRA_PATH=.\nCVCTL_STRATEGY=recreate\n" +
		"CVCTL_INFRA_URL=git@github.com:org/demo-infra.git\nCVCTL_SVC_DB_MODE=local\nCVCTL_SVC_DB_VERSION=16\nCVCTL_SVC_DB_USER=db\n" +
		"CVCTL_SVC_DB_PASSWORD=existingpass123456789012345678\nCVCTL_SVC_DB_DB=db\nCVCTL_DOMAIN_WEB=web.example.com\n"
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(envBody), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmp, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	origin := filepath.Join(tmp, "origin")
	writeCLIInfra(t, origin)
	fl := systest.NewFakeLocker()
	oldLock := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLock })
	sys.LockFile = fl.LockFile

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	osfs := sys.NewOSFS()
	stateRoot := filepath.Join(tmp, "state")
	netRun := &systest.Runner{}
	netRun.OnPrefix("docker network", sys.Result{})
	deployer := &deploy.Deployer{
		Run:        netRun,
		DeployRoot: tmp,
		TraefikDir: filepath.Join(tmp, "traefik"),
		StateRoot:  stateRoot,
		FS:         osfs,
		Store:      state.NewStore(osfs, stateRoot),
		Catalog:    cat,
		Compose:    func(_, _, _ string) deploy.ComposeAPI { return cliNoopCompose{} },
		Health:     func(context.Context, string, string) error { return errors.New("unhealthy") },
	}
	oldEnv := newProjectEnv
	t.Cleanup(func() { newProjectEnv = oldEnv })
	newProjectEnv = func(projectOpts) (*project.Env, error) {
		return &project.Env{
			FS:         osfs,
			DeployRoot: tmp,
			StateRoot:  stateRoot,
			Catalog:    cat,
			Host:       &hostcfg.Config{Registries: "ghcr.io/codeavers", Storage: "s3"},
			Deployer:   deployer,
			Data:       cliFakeData{},
			Resolver:   &opsFakeResolver{},
			RepoFactory: func(dir, _ string) project.RepoLike {
				return &cliStubRepo{dir: dir, infraSource: origin}
			},
		}, nil
	}
}

func TestProjectRestoreCLIDeployFailureListsVolumes(t *testing.T) {
	setupRestoreDeployFailure(t)
	code, out, errb := run(t, "project", "restore", "demo-dev")
	if code != 1 || !strings.Contains(out, "restored volume: demo-dev_db-data") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	if strings.Contains(out, "demo-dev: \n") {
		t.Fatalf("empty status printed: %q", out)
	}
	if !strings.Contains(errb+out, "cvctl project update demo-dev") {
		t.Fatalf("hint missing: out=%s err=%s", out, errb)
	}
}

func TestProjectRestoreCLIDeployFailureJSONIncludesVolumes(t *testing.T) {
	setupRestoreDeployFailure(t)
	code, out, _ := run(t, "project", "restore", "demo-dev", "--json")
	var env struct {
		OK    bool `json:"ok"`
		Error struct{ Code, Hint string }
		Data  struct {
			Volumes []string `json:"volumes"`
			Status  string   `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("json: %v (%s)", err, out)
	}
	if code != 1 || env.OK || len(env.Data.Volumes) != 1 || env.Data.Volumes[0] != "demo-dev_db-data" || env.Data.Status == "" {
		t.Fatalf("code=%d env=%+v out=%s", code, env, out)
	}
}
