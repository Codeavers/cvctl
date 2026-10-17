package project

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

type stubRepo struct {
	dir         string
	infraSource string
	fs          sys.FS
	cloneErr    error
	lsRemoteErr error
	fetchCommit string
	fetchErr    error
	showData    map[string][]byte
	showErr     error
	checkoutErr error
	onFetch     func()
	log         *[]string
}

func (s *stubRepo) record(entry string) {
	if s.log != nil {
		*s.log = append(*s.log, entry)
	}
}

func (s *stubRepo) Clone(_ context.Context, _, _ string) error {
	if s.cloneErr != nil {
		return s.cloneErr
	}
	if s.infraSource != "" && s.dir != "" {
		_ = os.MkdirAll(s.dir, 0o755)
		if data, err := os.ReadFile(filepath.Join(s.infraSource, "compose.yml")); err == nil {
			_ = os.WriteFile(filepath.Join(s.dir, "compose.yml"), data, 0o644)
			if s.fs != nil {
				_ = s.fs.WriteFile(filepath.Join(s.dir, "compose.yml"), data, 0o644, sys.Owner{})
			}
		}
		if data, err := os.ReadFile(filepath.Join(s.infraSource, "env.spec.yml")); err == nil {
			_ = os.WriteFile(filepath.Join(s.dir, "env.spec.yml"), data, 0o644)
			if s.fs != nil {
				_ = s.fs.WriteFile(filepath.Join(s.dir, "env.spec.yml"), data, 0o644, sys.Owner{})
			}
		}
	}
	return nil
}

func (s *stubRepo) LsRemote(_ context.Context, _ string) error {
	return s.lsRemoteErr
}

func (s *stubRepo) Fetch(_ context.Context, branch string) (string, error) {
	s.record("fetch " + branch)
	if s.onFetch != nil {
		s.onFetch()
	}
	if s.fetchErr != nil {
		return "", s.fetchErr
	}
	if s.fetchCommit != "" {
		return s.fetchCommit, nil
	}
	return "abc1234567890", nil
}

func (s *stubRepo) Show(_ context.Context, _, path string) ([]byte, error) {
	if s.showErr != nil {
		return nil, s.showErr
	}
	if d, ok := s.showData[path]; ok {
		return d, nil
	}
	if s.infraSource != "" {
		return os.ReadFile(filepath.Join(s.infraSource, path))
	}
	return nil, os.ErrNotExist
}

// Checkout mimics a working tree update by copying the simulated origin into dir.
func (s *stubRepo) Checkout(_ context.Context, commit string) error {
	s.record("checkout " + commit)
	if s.checkoutErr != nil {
		return s.checkoutErr
	}
	if s.infraSource != "" && s.dir != "" {
		for _, name := range []string{"compose.yml", "env.spec.yml"} {
			if data, err := os.ReadFile(filepath.Join(s.infraSource, name)); err == nil {
				_ = os.WriteFile(filepath.Join(s.dir, name), data, 0o644)
			}
		}
	}
	return nil
}

func testCatalog(t *testing.T) kinds.Catalog {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatalf("kinds.Load: %v", err)
	}
	return cat
}

func writeTestInfra(t *testing.T, dir, composeContent, specContent string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(composeContent), 0o644); err != nil {
		t.Fatalf("write compose: %v", err)
	}
	if specContent != "" {
		if err := os.WriteFile(filepath.Join(dir, "env.spec.yml"), []byte(specContent), 0o644); err != nil {
			t.Fatalf("write spec: %v", err)
		}
	}
}

const validCompose = `
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

const validSpec = `
vars:
  DB_URL:
    source: service
    service: db
