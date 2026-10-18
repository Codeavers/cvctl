package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

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
	s.record("clone")
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
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: MAIN_DB
      cvctl.external: allowed
  cache:
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
    labels:
      cvctl.kind: postgres
      cvctl.version: "16"
      cvctl.env: DB
  new-db:
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

	res, err := Update(context.Background(), e, inst, UpdateOptions{})
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
	if !errors.As(err, &oe) || oe.Code != "E_INSTANCE_NAME" {
		t.Fatalf("expected E_INSTANCE_NAME, got %v", err)
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
	if !errors.As(err, &oe) || oe.Code != "E_INSTANCE_NAME" {
		t.Fatalf("expected E_INSTANCE_NAME for 70-char name, got %v", err)
	}

	// 2b. double hyphen in instance name
	if err := ValidateInstance("/srv/deploy", "/var/lib/cvctl", "my--app-dev", "dev"); err == nil {
		t.Fatal("expected error for instance with '--', got nil")
	} else if !errors.As(err, &oe) || oe.Code != "E_INSTANCE_NAME" {
		t.Fatalf("expected E_INSTANCE_NAME for double hyphen, got %v", err)
	}

	// 3. --env "../foo"
	_, err = Add(context.Background(), e, "git@github.com:myorg/repo.git", AddOptions{Project: "my-app", Env: "../foo"})
	if err == nil {
		t.Fatal("expected error for --env '../foo', got nil")
	}
	if !errors.As(err, &oe) || oe.Code != "E_PROJECT_ENV_INVALID" {
		t.Fatalf("expected E_PROJECT_ENV_INVALID for invalid env, got %v", err)
	}

	// 4. --set with newline
	_, err = Add(context.Background(), e, "git@github.com:myorg/repo.git", AddOptions{
		Project: "my-app",
		Env:     "dev",
		Set:     map[string]string{"FOO": "bad\nvalue"},
	})
	if err == nil {
		t.Fatal("expected error for --set with newline, got nil")
	}
	if !errors.As(err, &oe) || oe.Code != "E_ENVSPEC_VALUE" {
		t.Fatalf("expected E_ENVSPEC_VALUE for --set with newline, got %v", err)
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

	_, err := Update(context.Background(), e, "../x", UpdateOptions{})
	if err == nil {
		t.Fatal("expected error for Update('../x'), got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_INSTANCE_NAME" {
		t.Fatalf("expected E_INSTANCE_NAME, got %v", err)
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

	_, err := Update(context.Background(), e, inst, UpdateOptions{})
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

	_, err = Update(context.Background(), e, inst, UpdateOptions{})
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

	_, err := Update(context.Background(), e, inst, UpdateOptions{})
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

func (noopCompose) Config(context.Context) error                          { return nil }
func (noopCompose) Pull(context.Context, ...string) error                 { return nil }
func (noopCompose) UpData(context.Context, []string, time.Duration) error { return nil }
func (noopCompose) ExecScript(context.Context, string, string) (sys.Result, error) {
	return sys.Result{}, nil
}

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
	netRun := &systest.Runner{}
	netRun.OnPrefix("docker network", sys.Result{})
	deployer := &deploy.Deployer{
		Run:        netRun,
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

	res, err := Update(context.Background(), rig.env, "demo-dev", UpdateOptions{})
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
	if _, err := Update(context.Background(), rig.env, "demo-dev", UpdateOptions{}); err != nil {
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
	if _, err := Update(context.Background(), rig.env, "demo-dev", UpdateOptions{}); err != nil {
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

const testS3Compose = `
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
  media:
    labels:
      cvctl.kind: s3
      cvctl.env: S3
      cvctl.external: allowed
x-cvctl:
  health:
    service: web
    http: /healthz
`

const testS3Spec = `
vars:
  S3_ENDPOINT:
    source: service
    service: media
`

const testS3BucketsCompose = `
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
  media:
    labels:
      cvctl.kind: s3
      cvctl.env: S3
      cvctl.expose: "9000"
      cvctl.external: allowed
x-cvctl:
  health:
    service: web
    http: /healthz
  s3:
    media:
      buckets:
        photos:
          public: true
        backups: {}
`

const testS3BucketsSpec = `
vars:
  S3_ENDPOINT:
    source: service
    service: media
  S3_BUCKET_PHOTOS:
    source: service
    service: media
  S3_PUBLIC_URL_PHOTOS:
    source: service
    service: media
  S3_BUCKET_BACKUPS:
    source: service
    service: media
`

func TestAddLocalS3GeneratesAppCreds(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3Compose, testS3Spec)

	fsys := sys.NewOSFS()
	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          fsys,
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Domains: map[string]string{"web": "web.example.com"},
	})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	cvctlEnvPath := filepath.Join(tmp, "my-service-dev", "cvctl.env")
	data, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatalf("read cvctl.env: %v", err)
	}
	content := string(data)

	if !strings.Contains(content, "CVCTL_SVC_MEDIA_APP_USER=media-app") &&
		!strings.Contains(content, `CVCTL_SVC_MEDIA_APP_USER="media-app"`) {
		t.Errorf("expected CVCTL_SVC_MEDIA_APP_USER=media-app in cvctl.env, got:\n%s", content)
	}

	sd := res.Decisions.Services["media"]
	if sd.Creds.AppUser != "media-app" {
		t.Errorf("expected AppUser media-app, got %q", sd.Creds.AppUser)
	}
	if len(sd.Creds.AppPassword) != 32 {
		t.Errorf("expected 32-char AppPassword, got %q", sd.Creds.AppPassword)
	}
	if !strings.Contains(content, sd.Creds.AppPassword) {
		t.Errorf("expected cvctl.env to contain generated app password %q", sd.Creds.AppPassword)
	}
}

func TestUpdateFillsAppCredsOnOldCvctlEnv(t *testing.T) {
	tmp := t.TempDir()
	inst := "my-service-dev"
	instDir := filepath.Join(tmp, inst)
	infraDir := filepath.Join(instDir, "infra")
	cat := testCatalog(t)
	stub := &stubRepo{}

	oldCvctlEnv := `CVCTL_INSTANCE=my-service-dev
CVCTL_ENV=dev
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_SVC_MEDIA_MODE=local
CVCTL_SVC_MEDIA_VERSION=RELEASE.2026-09-16T00-00-00Z
CVCTL_SVC_MEDIA_USER=media
CVCTL_SVC_MEDIA_PASSWORD=existingpass123456789012345678
CVCTL_SVC_MEDIA_DB=media
CVCTL_DOMAIN_WEB=web.example.com
`
	writeTestInfra(t, infraDir, testS3Compose, testS3Spec)
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(oldCvctlEnv), 0o640); err != nil {
		t.Fatalf("write cvctl.env: %v", err)
	}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(_, _ string) RepoLike {
			return stub
		},
	}

	res, err := Update(context.Background(), e, inst, UpdateOptions{})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	sd := res.Decisions.Services["media"]
	if sd.Creds.Password != "existingpass123456789012345678" {
		t.Errorf("expected existing password preserved, got %q", sd.Creds.Password)
	}
	if sd.Creds.AppUser != "media-app" {
		t.Errorf("expected AppUser media-app, got %q", sd.Creds.AppUser)
	}
	if len(sd.Creds.AppPassword) != 32 {
		t.Fatalf("expected 32-char AppPassword, got %q", sd.Creds.AppPassword)
	}

	// Re-run Update and assert AppPassword is never regenerated
	res2, err := Update(context.Background(), e, inst, UpdateOptions{})
	if err != nil {
		t.Fatalf("second Update failed: %v", err)
	}
	if res2.Decisions.Services["media"].Creds.AppPassword != sd.Creds.AppPassword {
		t.Errorf("app password was regenerated: got %q, want %q", res2.Decisions.Services["media"].Creds.AppPassword, sd.Creds.AppPassword)
	}
}

func TestExternalPerBucketQuestionsDefaultsAndAskedOrder(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	answers := map[string]string{
		"data_media_mode":                  "external",
		"data_media_ext_endpoint":          "https://s3.example.com",
		"data_media_ext_access_key":        "ak123",
		"data_media_ext_secret_key":        "sk123",
		"data_media_ext_region":            "", // should default to "auto"
		"data_media_ext_bucket_photos":     "", // should default to "photos"
		"data_media_ext_public_url_photos": "https://cdn.example.com/photos",
		"data_media_ext_bucket_backups":    "", // should default to "backups"
		"domain_web":                       "web.example.com",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
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

	wantAsked := []string{
		"data_media_mode",
		"data_media_ext_endpoint",
		"data_media_ext_access_key",
		"data_media_ext_secret_key",
		"data_media_ext_region",
		"data_media_ext_bucket_backups",
		"data_media_ext_bucket_photos",
		"data_media_ext_public_url_photos",
		"domain_web",
	}
	if !slices.Equal(prompter.Asked, wantAsked) {
		t.Fatalf("prompter.Asked = %v, want %v", prompter.Asked, wantAsked)
	}

	ext := res.Decisions.Services["media"].External
	if ext["region"] != "auto" {
		t.Errorf("expected region auto, got %q", ext["region"])
	}
	if ext["bucket_photos"] != "photos" {
		t.Errorf("expected bucket_photos photos, got %q", ext["bucket_photos"])
	}
	if ext["public_url_photos"] != "https://cdn.example.com/photos" {
		t.Errorf("expected public_url_photos https://cdn.example.com/photos, got %q", ext["public_url_photos"])
	}
	if ext["bucket_backups"] != "backups" {
		t.Errorf("expected bucket_backups backups, got %q", ext["bucket_backups"])
	}
}

func TestAddExtNonInteractiveHappyPath(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Env:     "prod",
		Domains: map[string]string{"web": "web.example.com"},
		Ext: map[string]map[string]string{
			"media": {
				"endpoint":          "https://s3.amazonaws.com",
				"access_key":        "prod-ak",
				"secret_key":        "prod-sk",
				"public_url_photos": "https://cdn.example.com/photos",
			},
		},
	})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	sd := res.Decisions.Services["media"]
	if sd.Mode != "external" {
		t.Fatalf("expected Mode external, got %q", sd.Mode)
	}
	if sd.External["endpoint"] != "https://s3.amazonaws.com" {
		t.Errorf("expected endpoint https://s3.amazonaws.com, got %q", sd.External["endpoint"])
	}
	if sd.External["region"] != "auto" {
		t.Errorf("expected region auto, got %q", sd.External["region"])
	}
	if sd.External["bucket_photos"] != "photos" {
		t.Errorf("expected bucket_photos photos, got %q", sd.External["bucket_photos"])
	}
	if sd.External["public_url_photos"] != "https://cdn.example.com/photos" {
		t.Errorf("expected public_url_photos https://cdn.example.com/photos, got %q", sd.External["public_url_photos"])
	}
	if sd.External["bucket_backups"] != "backups" {
		t.Errorf("expected bucket_backups backups, got %q", sd.External["bucket_backups"])
	}
}

func TestAddWithDomainMedia(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Env: "dev",
		Domains: map[string]string{
			"web":   "web.example.com",
			"media": "media.example.com",
		},
	})
	if err != nil {
		t.Fatalf("Add with --domain media failed: %v", err)
	}

	if res.Decisions.Domains["media"] != "media.example.com" {
		t.Errorf("expected media domain media.example.com, got %q", res.Decisions.Domains["media"])
	}

	cvctlEnvPath := filepath.Join(tmp, "my-service-dev", "cvctl.env")
	data, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatalf("read cvctl.env: %v", err)
	}
	if !strings.Contains(string(data), "CVCTL_DOMAIN_MEDIA=media.example.com") {
		t.Errorf("expected CVCTL_DOMAIN_MEDIA in cvctl.env, got:\n%s", string(data))
	}
}

