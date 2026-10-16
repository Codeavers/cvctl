package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

type stubRepo struct {
	dir         string
	infraSource string
	cloneErr    error
	lsRemoteErr error
	fetchCommit string
	fetchErr    error
	showData    map[string][]byte
	showErr     error
	checkoutErr error
}

func (s *stubRepo) Clone(_ context.Context, _, _ string) error {
	if s.cloneErr != nil {
		return s.cloneErr
	}
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

func (s *stubRepo) LsRemote(_ context.Context, _ string) error {
	return s.lsRemoteErr
}

func (s *stubRepo) Fetch(_ context.Context, _ string) (string, error) {
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
	return nil, os.ErrNotExist
}

func (s *stubRepo) Checkout(_ context.Context, _ string) error {
	return s.checkoutErr
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
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_FILE_REPO" {
		t.Errorf("expected E_PROJECT_FILE_REPO, got %v", err)
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