`

func TestAddDerivesInstanceName(t *testing.T) {
	tests := []struct {
		url      string
		project  string
		env      string
		expected string
	}{
		{
			url:      "git@github.com:myorg/my-service-infra.git",
			expected: "my-service-dev",
		},
		{
			url:      "https://github.com/myorg/web-app.git",
			env:      "prod",
			project:  "custom",
			expected: "custom-prod",
		},
		{
			url:      "git@github.com:myorg/api-infra.git",
			env:      "prod",
			expected: "api-prod",
		},
		{
			url:      "file:///tmp/repo-infra/",
			env:      "dev",
			expected: "repo-dev",
		},
		{
			url:      "git@github.com:myorg/UPPER-infra.git",
			env:      "dev",
			expected: "upper-dev",
		},
		{
			url:      "git@github.com:myorg/repo.git",
			project:  "Foo.Bar",
			env:      "dev",
			expected: "foo-bar-dev",
		},
		{
			url:      "git@github.com:myorg/repo.git",
			project:  "a_b",
			env:      "dev",
			expected: "a-b-dev",
		},
		{
			url:      "git@github.com:myorg/repo.git",
			project:  "a/../../x",
			env:      "dev",
			expected: "a-x-dev",
		},
	}

	for _, tt := range tests {
		got := DeriveInstanceName(tt.url, tt.project, tt.env)
		if got != tt.expected {
			t.Errorf("DeriveInstanceName(%q, %q, %q) = %q, want %q", tt.url, tt.project, tt.env, got, tt.expected)
		}
	}
}

func TestAddExisting(t *testing.T) {
	fsys := systest.NewFS()
	deployRoot := "/srv/deploy"
	inst := "my-service-dev"
	instDir := filepath.Join(deployRoot, inst)
	_ = fsys.MkdirAll(instDir, 0o2775, sys.Owner{User: "deployer", Group: "deploy"})

	e := &Env{
		FS:         fsys,
		DeployRoot: deployRoot,
		StateRoot:  "/var/lib/cvctl",
	}

	_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_PROJECT_EXISTS" {
		t.Errorf("expected code E_PROJECT_EXISTS, got %s", oe.Code)
	}
	wantHint := "cvctl project update " + inst
	if !strings.Contains(oe.Hint, wantHint) {
		t.Errorf("expected hint to contain %q, got %q", wantHint, oe.Hint)
	}
}

func TestAddDeployKeyPendingNonInteractive(t *testing.T) {
	fsys := systest.NewFS()
	stub := &stubRepo{lsRemoteErr: errors.New("permission denied")}

	e := &Env{
		FS:          fsys,
		DeployRoot:  "/srv/deploy",
		StateRoot:   "/var/lib/cvctl",
		Interactive: false,
		RepoFactory: func(_, _ string) RepoLike {
			return stub
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_DEPLOY_KEY_PENDING" {
		t.Errorf("expected code E_DEPLOY_KEY_PENDING, got %s", oe.Code)
	}
	if !strings.Contains(oe.Hint, "ssh-ed25519") {
		t.Errorf("expected hint to contain public key, got %q", oe.Hint)
	}

	keyPath := "/var/lib/cvctl/my-service-dev/deploy_key"
	key1, err := fsys.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading deploy key: %v", err)
	}

	// Re-running project add reuses the key that already exists.
	_, err2 := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{})
	if err2 == nil {
		t.Fatal("expected error on re-run, got nil")
	}
	key2, err := fsys.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("reading deploy key on re-run: %v", err)
	}
	if string(key1) != string(key2) {
		t.Error("expected deploy key to be reused, but it changed")
	}
}

func TestAddGeneratesLocalCreds(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, validCompose, validSpec)

	fsys := sys.NewOSFS()
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: true,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	cvctlEnvPath := filepath.Join(tmp, "my-service-dev", "cvctl.env")
	data, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatalf("read cvctl.env: %v", err)
	}
	content := string(data)

	// User and db must be lowercase normalized service name.
	if !strings.Contains(content, `CVCTL_SVC_DB_USER="db"`) && !strings.Contains(content, `CVCTL_SVC_DB_USER=db`) {
		t.Errorf("expected user db in cvctl.env, got:\n%s", content)
	}
	if !strings.Contains(content, `CVCTL_SVC_DB_DB="db"`) && !strings.Contains(content, `CVCTL_SVC_DB_DB=db`) {
		t.Errorf("expected db db in cvctl.env, got:\n%s", content)
	}

	// Password must be 32 alphanumerics.
	pw := ""
	if res.Decisions != nil {
		pw = res.Decisions.Services["db"].Creds.Password
	}
	if pw == "" {
		re := regexp.MustCompile(`CVCTL_SVC_DB_PASSWORD="?([A-Za-z0-9]+)"?`)
		m := re.FindStringSubmatch(content)
		if len(m) > 1 {
			pw = m[1]
		}
	}
	if len(pw) != 32 {
		t.Errorf("expected password length 32, got %d (%q)", len(pw), pw)
	}
	alphanumericRe := regexp.MustCompile(`^[a-zA-Z0-9]{32}$`)
	if !alphanumericRe.MatchString(pw) {
		t.Errorf("expected password to match ^[a-zA-Z0-9]{32}$, got %q", pw)
	}
}

func TestAddAsksExternalOnlyWhenAllowed(t *testing.T) {
	compose := `
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
  main-db:
    image: postgres:16
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: MAIN_DB
      cvctl.external: allowed
  cache:
    image: redis:7
    labels:
      cvctl.kind: redis
      cvctl.version: "7"
      cvctl.env: REDIS
x-cvctl:
  health:
    service: web
    http: /healthz
`
	spec := `
vars:
  MAIN_DB_URL:
    source: service
    service: main-db
  REDIS_URL:
    source: service
    service: cache
`
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, compose, spec)

	fsys := sys.NewOSFS()
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	// main-db has cvctl.external: allowed, so data_main-db_mode is asked.
	// cache does not have cvctl.external: allowed, so data_cache_mode is NOT asked.
	answers := map[string]string{
		"data_main-db_mode":    "external",
		"data_main-db_ext_url": "postgres://user:pass@postgres.remote.com:5432/remotedb",
		"domain_web":           "web.example.com",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompter,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	if res.Decisions.Services["main-db"].Mode != "external" {
		t.Errorf("expected main-db to be external, got %s", res.Decisions.Services["main-db"].Mode)
	}
	if res.Decisions.Services["cache"].Mode != "local" {
		t.Errorf("expected cache to be local, got %s", res.Decisions.Services["cache"].Mode)
	}
}

func TestAddLintFailureCleansInstanceKeepsKey(t *testing.T) {
	// Invalid compose: app service has no memory limit
	invalidCompose := `
services:
  web:
    image: ghcr.io/codeavers/web:develop
    labels:
      cvctl.role: app
x-cvctl:
  health:
    service: web
    http: /healthz