func TestAddProdPublicLocalRejectedNothingWritten(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	answers := map[string]string{
		"data_media_mode": "local",
		"domain_web":      "web.example.com",
		"domain_media":    "media.example.com",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompter,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Env: "prod",
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var issuesErr *output.IssuesError
	if !errors.As(err, &issuesErr) {
		t.Fatalf("expected *output.IssuesError, got %T: %v", err, err)
	}
	found := false
	for _, iss := range issuesErr.Issues {
		if iss.Code == "E_S3_PUBLIC_LOCAL_PROD" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected E_S3_PUBLIC_LOCAL_PROD issue, got: %+v", issuesErr.Issues)
	}

	// Verify nothing was written / instance directory was cleaned up
	instDir := filepath.Join(tmp, "my-service-prod")
	if _, statErr := os.Stat(instDir); !os.IsNotExist(statErr) {
		t.Fatalf("expected instance dir %s to be cleaned up, stat: %v", instDir, statErr)
	}
}

func TestProdDefaultModeIsExternal(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	answers := map[string]string{
		"data_media_mode":                  "", // accept default
		"data_media_ext_endpoint":          "https://s3.example.com",
		"data_media_ext_access_key":        "ak",
		"data_media_ext_secret_key":        "sk",
		"data_media_ext_region":            "",
		"data_media_ext_bucket_photos":     "",
		"data_media_ext_public_url_photos": "https://cdn.example.com/photos",
		"data_media_ext_bucket_backups":    "",
		"domain_web":                       "web.example.com",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
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
		Env: "prod",
	})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	if res.Decisions.Services["media"].Mode != "external" {
		t.Fatalf("expected Mode external, got %q", res.Decisions.Services["media"].Mode)
	}
}

func TestAddNonInteractiveDevExtImpliesExternalMode(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}

	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		Env:     "dev",
		Domains: map[string]string{"web": "web.example.com"},
		Ext: map[string]map[string]string{
			"media": {
				"endpoint":          "https://s3.amazonaws.com",
				"access_key":        "dev-ak",
				"secret_key":        "dev-sk",
				"public_url_photos": "https://cdn.example.com/photos",
			},
		},
	})
	if err != nil {
		t.Fatalf("Add failed: %v", err)
	}

	if res.Decisions.Services["media"].Mode != "external" {
		t.Fatalf("expected Mode external, got %q", res.Decisions.Services["media"].Mode)
	}

	instDir := filepath.Join(tmp, "my-service-dev")
	rawCvctl, err := os.ReadFile(filepath.Join(instDir, "cvctl.env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawCvctl), "CVCTL_SVC_MEDIA_MODE=external") {
		t.Fatalf("expected CVCTL_SVC_MEDIA_MODE=external in cvctl.env, got:\n%s", string(rawCvctl))
	}
}

func TestAddExtRejectsUnknownFieldNames(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)

	cat := testCatalog(t)
	stub := &stubRepo{infraSource: infraDir}

	newEnv := func() *Env {
		return &Env{
			FS:          sys.NewOSFS(),
			DeployRoot:  tmp,
			StateRoot:   filepath.Join(tmp, "state"),
			Interactive: false,
			Catalog:     cat,
			RepoFactory: func(dir, _ string) RepoLike {
				r := *stub
				r.dir = dir
				return &r
			},
		}
	}

	cases := []struct {
		name      string
		ext       map[string]map[string]string
		wantField string
	}{
		{
			name: "unknown field on kind",
			ext: map[string]map[string]string{
				"media": {"nonexistent_field": "secret123"},
			},
			wantField: "media.nonexistent_field",
		},
		{
			name: "bucket field when buckets declared",
			ext: map[string]map[string]string{
				"media": {"bucket": "secretbucket"},
			},
			wantField: "media.bucket",
		},
		{
			name: "public_url on private bucket",
			ext: map[string]map[string]string{
				"media": {"public_url_backups": "https://cdn.example.com"},
			},
			wantField: "media.public_url_backups",
		},
		{
			name: "bucket on undeclared bucket",
			ext: map[string]map[string]string{
				"media": {"bucket_unknown": "my-bucket"},
			},
			wantField: "media.bucket_unknown",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv()
			_, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
				Env:     "dev",
				Domains: map[string]string{"web": "web.example.com"},
				Ext:     tc.ext,
			})
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ue *output.UsageError
			if !errors.As(err, &ue) {
				t.Fatalf("expected *output.UsageError, got %T: %v", err, err)
			}
			var oe *output.Error
			if !errors.As(ue.Err, &oe) {
				t.Fatalf("expected *output.Error, got %T", ue.Err)
			}
			if oe.Code != "E_USAGE_EXT" {
				t.Fatalf("expected code E_USAGE_EXT, got %q", oe.Code)
			}
			if !strings.Contains(oe.Message, tc.wantField) {
				t.Fatalf("expected message to name %q, got: %s", tc.wantField, oe.Message)
			}
			// Assert values are absent from error message and hint
			for _, fVals := range tc.ext {
				for _, v := range fVals {
					if strings.Contains(oe.Message, v) {
						t.Fatalf("error message leaked ext value %q: %s", v, oe.Message)
					}
					if strings.Contains(oe.Hint, v) {
						t.Fatalf("error hint leaked ext value %q: %s", v, oe.Hint)
					}
				}
			}
		})
	}
}