`
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, invalidCompose, "")

	fsys := sys.NewOSFS()
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{})
	if err == nil {
		t.Fatal("expected lint failure error, got nil")
	}

	// Instance dir should be cleaned up.
	instDir := filepath.Join(tmp, "my-service-dev")
	if _, statErr := os.Stat(instDir); statErr == nil {
		t.Errorf("expected instance directory %s to be removed, but it exists", instDir)
	}

	// Key should still exist.
	keyPath := filepath.Join(tmp, "var-lib-cvctl", "my-service-dev", "deploy_key")
	if _, statErr := os.Stat(keyPath); statErr != nil {
		t.Errorf("expected deploy key %s to be kept, but got error: %v", keyPath, statErr)
	}
}

func TestUpdateAsksOnlyNew(t *testing.T) {
	tmp := t.TempDir()
	inst := "my-service-dev"
	instDir := filepath.Join(tmp, inst)
	infraDir := filepath.Join(instDir, "infra")

	cat := testCatalog(t)
	stub := &stubRepo{}

	// Pre-existing decisions in cvctl.env:
	// db service is already decided and local.
	// web domain is already decided.
	existingDec := &contract.Project{}
	_ = existingDec
	existingCvctlEnv := `CVCTL_INSTANCE=my-service-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_SVC_DB_MODE=local
CVCTL_SVC_DB_VERSION=16
CVCTL_SVC_DB_USER=db
CVCTL_SVC_DB_PASSWORD=existingpass123456789012345678
CVCTL_SVC_DB_DB=db
CVCTL_DOMAIN_WEB=web.example.com
`
	existingAppEnv := `EXISTING_VAR=already_set_value
`
	// New compose adds new-db (external allowed) and api (exposed), keeps web and db.
	newCompose := `
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
  api:
    image: ghcr.io/codeavers/api:develop
    labels:
      cvctl.role: app
      cvctl.expose: "8000"
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
  new-db:
    image: postgres:16
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: NEW_DB
      cvctl.external: allowed
x-cvctl:
  health:
    service: web
    http: /healthz
`
	newSpec := `
vars:
  DB_URL:
    source: service
    service: db
  NEW_DB_URL:
    source: service
    service: new-db
  EXISTING_VAR:
    source: prompt
  NEW_VAR:
    source: prompt
`
	writeTestInfra(t, infraDir, newCompose, newSpec)
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatalf("write cvctl.env: %v", err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "app.env"), []byte(existingAppEnv), 0o640); err != nil {
		t.Fatalf("write app.env: %v", err)
	}

	// Prompter only expects questions for what is NEW:
	// data_new-db_mode, domain_api, env_NEW_VAR.
	// If it asks data_db_mode, domain_web, or env_EXISTING_VAR, the test fails.
	answers := map[string]string{
		"data_new-db_mode": "local",
		"domain_api":       "api.example.com",
		"env_NEW_VAR":      "new_val",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	fsys := sys.NewOSFS()
	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompter,
		RepoFactory: func(_, _ string) RepoLike {
			return stub
		},
	}

	res, err := Update(context.Background(), e, inst)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	if res.Decisions.Services["db"].Creds.Password != "existingpass123456789012345678" {
		t.Errorf("expected existing db credentials to be preserved, got %s", res.Decisions.Services["db"].Creds.Password)
	}
	if res.Decisions.Domains["web"] != "web.example.com" {
		t.Errorf("expected existing web domain to be preserved, got %s", res.Decisions.Domains["web"])
	}
	if res.Decisions.Domains["api"] != "api.example.com" {
		t.Errorf("expected api domain to be set, got %s", res.Decisions.Domains["api"])
	}

	appEnvData, err := os.ReadFile(filepath.Join(instDir, "app.env"))
	if err != nil {
		t.Fatalf("read app.env: %v", err)
	}
	appEnvContent := string(appEnvData)
	if !strings.Contains(appEnvContent, "EXISTING_VAR=already_set_value") {
		t.Errorf("expected existing var preserved in app.env, got:\n%s", appEnvContent)
	}
	if !strings.Contains(appEnvContent, "NEW_VAR=new_val") {
		t.Errorf("expected new var added to app.env, got:\n%s", appEnvContent)
	}
}

func TestAddBlueGreenUnsupported(t *testing.T) {
	e := &Env{}
	_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Strategy: "blue-green",
	})
	if err == nil {
		t.Fatal("expected error for blue-green strategy, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_STRATEGY_UNSUPPORTED" {
		t.Errorf("expected code E_STRATEGY_UNSUPPORTED, got %s", oe.Code)
	}
}

func TestAddFileRepoAllowed(t *testing.T) {
	// Without CVCTL_ALLOW_FILE_REPOS=1, file:// fails
	t.Setenv("CVCTL_ALLOW_FILE_REPOS", "")
	e := &Env{}
	_, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{})
	if err == nil {
		t.Fatal("expected error for file repo without env var, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_REPO_URL" {
		t.Errorf("expected E_PROJECT_REPO_URL, got %v", err)
	}

	// With CVCTL_ALLOW_FILE_REPOS=1, file:// succeeds and generates no deploy key
	t.Setenv("CVCTL_ALLOW_FILE_REPOS", "1")
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, validCompose, validSpec)

	fsys := sys.NewOSFS()
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e = &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: true,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "file:///tmp/repo-infra", AddOptions{})
	if err != nil {
		t.Fatalf("Add with file repo failed: %v", err)
	}
	if res.DeployKeyPub != "" {
		t.Errorf("expected empty deploy key pub for file repo, got %q", res.DeployKeyPub)
	}
	keyPath := filepath.Join(tmp, "var-lib-cvctl", "repo-dev", "deploy_key")
	if _, statErr := os.Stat(keyPath); statErr == nil {
		t.Errorf("expected no deploy key to be generated, but %s exists", keyPath)
	}
}

func TestAddPreSeededSet(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	compose := `
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
	spec := `
vars:
  DB_URL:
    source: service
    service: db
  MY_PRESEEDED_VAR:
    source: prompt
`
	writeTestInfra(t, infraDir, compose, spec)

	fsys := sys.NewOSFS()
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	// Because MY_PRESEEDED_VAR is in Set, Prompter will NOT be asked for env_MY_PRESEEDED_VAR!
	answers := map[string]string{
		"domain_web": "web.example.com",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompter,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Set: map[string]string{
			"MY_PRESEEDED_VAR": "secret_preseeded_val",
		},
	})
	if err != nil {
		t.Fatalf("Add with Set failed: %v", err)
	}

	appEnvPath := filepath.Join(tmp, "my-service-dev", "app.env")
	appEnvData, err := os.ReadFile(appEnvPath)
	if err != nil {
		t.Fatalf("read app.env: %v", err)
	}
	if !strings.Contains(string(appEnvData), `MY_PRESEEDED_VAR=secret_preseeded_val`) &&
		!strings.Contains(string(appEnvData), `MY_PRESEEDED_VAR="secret_preseeded_val"`) {
		t.Errorf("expected pre-seeded var in app.env, got:\n%s", string(appEnvData))
	}
	_ = res
}