func TestUpdateProdPublicLocalRejectedLeavesCvctlEnvByteIdentical(t *testing.T) {
	tmp := t.TempDir()
	inst := "my-service-prod"
	instDir := filepath.Join(tmp, inst)
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}

	infraDir := filepath.Join(instDir, "infra")
	cat := testCatalog(t)
	stub := &stubRepo{}

	existingCvctlEnv := `CVCTL_INSTANCE=my-service-prod
CVCTL_ENV=prod
CVCTL_INFRA_BRANCH=main
CVCTL_TRACK_TAG=main
CVCTL_INFRA_PATH=.
CVCTL_STRATEGY=recreate
CVCTL_SVC_MEDIA_MODE=local
CVCTL_SVC_MEDIA_EXT_ENDPOINT=https://left.over.example.com
CVCTL_SVC_MEDIA_EXT_BUCKET_PRODUCTS=leftover-products
CVCTL_SVC_MEDIA_VERSION=RELEASE.2024-01-01T00-00-00Z
CVCTL_SVC_MEDIA_USER=media
CVCTL_SVC_MEDIA_PASSWORD=existingpass123456789012345678
CVCTL_SVC_MEDIA_DB=media
CVCTL_SVC_MEDIA_APP_USER=media-app
CVCTL_SVC_MEDIA_APP_PASSWORD=existingapppass1234567890123456
CVCTL_DOMAIN_WEB=web.example.com
CVCTL_DOMAIN_MEDIA=media.example.com
`
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	if err := os.WriteFile(cvctlEnvPath, []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	appEnvPath := filepath.Join(instDir, "app.env")
	if err := os.WriteFile(appEnvPath, []byte(""), 0o640); err != nil {
		t.Fatal(err)
	}

	beforeBytes, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatal(err)
	}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(_, _ string) RepoLike { return stub },
	}

	_, err = Update(context.Background(), e, inst, UpdateOptions{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var issuesErr *output.IssuesError
	if !errors.As(err, &issuesErr) {
		t.Fatalf("expected *output.IssuesError, got %T: %v", err, err)
	}
	found := false
	for _, iss := range issuesErr.Issues {
		if iss.Code == "E_S3_PUBLIC_LOCAL_PROD" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected E_S3_PUBLIC_LOCAL_PROD issue, got: %+v", issuesErr.Issues)
	}

	afterBytes, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("expected cvctl.env to remain byte-identical, before:\n%s\nafter:\n%s", string(beforeBytes), string(afterBytes))
	}
}

func TestUpdateNewlyDeclaredBucketAsksOnlyNewBucket(t *testing.T) {
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
CVCTL_SVC_MEDIA_MODE=external
CVCTL_SVC_MEDIA_VERSION=RELEASE.2024-01-01T00-00-00Z
CVCTL_SVC_MEDIA_EXT_ENDPOINT=https://s3.example.com
CVCTL_SVC_MEDIA_EXT_REGION=auto
CVCTL_SVC_MEDIA_EXT_ACCESS_KEY=ak
CVCTL_SVC_MEDIA_EXT_SECRET_KEY=sk
CVCTL_SVC_MEDIA_EXT_BUCKET_BACKUPS=my-backups
CVCTL_DOMAIN_WEB=web.example.com
`
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	if err := os.WriteFile(cvctlEnvPath, []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	appEnvPath := filepath.Join(instDir, "app.env")
	if err := os.WriteFile(appEnvPath, []byte(""), 0o640); err != nil {
		t.Fatal(err)
	}

	answers := map[string]string{
		"data_media_ext_bucket_photos":     "new-photos-bucket",
		"data_media_ext_public_url_photos": "https://cdn.example.com/photos",
	}
	prompter := hostcfgtest.NewPrompter(t, answers)

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: true,
		Catalog:     cat,
		Prompt:      prompter,
		RepoFactory: func(_, _ string) RepoLike { return stub },
	}

	res, err := Update(context.Background(), e, inst, UpdateOptions{})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	wantAsked := []string{
		"data_media_ext_bucket_photos",
		"data_media_ext_public_url_photos",
	}
	if !slices.Equal(prompter.Asked, wantAsked) {
		t.Fatalf("expected asked %v, got %v", wantAsked, prompter.Asked)
	}

	sd := res.Decisions.Services["media"]
	if sd.External["bucket_photos"] != "new-photos-bucket" {
		t.Errorf("expected bucket_photos new-photos-bucket, got %q", sd.External["bucket_photos"])
	}
	if sd.External["public_url_photos"] != "https://cdn.example.com/photos" {
		t.Errorf("expected public_url_photos https://cdn.example.com/photos, got %q", sd.External["public_url_photos"])
	}
	if sd.External["bucket_backups"] != "my-backups" {
		t.Errorf("expected bucket_backups my-backups preserved, got %q", sd.External["bucket_backups"])
	}
}

func TestUpdateExtHappyPath(t *testing.T) {
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
CVCTL_SVC_MEDIA_MODE=external
CVCTL_SVC_MEDIA_VERSION=RELEASE.2024-01-01T00-00-00Z
CVCTL_SVC_MEDIA_EXT_ENDPOINT=https://s3.example.com
CVCTL_SVC_MEDIA_EXT_REGION=auto
CVCTL_SVC_MEDIA_EXT_ACCESS_KEY=ak
CVCTL_SVC_MEDIA_EXT_SECRET_KEY=sk
CVCTL_SVC_MEDIA_EXT_BUCKET_PHOTOS=photos
CVCTL_SVC_MEDIA_EXT_PUBLIC_URL_PHOTOS=https://cdn.example.com/photos
CVCTL_SVC_MEDIA_EXT_BUCKET_BACKUPS=backups
CVCTL_DOMAIN_WEB=web.example.com
`
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	if err := os.WriteFile(cvctlEnvPath, []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	appEnvPath := filepath.Join(instDir, "app.env")
	if err := os.WriteFile(appEnvPath, []byte(""), 0o640); err != nil {
		t.Fatal(err)
	}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(_, _ string) RepoLike { return stub },
	}

	res, err := Update(context.Background(), e, inst, UpdateOptions{
		Ext: map[string]map[string]string{
			"media": {
				"region": "eu-central-1",
			},
		},
	})
	if err != nil {
		t.Fatalf("Update failed: %v", err)
	}

	if res.Decisions.Services["media"].External["region"] != "eu-central-1" {
		t.Fatalf("expected region eu-central-1, got %q", res.Decisions.Services["media"].External["region"])
	}

	rawCvctl, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(rawCvctl), "CVCTL_SVC_MEDIA_EXT_REGION=eu-central-1") {
		t.Fatalf("expected updated region in cvctl.env, got:\n%s", string(rawCvctl))
	}
}