func TestAddWithDomain(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra-source")
	writeTestInfra(t, infraDir, validCompose, validSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Catalog:     cat,
		Interactive: false,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Domains: map[string]string{
			"web": "web.example.com",
		},
	})
	if err != nil {
		t.Fatalf("Add with Domains failed: %v", err)
	}

	if res.Decisions.Domains["web"] != "web.example.com" {
		t.Errorf("expected domain web.example.com, got %s", res.Decisions.Domains["web"])
	}
}

func TestAddWithDomainUnknownService(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra-source")
	writeTestInfra(t, infraDir, validCompose, validSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Catalog:     cat,
		Interactive: false,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Domains: map[string]string{
			"nonexistent": "bad.example.com",
		},
	})
	if err == nil {
		t.Fatal("expected error for unknown service domain, got nil")
	}

	var usage *output.UsageError
	if !errors.As(err, &usage) {
		t.Fatalf("expected usage error, got %T: %v", err, err)
	}
}

func TestAddPathTraversalAndValidation(t *testing.T) {
	fsys := systest.NewFS()
	runner := &systest.Runner{}
	e := &Env{
		FS:         fsys,
		Run:        runner,
		DeployRoot: "/srv/deploy",
		StateRoot:  "/var/lib/cvctl",
	}

	// 1. --name ".."
	_, err := Add(context.Background(), e, "git@github.com:myorg/repo.git", AddOptions{Project: "..", Env: "dev"})
	if err == nil {
		t.Fatal("expected error for --name '..', got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_NAME_INVALID" {
		t.Fatalf("expected E_PROJECT_NAME_INVALID, got %v", err)
	}
	if !strings.Contains(oe.Hint, "use --name with letters, digits and '-'") {
		t.Errorf("expected hint to contain 'use --name with letters, digits and -', got %q", oe.Hint)
	}

	// 2. 70-char name
	longName := strings.Repeat("a", 70)
	_, err = Add(context.Background(), e, "git@github.com:myorg/repo.git", AddOptions{Project: longName, Env: "dev"})
	if err == nil {
		t.Fatal("expected error for 70-char name, got nil")
	}
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_NAME_INVALID" {
		t.Fatalf("expected E_PROJECT_NAME_INVALID for 70-char name, got %v", err)
	}

	// 3. --env "../foo"
	_, err = Add(context.Background(), e, "git@github.com:myorg/repo.git", AddOptions{Project: "my-app", Env: "../foo"})
	if err == nil {
		t.Fatal("expected error for --env '../foo', got nil")
	}
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_ENV_INVALID" {
		t.Fatalf("expected E_PROJECT_ENV_INVALID for invalid env, got %v", err)
	}

	// Assert fake FS has no files created (none of the error cases touched FS)
	if len(fsys.Files()) != 0 {
		t.Fatalf("expected fake FS to have no files, got %d files: %+v", len(fsys.Files()), fsys.Files())
	}
}

func TestUpdatePathTraversalAndValidation(t *testing.T) {
	fsys := systest.NewFS()
	runner := &systest.Runner{}
	e := &Env{
		FS:         fsys,
		Run:        runner,
		DeployRoot: "/srv/deploy",
		StateRoot:  "/var/lib/cvctl",
	}

	_, err := Update(context.Background(), e, "../x")
	if err == nil {
		t.Fatal("expected error for Update('../x'), got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_NAME_INVALID" {
		t.Fatalf("expected E_PROJECT_NAME_INVALID, got %v", err)
	}

	// Assert fake FS has no files created
	if len(fsys.Files()) != 0 {
		t.Fatalf("expected fake FS to have no files, got %d files: %+v", len(fsys.Files()), fsys.Files())
	}
}

func TestRemoveAllNeverFollowsSymlinks(t *testing.T) {
	outsideDir := t.TempDir()
	targetFile := filepath.Join(outsideDir, "target.txt")
	if err := os.WriteFile(targetFile, []byte("preserve me"), 0o644); err != nil {
		t.Fatal(err)
	}

	cleanDir := t.TempDir()
	treeDir := filepath.Join(cleanDir, "tree")
	if err := os.MkdirAll(treeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(treeDir, "link_to_outside")
	if err := os.Symlink(outsideDir, symlinkPath); err != nil {
		t.Fatal(err)
	}

	fsys := sys.NewOSFS()
	if err := removeAll(fsys, treeDir); err != nil {
		t.Fatalf("removeAll failed: %v", err)
	}

	if _, err := os.Stat(targetFile); err != nil {
		t.Errorf("expected target file outside tree to still exist, but got: %v", err)
	}
	if _, err := os.Stat(outsideDir); err != nil {
		t.Errorf("expected target dir outside tree to still exist, but got: %v", err)
	}
}

func TestRepoURLAllowlist(t *testing.T) {
	fsys := systest.NewFS()
	runner := &systest.Runner{}
	e := &Env{
		FS:         fsys,
		Run:        runner,
		DeployRoot: "/srv/deploy",
		StateRoot:  "/var/lib/cvctl",
	}

	t.Setenv("CVCTL_ALLOW_FILE_REPOS", "")

	invalidURLs := []string{
		"-u/path/to/repo",
		"--upload-pack=evil",
		"ext::sh -c evil",
		"/tmp/local/repo",
		"./relative/repo",
		"relative/repo",
		"http://github.com/myorg/repo.git",
		"file:///tmp/repo-infra",
	}

	for _, url := range invalidURLs {
		_, err := Add(context.Background(), e, url, AddOptions{})
		if err == nil {
			t.Errorf("expected error for URL %q, got nil", url)
			continue
		}
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Code != "E_PROJECT_REPO_URL" {
			t.Errorf("expected E_PROJECT_REPO_URL for %q, got %v", url, err)
		}
		if oe != nil {
			if !strings.Contains(oe.Hint, "ssh://") || !strings.Contains(oe.Hint, "git@") || !strings.Contains(oe.Hint, "https://") {
				t.Errorf("expected hint to list accepted forms for %q, got %q", url, oe.Hint)
			}
		}
	}

	if len(fsys.Files()) != 0 {
		t.Fatalf("expected fake FS to have no files, got %d files", len(fsys.Files()))
	}
}

func TestDataModeValidation(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	compose := `
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
  main-db:
    image: postgres:16
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: MAIN_DB
      cvctl.external: allowed
x-cvctl:
  health:
    service: web
    http: /healthz
`
	spec := `
vars:
  MAIN_DB_URL:
    source: service
    service: main-db
`
	writeTestInfra(t, infraDir, compose, spec)
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	// 1. Non-interactive with invalid answer -> E_PROJECT_ANSWER_INVALID
	prompterInvalid := hostcfgtest.NewPrompter(t, map[string]string{
		"data_main-db_mode": "other",
	})
	eNonInt := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: false,
		Catalog:     cat,
		Prompt:      prompterInvalid,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}
	_, err := Add(context.Background(), eNonInt, "git@github.com:myorg/repo.git", AddOptions{})
	if err == nil {
		t.Fatal("expected error for invalid data mode in non-interactive, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_ANSWER_INVALID" {
		t.Fatalf("expected E_PROJECT_ANSWER_INVALID, got %v", err)
	}

	// 2. Interactive with whitespace and uppercase: "  LOCAL  " -> trimmed & lowercased to "local"
	prompterTrim := hostcfgtest.NewPrompter(t, map[string]string{
		"data_main-db_mode": "  LOCAL  ",
		"domain_web":        "web.example.com",
	})
	eInt := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  filepath.Join(tmp, "deploy-trim"),
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl-trim"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompterTrim,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}
	res, err := Add(context.Background(), eInt, "git@github.com:myorg/repo.git", AddOptions{})
	if err != nil {
		t.Fatalf("Add failed with trimmed/lowercased answer: %v", err)
	}
	if res.Decisions.Services["main-db"].Mode != "local" {
		t.Errorf("expected mode local, got %s", res.Decisions.Services["main-db"].Mode)
	}

	// 3. Interactive with invalid answer first, then valid answer on re-ask
	seqPrompt := &sequencePrompter{
		answers: map[string][]string{
			"data_main-db_mode":    {"bad-input", "external"},
			"data_main-db_ext_url": {"postgres://host:5432/db"},
			"domain_web":           {"web.example.com"},
		},
		idx: make(map[string]int),
	}
	eReask := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  filepath.Join(tmp, "deploy-reask"),
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl-reask"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      seqPrompt,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}
	resReask, err := Add(context.Background(), eReask, "git@github.com:myorg/repo.git", AddOptions{})
	if err != nil {
		t.Fatalf("Add failed on re-ask: %v", err)
	}
	if resReask.Decisions.Services["main-db"].Mode != "external" {
		t.Errorf("expected mode external, got %s", resReask.Decisions.Services["main-db"].Mode)
	}
	if seqPrompt.idx["data_main-db_mode"] != 2 {
		t.Errorf("expected data_main-db_mode to be asked twice, got %d", seqPrompt.idx["data_main-db_mode"])
	}
}

type sequencePrompter struct {
	answers map[string][]string
	idx     map[string]int
}

func (s *sequencePrompter) Ask(q hostcfg.Question, def string) (string, error) {
	list := s.answers[q.Key]
	i := s.idx[q.Key]
	if i < len(list) {
		s.idx[q.Key]++
		return list[i], nil
	}
	return def, nil
}

func TestUpdateAppEnvMalformedAndMerge(t *testing.T) {
	tmp := t.TempDir()
	inst := "my-service-dev"
	instDir := filepath.Join(tmp, inst)
	infraDir := filepath.Join(instDir, "infra")

	cat := testCatalog(t)
	stub := &stubRepo{}

	existingCvctlEnv := `CVCTL_INSTANCE=my-service-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_DOMAIN_WEB=web.example.com
`
	// 1. Malformed app.env -> E_APPENV_READ, file unchanged
	malformedAppEnv := "EXPORT_FOO=\"unterminated_quote"
	writeTestInfra(t, infraDir, validCompose, validSpec)
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	appEnvPath := filepath.Join(instDir, "app.env")
	if err := os.WriteFile(appEnvPath, []byte(malformedAppEnv), 0o640); err != nil {
		t.Fatal(err)
	}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(_, _ string) RepoLike { return stub },
	}

	_, err := Update(context.Background(), e, inst)
	if err == nil {
		t.Fatal("expected error for malformed app.env, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_APPENV_READ" {
		t.Fatalf("expected E_APPENV_READ, got %v", err)
	}

	// Verify file is unchanged
	afterData, rErr := os.ReadFile(appEnvPath)
	if rErr != nil {
		t.Fatal(rErr)
	}
	if string(afterData) != malformedAppEnv {
		t.Errorf("expected malformed app.env to remain unchanged, got %q", string(afterData))
	}

	// 2. Valid app.env with existing keys: merge without dropping existing keys
	validExistingAppEnv := "PRESERVED_KEY=preserved_value\n"
	if err := os.WriteFile(appEnvPath, []byte(validExistingAppEnv), 0o640); err != nil {
		t.Fatal(err)
	}

	specWithNewVar := `
vars:
  DB_URL:
    source: service
    service: db
  PRESERVED_KEY:
    source: prompt
  NEW_PROMPT_VAR:
    source: prompt
`
	writeTestInfra(t, infraDir, validCompose, specWithNewVar)
	prompter := hostcfgtest.NewPrompter(t, map[string]string{
		"env_NEW_PROMPT_VAR": "added_value",
	})
	e.Prompt = prompter
	e.Interactive = true

	_, err = Update(context.Background(), e, inst)
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	mergedData, err := os.ReadFile(appEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	mergedContent := string(mergedData)
	if !strings.Contains(mergedContent, "PRESERVED_KEY=preserved_value") {
		t.Errorf("expected PRESERVED_KEY preserved, got:\n%s", mergedContent)
	}
	if !strings.Contains(mergedContent, "NEW_PROMPT_VAR=added_value") {
		t.Errorf("expected NEW_PROMPT_VAR added, got:\n%s", mergedContent)
	}
}

func TestUpdateAppEnvMalformedHidesSecrets(t *testing.T) {
	tmp := t.TempDir()
	inst := "my-service-dev"
	instDir := filepath.Join(tmp, inst)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}

	infraDir := filepath.Join(instDir, "infra")
	cat := testCatalog(t)
	stub := &stubRepo{}

	existingCvctlEnv := `CVCTL_INSTANCE=my-service-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_DOMAIN_WEB=web.example.com
`
	writeTestInfra(t, infraDir, validCompose, validSpec)
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	appEnvPath := filepath.Join(instDir, "app.env")
	if err := os.WriteFile(appEnvPath, []byte("SECRET=\"topsecretvalue\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(_, _ string) RepoLike { return stub },
	}

	_, err := Update(context.Background(), e, inst)
	if err == nil {
		t.Fatal("expected error for malformed app.env, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_APPENV_READ" {
		t.Fatalf("expected E_APPENV_READ, got %v", err)
	}
	if strings.Contains(oe.Message, "topsecretvalue") {
		t.Errorf("error message leaked secret: %s", oe.Message)
	}
	if strings.Contains(oe.Hint, "topsecretvalue") {
		t.Errorf("error hint leaked secret: %s", oe.Hint)
	}
	expectedMsg := fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", appEnvPath)
	if oe.Message != expectedMsg {
		t.Errorf("expected message %q, got %q", expectedMsg, oe.Message)
	}
}

func TestDeployKeyPreservesExistingPrivateKeyWhenPubMissing(t *testing.T) {
	t.Setenv("CVCTL_ALLOW_FILE_REPOS", "1")
	tmp := t.TempDir()
	fsys := sys.NewOSFS()
	cat := testCatalog(t)

	infraSrc := filepath.Join(tmp, "source-infra")
	writeTestInfra(t, infraSrc, validCompose, validSpec)

	stateRoot := filepath.Join(tmp, "var-lib-cvctl")
	inst := "demo-dev"
	keyDir := filepath.Join(stateRoot, inst)
	keyPath := filepath.Join(keyDir, "deploy_key")

	// Pre-create private key
	privPEM, _, err := gitrepo.NewDeployKey("custom-key")
	if err != nil {
		t.Fatalf("failed to generate key: %v", err)
	}
	if err := fsys.MkdirAll(keyDir, 0o750, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("failed to mkdir: %v", err)
	}
	if err := fsys.WriteFile(keyPath, privPEM, 0o600, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("failed to write key: %v", err)
	}

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   stateRoot,
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			return &stubRepo{dir: dir, infraSource: infraSrc}
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:codeavers/infra.git", AddOptions{
		Project: "demo",
		Env:     "dev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify private key was NOT overwritten
	currentPriv, err := fsys.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("cannot read private key: %v", err)
	}
	if string(currentPriv) != string(privPEM) {
		t.Errorf("private key was overwritten! expected %s, got %s", string(privPEM), string(currentPriv))
	}

	// Verify public key was regenerated from existing private key
	pubPath := keyPath + ".pub"
	currentPub, err := fsys.ReadFile(pubPath)
	if err != nil {
		t.Fatalf("cannot read public key: %v", err)
	}
	if string(currentPub) != res.DeployKeyPub {
		t.Errorf("expected deploy key pub to match file: %s vs %s", res.DeployKeyPub, string(currentPub))
	}
}

func TestProjectRepoURLRedactsCredentials(t *testing.T) {
	tmp := t.TempDir()
	e := &Env{
		DeployRoot: tmp,
		StateRoot:  filepath.Join(tmp, "var-lib-cvctl"),
	}
	badURL := "https://myuser:secretpassword@-bad-host/repo"
	_, err := Add(context.Background(), e, badURL, AddOptions{})
	if err == nil {
		t.Fatal("expected error for bad repo URL, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_REPO_URL" {
		t.Fatalf("expected E_PROJECT_REPO_URL, got %v", err)
	}
	if strings.Contains(oe.Message, "secretpassword") {
		t.Errorf("error message leaked secret: %s", oe.Message)
	}
	if !strings.Contains(oe.Message, "https://***@-bad-host") {
		t.Errorf("expected message to contain redacted https URL, got: %s", oe.Message)
	}
}

func TestStateDirCreated0750(t *testing.T) {
	tmp := t.TempDir()
	fsys := systest.NewFS()
	cat := testCatalog(t)

	infraSrc := filepath.Join(tmp, "source-infra")
	writeTestInfra(t, infraSrc, validCompose, validSpec)

	stateRoot := filepath.Join(tmp, "var-lib-cvctl")
	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   stateRoot,
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			return &stubRepo{dir: dir, infraSource: infraSrc, fs: fsys}
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:codeavers/infra.git", AddOptions{
		Project: "demo",
		Env:     "dev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	keyDir := filepath.Join(stateRoot, "demo-dev")
	fi, err := fsys.Stat(keyDir)
	if err != nil {
		t.Fatalf("failed to stat keyDir: %v", err)
	}
	if fi.Mode != 0o750 {
		t.Errorf("expected keyDir mode 0750, got %#o", fi.Mode)
	}
}

func TestProjectAddParentOfInfraCreatedBeforeClone(t *testing.T) {
	tmp := t.TempDir()
	fsys := systest.NewFS()
	cat := testCatalog(t)

	infraSrc := filepath.Join(tmp, "source-infra")
	writeTestInfra(t, infraSrc, validCompose, validSpec)

	var parentMode os.FileMode
	var parentOwner sys.Owner
	var parentExistsBeforeClone bool

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "var-lib-cvctl"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			return &recordParentRepo{
				stubRepo: stubRepo{dir: dir, infraSource: infraSrc, fs: fsys},
				dir:      dir,
				cloneHook: func() {
					parent := filepath.Dir(dir)
					if fi, err := fsys.Stat(parent); err == nil {
						parentExistsBeforeClone = true
						parentMode = fi.Mode
						parentOwner = sys.Owner{User: fi.User, Group: fi.Group}
					}
				},
			}
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:codeavers/infra.git", AddOptions{
		Project: "demo",
		Env:     "dev",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !parentExistsBeforeClone {
		t.Fatalf("parent directory /srv/deploy/<inst> did not exist before clone")
	}
	if parentMode != 0o2775 {
		t.Errorf("expected parent mode 02775, got %#o", parentMode)
	}
	if parentOwner.User != "deployer" || parentOwner.Group != "deploy" {
		t.Errorf("expected parent owner deployer:deploy, got %s:%s", parentOwner.User, parentOwner.Group)
	}
}

type recordParentRepo struct {
	stubRepo
	dir       string
	cloneHook func()
}

func (r *recordParentRepo) Clone(ctx context.Context, url, branch string) error {
	if r.cloneHook != nil {
		r.cloneHook()
	}
	return r.stubRepo.Clone(ctx, url, branch)
}

func TestProjectAddInitialStateSavedBeforeDeploy(t *testing.T) {
	tmp := t.TempDir()
	fsys := systest.NewFS()
	cat := testCatalog(t)

	infraSrc := filepath.Join(tmp, "source-infra")
	writeTestInfra(t, infraSrc, validCompose, validSpec)

	stateRoot := filepath.Join(tmp, "var-lib-cvctl")
	store := state.NewStore(fsys, stateRoot)

	// Deployer with nil Compose / Health will fail in Deploy immediately with E_DEPLOY_MISCONFIGURED
	brokenDeployer := &deploy.Deployer{
		DeployRoot: tmp,
		StateRoot:  stateRoot,
		FS:         fsys,
		Store:      store,
		// Compose and Health nil -> deploy fails
	}

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   stateRoot,
		Interactive: false,
		Catalog:     cat,
		Deployer:    brokenDeployer,
		RepoFactory: func(dir, _ string) RepoLike {
			return &stubRepo{dir: dir, infraSource: infraSrc, fs: fsys}
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:codeavers/infra.git", AddOptions{
		Project: "demo",
		Env:     "dev",
	})
	if err == nil {
		t.Fatal("expected deploy error, got nil")
	}

	// Verify state.json exists and has initial values
	st, err := store.Load("demo-dev")
	if err != nil {
		t.Fatalf("failed to load state: %v", err)
	}
	if st.Status != state.StatusFailed {
		t.Errorf("expected status FAILED, got %s", st.Status)
	}
	if st.Detail != "initial deploy pending" {
		t.Errorf("expected detail 'initial deploy pending', got %q", st.Detail)
	}
	if st.FailedHash != "" {
		t.Errorf("expected empty FailedHash, got %q", st.FailedHash)
	}

	instances, err := store.Instances()
	if err != nil {
		t.Fatalf("failed to list instances: %v", err)
	}
	if !slices.Contains(instances, "demo-dev") {
		t.Errorf("expected store.Instances() to contain demo-dev, got %v", instances)
	}
}

type noopCompose struct{}

func (noopCompose) Config(context.Context) error           { return nil }
func (noopCompose) Pull(context.Context, ...string) error  { return nil }
func (noopCompose) UpData(context.Context, []string) error { return nil }
func (noopCompose) RunHook(context.Context, string, string) (sys.Result, error) {
	return sys.Result{}, nil
}
func (noopCompose) Up(context.Context) error                          { return nil }
func (noopCompose) Logs(context.Context, string, int) (string, error) { return "", nil }

type fixedResolver struct{}

func (fixedResolver) Digest(context.Context, string, string) (string, error) {
	return "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", nil
}

const updateV1Env = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_SVC_DB_MODE=local
CVCTL_SVC_DB_VERSION=16
CVCTL_SVC_DB_USER=db
CVCTL_SVC_DB_PASSWORD=existingpass123456789012345678
CVCTL_SVC_DB_DB=db
CVCTL_DOMAIN_WEB=web.example.com
CVCTL_SVC_GONE_MODE=local
CVCTL_SVC_GONE_PASSWORD=keepmekeepmekeepmekeepmekeepme12
`

// updateRig simulates an instance deployed from infra v1 whose origin has since
// gained a second postgres service ("extra").
type updateRig struct {
	env      *Env
	prompter *hostcfgtest.Prompter
	store    *state.Store
	instDir  string
	infraDir string
	log      *[]string
	stub     *stubRepo
}

func newUpdateRig(t *testing.T, answers map[string]string) *updateRig {
	t.Helper()
	tmp := t.TempDir()
	instDir := filepath.Join(tmp, "demo-dev")
	infraDir := filepath.Join(instDir, "infra")
	originDir := filepath.Join(tmp, "origin")

	writeTestInfra(t, infraDir, validCompose, validSpec)
	v2Compose := strings.Replace(validCompose, "x-cvctl:", `  extra:
    image: postgres:16
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: EXTRA
      cvctl.external: allowed
x-cvctl:`, 1)
	v2Spec := validSpec + `  EXTRA_URL:
    source: service
    service: extra
`
	writeTestInfra(t, originDir, v2Compose, v2Spec)
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(updateV1Env), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(filepath.Join(tmp, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}

	fl := systest.NewFakeLocker()
	oldLock := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLock })
	sys.LockFile = fl.LockFile

	osfs := sys.NewOSFS()
	stateRoot := filepath.Join(tmp, "state")
	store := state.NewStore(osfs, stateRoot)
	cat := testCatalog(t)
	deployer := &deploy.Deployer{
		DeployRoot: tmp,
		TraefikDir: filepath.Join(tmp, "traefik"),
		StateRoot:  stateRoot,
		FS:         osfs,
		Store:      store,
		Catalog:    cat,
		Compose:    func(_, _, _ string) deploy.ComposeAPI { return noopCompose{} },
		Health:     func(context.Context, string, string) error { return nil },
	}

	log := &[]string{}
	stub := &stubRepo{dir: infraDir, infraSource: originDir, fetchCommit: "feedface00000000", log: log}
	prompter := hostcfgtest.NewPrompter(t, answers)
	e := &Env{
		FS:          osfs,
		DeployRoot:  tmp,
		StateRoot:   stateRoot,
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompter,
		Host:        &hostcfg.Config{Registries: "ghcr.io/codeavers"},
		Deployer:    deployer,
		Resolver:    fixedResolver{},
		RepoFactory: func(string, string) RepoLike { return stub },
	}
	return &updateRig{env: e, prompter: prompter, store: store, instDir: instDir, infraDir: infraDir, log: log, stub: stub}
}

func TestUpdateFetchesChecksOutAndAsksOnlyForNewService(t *testing.T) {
	rig := newUpdateRig(t, map[string]string{"data_extra_mode": "local"})

	res, err := Update(context.Background(), rig.env, "demo-dev")
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if res.Deploy.Status != state.StatusOK {
		t.Fatalf("expected deploy OK, got %v (%s)", res.Deploy.Status, res.Deploy.Detail)
	}
	if len(rig.prompter.Asked) != 1 || rig.prompter.Asked[0] != "data_extra_mode" {
		t.Fatalf("expected only data_extra_mode to be asked, got %v", rig.prompter.Asked)
	}
	if got := strings.Join(*rig.log, ","); !strings.HasPrefix(got, "fetch develop,checkout feedface00000000") {
		t.Fatalf("expected fetch then checkout of the fetched commit first, got %s", got)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.Current == nil || st.Current.InfraCommit != "feedface00000000" {
		t.Fatalf("expected current commit feedface00000000, got %+v", st.Current)
	}
}

func TestUpdateHoldsInstanceLockDuringDeploy(t *testing.T) {
	rig := newUpdateRig(t, map[string]string{"data_extra_mode": "local"})
	rig.stub.onFetch = func() {
		if _, err := rig.store.Lock("demo-dev"); err == nil {
			t.Error("instance lock must be held while Update fetches")
		}
	}
	if _, err := Update(context.Background(), rig.env, "demo-dev"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	unlock, err := rig.store.Lock("demo-dev")
	if err != nil {
		t.Fatalf("lock must be released after Update: %v", err)
	}
	unlock()
}

func TestUpdatePreservesUnknownCvctlEnvKeys(t *testing.T) {
	rig := newUpdateRig(t, map[string]string{"data_extra_mode": "local"})
	if _, err := Update(context.Background(), rig.env, "demo-dev"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CVCTL_SVC_GONE_MODE=local", "CVCTL_SVC_GONE_PASSWORD=keepmekeepmekeepmekeepmekeepme12", "CVCTL_SVC_EXTRA_MODE=local", "CVCTL_SVC_DB_PASSWORD=existingpass123456789012345678"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("cvctl.env missing %q:\n%s", want, raw)
		}
	}
}