// --ext moves a local service to external; with only some fields given, Check
// rejects the result before anything is written.
func TestUpdateExtSwitchesLocalToExternalAndChecksFields(t *testing.T) {
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
CVCTL_SVC_MEDIA_MODE=local
CVCTL_SVC_MEDIA_VERSION=RELEASE.2024-01-01T00-00-00Z
CVCTL_SVC_MEDIA_USER=media
CVCTL_SVC_MEDIA_PASSWORD=existingpass123456789012345678
CVCTL_SVC_MEDIA_DB=media
CVCTL_SVC_MEDIA_APP_USER=media-app
CVCTL_SVC_MEDIA_APP_PASSWORD=existingapppass1234567890123456
CVCTL_DOMAIN_WEB=web.example.com
CVCTL_DOMAIN_MEDIA=media.example.com
`
	writeTestInfra(t, infraDir, testS3BucketsCompose, testS3BucketsSpec)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	if err := os.WriteFile(cvctlEnvPath, []byte(existingCvctlEnv), 0o640); err != nil {
		t.Fatal(err)
	}
	appEnvPath := filepath.Join(instDir, "app.env")
	if err := os.WriteFile(appEnvPath, []byte(""), 0o640); err != nil {
		t.Fatal(err)
	}

	beforeBytes, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatal(err)
	}

	e := &Env{
		FS:          sys.NewOSFS(),
		DeployRoot:  tmp,
		StateRoot:   filepath.Join(tmp, "state"),
		Interactive: false,
		Catalog:     cat,
		RepoFactory: func(_, _ string) RepoLike { return stub },
	}

	_, err = Update(context.Background(), e, inst, UpdateOptions{
		Ext: map[string]map[string]string{
			"media": {
				"region": "eu-central-1",
			},
		},
	})
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var ie *output.IssuesError
	if !errors.As(err, &ie) {
		t.Fatalf("expected *output.IssuesError, got %T: %v", err, err)
	}
	missingEndpoint := false
	for _, is := range ie.Issues {
		if is.Code == "E_DECISION_MISSING" && strings.Contains(is.Hint, "EXT_ENDPOINT") {
			missingEndpoint = true
		}
	}
	if !missingEndpoint {
		t.Fatalf("expected a missing endpoint issue after switching to external, got %v", ie.Issues)
	}

	afterBytes, err := os.ReadFile(cvctlEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) {
		t.Fatalf("expected cvctl.env to remain byte-identical, before:\n%s\nafter:\n%s", string(beforeBytes), string(afterBytes))
	}
}
