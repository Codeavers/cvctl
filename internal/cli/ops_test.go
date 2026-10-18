package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/project"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

type opsStubRepo struct {
	dir         string
	infraSource string
}

type opsFakeResolver struct{}

func (f *opsFakeResolver) Digest(_ context.Context, _, _ string) (string, error) {
	return "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", nil
}

func (s *opsStubRepo) Clone(_ context.Context, _, _ string) error {
	return nil
}

func (s *opsStubRepo) LsRemote(_ context.Context, _ string) error {
	return nil
}

func (s *opsStubRepo) Fetch(_ context.Context, _ string) (string, error) {
	return "abc1234567890", nil
}

func (s *opsStubRepo) Show(_ context.Context, _, path string) ([]byte, error) {
	if strings.HasSuffix(path, "compose.yml") {
		return os.ReadFile(filepath.Join(s.infraSource, path))
	}
	return []byte("CVCTL_DIGEST_WEB=sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"), nil
}

func (s *opsStubRepo) Checkout(_ context.Context, _ string) error {
	return nil
}

type opsStubCompose struct {
	logsOutput string
	lastTail   int
	lastSvc    string
}

func (c *opsStubCompose) Config(_ context.Context) error                              { return nil }
func (c *opsStubCompose) Pull(_ context.Context, _ ...string) error                   { return nil }
func (c *opsStubCompose) UpData(_ context.Context, _ []string, _ time.Duration) error { return nil }
func (c *opsStubCompose) ExecScript(_ context.Context, _, _ string) (sys.Result, error) {
	return sys.Result{}, nil
}

func (c *opsStubCompose) RunHook(_ context.Context, _, _ string) (sys.Result, error) {
	return sys.Result{ExitCode: 0}, nil
}
func (c *opsStubCompose) Up(_ context.Context) error { return nil }
func (c *opsStubCompose) Logs(_ context.Context, svc string, tail int) (string, error) {
	c.lastSvc = svc
	c.lastTail = tail
	if c.logsOutput != "" {
		return c.logsOutput, nil
	}
	return "app log line 1\napp log line 2\n", nil
}

func setupOpsFixture(t *testing.T, inst string) (string, *OpsEnv) {
	t.Helper()
	tmp := t.TempDir()
	instDir := filepath.Join(tmp, inst)
	stateDir := filepath.Join(tmp, "state")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o755); err != nil {
		t.Fatal(err)
	}

	infraDir := filepath.Join(instDir, "infra")
	writeCLIInfra(t, infraDir)

	cvctlEnv := instance.Decisions{
		Instance:    inst,
		Env:         "dev",
		InfraBranch: "develop",
		TrackTag:    "develop",
		InfraPath:   ".",
		Strategy:    "recreate",
		Domains:     map[string]string{"web": "app.example.com"},
		Digests:     map[string]string{"web": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		Services: map[string]instance.ServiceDecision{
			"db": {
				Mode:    "local",
				Version: "16",
				Creds: kinds.Credentials{
					User:     "dbuser",
					Password: "dbpassword",
					DB:       "dbname",
				},
			},
		},
	}
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), instance.FormatEnv(cvctlEnv.Entries()), 0o640); err != nil {
		t.Fatal(err)
	}

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	store := state.NewStore(sys.NewOSFS(), stateDir)
	stubRepo := &opsStubRepo{infraSource: infraDir}
	stubComp := &opsStubCompose{}

	if err := os.MkdirAll(filepath.Join(tmp, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}

	hcfg := &hostcfg.Config{Registries: "ghcr.io/codeavers"}
	netRun := &systest.Runner{}
	netRun.OnPrefix("docker network", sys.Result{})
	dep := &deploy.Deployer{
		Run:        netRun,
		DeployRoot: tmp,
		TraefikDir: filepath.Join(tmp, "traefik"),
		FS:         sys.NewOSFS(),
		Store:      store,
		Host:       hcfg,
		Catalog:    cat,
		Compose: func(inst, override, envFile string) deploy.ComposeAPI {
			return stubComp
		},
		Health: func(ctx context.Context, inst, override string) error {
			return nil
		},
		Repo: func(string) deploy.CheckoutAPI {
			return stubRepo
		},
	}

	env := &OpsEnv{
		FS:          sys.NewOSFS(),
		Run:         sys.NewExecRunner(),
		Store:       store,
		Deployer:    dep,
		Interactive: false,
		Catalog:     cat,
		DeployRoot:  tmp,
		StateRoot:   stateDir,
		Host:        hcfg,
		Resolver:    &opsFakeResolver{},
		RepoFactory: func(dir, keyPath string) project.RepoLike {
			return stubRepo
		},
	}

	return tmp, env
}

func TestGetEnvMasked(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	appEnvPath := filepath.Join(env.deployRoot(), "myapp-dev", "app.env")
	appEnvData := "SHORT=12345\nEXACT6=123456\nLONGVAL=secret123\nJP7=日本語語日本語\nJP6=日本語語日本\n"
	if err := os.WriteFile(appEnvPath, []byte(appEnvData), 0o640); err != nil {
		t.Fatal(err)
	}

	// 1. Human output: masked by default
	code, out, errb := run(t, "get-env", "myapp-dev")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "SHORT=***") {
		t.Errorf("expected SHORT=*** in out: %s", out)
	}
	if !strings.Contains(out, "EXACT6=***") {
		t.Errorf("expected EXACT6=*** in out: %s", out)
	}
	if !strings.Contains(out, "LONGVAL=se***") {
		t.Errorf("expected LONGVAL=se*** in out: %s", out)
	}
	if !strings.Contains(out, "JP7=日本***") {
		t.Errorf("expected JP7=日本*** in out: %s", out)
	}
	if !strings.Contains(out, "JP6=***") {
		t.Errorf("expected JP6=*** in out: %s", out)
	}

	// 2. JSON output: masked by default
	code, out, errb = run(t, "get-env", "myapp-dev", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Values map[string]string `json:"values"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if !res.OK {
		t.Fatalf("expected ok=true, got out=%s", out)
	}
	if res.Data.Values["SHORT"] != "***" || res.Data.Values["EXACT6"] != "***" || res.Data.Values["LONGVAL"] != "se***" || res.Data.Values["JP7"] != "日本***" || res.Data.Values["JP6"] != "***" {
		t.Fatalf("unexpected masked values in json: %+v", res.Data.Values)
	}

	// 3. Human output: --reveal
	code, out, errb = run(t, "get-env", "myapp-dev", "--reveal")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "SHORT=12345") || !strings.Contains(out, "EXACT6=123456") || !strings.Contains(out, "LONGVAL=secret123") {
		t.Errorf("expected revealed values in out: %s", out)
	}

	// 4. JSON output: --reveal
	code, out, errb = run(t, "get-env", "myapp-dev", "--reveal", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var revRes struct {
		OK   bool `json:"ok"`
		Data struct {
			Values map[string]string `json:"values"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &revRes); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if revRes.Data.Values["LONGVAL"] != "secret123" {
		t.Fatalf("expected revealed secret in json: %+v", revRes.Data.Values)
	}
}

func TestSetEnvHiddenPromptNoTTYIsUsage(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) {
		env.Interactive = false
		return env, nil
	}

	// Without value argument and non-interactive/no TTY -> usage error (exit code 2)
	code, out, errb := run(t, "set-env", "myapp-dev", "API_KEY")
	if code != 2 {
		t.Fatalf("expected exit code 2 (usage), got code=%d out=%s err=%s", code, out, errb)
	}
}

func TestStatusLocalStorageWarning(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	t.Setenv("CVCTL_STORAGE", "local")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	// Initial state
	_ = env.Store.Save(&state.State{
		Instance:  "myapp-dev",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "abcdef12345678"},
		UpdatedAt: time.Now().Add(-5 * time.Minute),
	})

	// 1. Human output: contains warning
	code, out, errb := run(t, "status")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "CVCTL_STORAGE=local") {
		t.Errorf("expected CVCTL_STORAGE=local warning in output: %s", out)
	}

	// 2. JSON output: warnings list has CVCTL_STORAGE=local
	code, out, errb = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Instances []struct {
				Instance string `json:"instance"`
				Status   string `json:"status"`
			} `json:"instances"`
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if len(res.Data.Warnings) == 0 {
		t.Fatal("expected warnings in json output, got empty")
	}
	found := false
	for _, w := range res.Data.Warnings {
		if strings.Contains(w, "CVCTL_STORAGE=local") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning mentioning CVCTL_STORAGE=local, got %+v", res.Data.Warnings)
	}
}

func TestDeployForcesFailed(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	// Current desired commit hash marked as failed
	wantDesired := state.Desired{
		InfraCommit: "abc1234567890",
		Digests: map[string]string{
			"web": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		},
	}
	st := &state.State{
		Instance:   "myapp-dev",
		Status:     state.StatusFailed,
		FailedHash: wantDesired.Hash(),
		UpdatedAt:  time.Now(),
	}
	if err := env.Store.Save(st); err != nil {
		t.Fatal(err)
	}

	// Running `cvctl deploy myapp-dev` sets Force: true, overcoming FailedHash
	code, out, errb := run(t, "deploy", "myapp-dev", "--json")
	if code != 0 {
		t.Fatalf("expected deploy to succeed with Force: true, got code %d out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Status string `json:"status"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if res.Data.Status != "OK" {
		t.Fatalf("expected status OK after forced deploy, got %+v", res.Data)
	}
}

func TestSetEnv(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	appEnvPath := filepath.Join(env.deployRoot(), "myapp-dev", "app.env")
	if err := os.WriteFile(appEnvPath, []byte("EXISTING=foo\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	// 1. Setting a key: preserves existing keys and never prints the value
	code, out, errb := run(t, "set-env", "myapp-dev", "NEW_KEY", "secret_value")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if strings.Contains(out, "secret_value") {
		t.Error("value should never be printed in output")
	}

	data, err := os.ReadFile(appEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	content := string(data)
	if !strings.Contains(content, "EXISTING=foo") || !strings.Contains(content, "NEW_KEY=secret_value") {
		t.Fatalf("expected merged keys in app.env, got:\n%s", content)
	}

	// 2. Unknown key: warns that key is not in env.spec.yml
	code, out, errb = run(t, "set-env", "myapp-dev", "NOT_IN_SPEC", "val", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var rep struct {
		OK   bool `json:"ok"`
		Data struct {
			Warnings []string `json:"warnings"`
			Next     string   `json:"next"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	found := false
	for _, w := range rep.Data.Warnings {
		if strings.Contains(w, "NOT_IN_SPEC is not in env.spec.yml") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected warning 'NOT_IN_SPEC is not in env.spec.yml', got %+v", rep.Data.Warnings)
	}

	// 3. BLOCKED state unblocked
	_ = env.Store.Save(&state.State{
		Instance:  "myapp-dev",
		Status:    state.StatusBlocked,
		Blocked:   []string{"NEEDED_KEY"},
		UpdatedAt: time.Now(),
	})
	code, out, errb = run(t, "set-env", "myapp-dev", "NEEDED_KEY", "val", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if rep.Data.Next != "next agent cycle will deploy; or run cvctl deploy myapp-dev" {
		t.Errorf("expected next message, got %q", rep.Data.Next)
	}

	// 4. Malformed app.env
	if err := os.WriteFile(appEnvPath, []byte("FOO=\"unterminated_quote"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, "set-env", "myapp-dev", "SOME_KEY", "val", "--json")
	if code == 0 {
		t.Fatalf("expected error on malformed app.env")
	}
	if !strings.Contains(out, "E_APPENV_READ") {
		t.Errorf("expected E_APPENV_READ, got %s", out)
	}

	// 5. Value with newline rejected with E_ENVSPEC_VALUE
	_ = os.WriteFile(appEnvPath, []byte("EXISTING=foo\n"), 0o640)
	code, out, _ = run(t, "set-env", "myapp-dev", "SOME_KEY", "bad\nvalue", "--json")
	if code == 0 {
		t.Fatalf("expected error on value with newline")
	}
	if !strings.Contains(out, "E_ENVSPEC_VALUE") {
		t.Errorf("expected E_ENVSPEC_VALUE, got %s", out)
	}
}

func TestSetStrategy(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	// blue-green -> E_STRATEGY_UNSUPPORTED
	code, out, errb := run(t, "set-strategy", "myapp-dev", "blue-green", "--json")
	if code == 0 {
		t.Fatalf("expected failure for blue-green, got code=%d out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "E_STRATEGY_UNSUPPORTED") {
		t.Errorf("expected E_STRATEGY_UNSUPPORTED in %s", out)
	}

	// recreate -> success and updates cvctl.env
	code, out, errb = run(t, "set-strategy", "myapp-dev", "recreate", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	cvctlEnvPath := filepath.Join(env.deployRoot(), "myapp-dev", "cvctl.env")
	data, _ := os.ReadFile(cvctlEnvPath)
	if !strings.Contains(string(data), "CVCTL_STRATEGY=recreate") {
		t.Errorf("expected CVCTL_STRATEGY=recreate in %s", string(data))
	}
}

func TestStatus(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	t.Setenv("CVCTL_STORAGE", "s3")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	_ = env.Store.Save(&state.State{
		Instance:  "myapp-dev",
		Status:    state.StatusOK,
		Current:   &state.Desired{InfraCommit: "1234567890abcdef"},
		UpdatedAt: time.Now().Add(-10 * time.Minute),
	})

	// Table output contains columns
	code, out, errb := run(t, "status")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	for _, col := range []string{"INSTANCE", "ENV", "STATUS", "COMMIT", "AGE"} {
		if !strings.Contains(out, col) {
			t.Errorf("expected column %s in %s", col, out)
		}
	}
	if !strings.Contains(out, "1234567") {
		t.Errorf("expected short commit 1234567 in %s", out)
	}

	// Filter single instance
	code, out, errb = run(t, "status", "myapp-dev", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Instances []struct {
				Instance string `json:"instance"`
				Status   string `json:"status"`
				Commit   string `json:"commit"`
				Age      string `json:"age"`
			} `json:"instances"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if len(res.Data.Instances) != 1 || res.Data.Instances[0].Instance != "myapp-dev" {
		t.Fatalf("expected single instance myapp-dev, got %+v", res.Data.Instances)
	}
}

func TestHistory(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	ev1 := state.Event{
		ID:     "ev-1",
		At:     time.Now().Add(-1 * time.Hour),
		Kind:   "deploy",
		Result: "OK",
		Detail: "initial deploy",
		Actor:  "cli",
	}
	ev2 := state.Event{
		ID:     "ev-2",
		At:     time.Now(),
		Kind:   "pause",
		Result: "PAUSED",
		Detail: "paused by operator",
		Actor:  "cli",
	}
	_ = env.Store.Append("myapp-dev", ev1)
	_ = env.Store.Append("myapp-dev", ev2)

	code, out, errb := run(t, "history", "myapp-dev", "-n", "1", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			History []state.Event `json:"history"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if len(res.Data.History) != 1 {
		t.Fatalf("expected 1 event, got %d", len(res.Data.History))
	}
	if res.Data.History[0].Kind != "pause" {
		t.Errorf("expected newest event pause, got %s", res.Data.History[0].Kind)
	}
}

func TestLogs(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	code, out, errb := run(t, "logs", "myapp-dev", "web", "--tail", "50")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "app log line 1") {
		t.Errorf("expected log output, got %s", out)
	}
}

func TestRollback(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	des1 := state.Desired{InfraCommit: "commit1111111"}
	des2 := state.Desired{InfraCommit: "commit2222222"}

	// Set state with Current and Previous
	_ = env.Store.Save(&state.State{
		Instance: "myapp-dev",
		Status:   state.StatusOK,
		Current:  &des2,
		Previous: &des1,
	})

	code, out, errb := run(t, "rollback", "myapp-dev", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	st, _ := env.Store.Load("myapp-dev")
	if st.Status != state.StatusPaused {
		t.Errorf("expected paused status after rollback, got %s", st.Status)
	}
	if st.Current.InfraCommit != "commit1111111" {
		t.Errorf("expected rollback to commit1111111, got %s", st.Current.InfraCommit)
	}
}

func TestPauseAndResume(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	_ = env.Store.Save(&state.State{
		Instance: "myapp-dev",
		Status:   state.StatusOK,
	})

	// 1. Pause
	code, out, errb := run(t, "pause", "myapp-dev", "--reason", "maintenance work", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	st, _ := env.Store.Load("myapp-dev")
	if st.Status != state.StatusPaused || st.PausedReason != "maintenance work" || st.PausedWantHash != "" {
		t.Fatalf("unexpected paused state: %+v", st)
	}
	events, _ := env.Store.History("myapp-dev", 1)
	if len(events) != 1 || events[0].Kind != "pause" || events[0].Actor != "cli" {
		t.Fatalf("unexpected history event: %+v", events)
	}

	// 2. Resume
	code, out, errb = run(t, "resume", "myapp-dev", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	st, _ = env.Store.Load("myapp-dev")
	if st.Status != state.StatusOK || st.PausedReason != "" {
		t.Fatalf("unexpected resumed state: %+v", st)
	}
	events, _ = env.Store.History("myapp-dev", 1)
	if len(events) != 1 || events[0].Kind != "resume" || events[0].Actor != "cli" {
		t.Fatalf("unexpected history event: %+v", events)
	}
}

func TestInstanceNotFound(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	commands := [][]string{
		{"get-env", "unknown-dev"},
		{"set-env", "unknown-dev", "KEY", "VAL"},
		{"set-strategy", "unknown-dev", "recreate"},
		{"history", "unknown-dev"},
		{"logs", "unknown-dev"},
		{"deploy", "unknown-dev"},
		{"rollback", "unknown-dev"},
		{"pause", "unknown-dev"},
		{"resume", "unknown-dev"},
		{"status", "unknown-dev"},
	}

	for _, cmdArgs := range commands {
		args := append(cmdArgs, "--json")
		code, out, _ := run(t, args...)
		if code == 0 {
			t.Fatalf("expected failure for %v on unknown instance", cmdArgs)
		}
		var res struct {
			OK    bool          `json:"ok"`
			Error *output.Error `json:"error"`
		}
		_ = json.Unmarshal([]byte(out), &res)
		if res.Error == nil || res.Error.Code != "E_INSTANCE_NOT_FOUND" {
			t.Errorf("for %v, expected E_INSTANCE_NOT_FOUND, got %s", cmdArgs, out)
		}
	}
}

func TestSetEnvInvalidKey(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	invalidKeys := []string{"", "A=B", "A B", "X\nY", "123KEY"}
	for _, key := range invalidKeys {
		code, out, _ := run(t, "set-env", "myapp-dev", key, "someval", "--json")
		if code != 2 {
			t.Errorf("for key %q: expected exit code 2, got %d (out: %s)", key, code, out)
		}
		var res struct {
			OK    bool          `json:"ok"`
			Error *output.Error `json:"error"`
		}
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			t.Fatalf("unmarshal json: %v out=%s", err, out)
		}
		if res.Error == nil || res.Error.Code != "E_SETENV_KEY_INVALID" {
			t.Errorf("for key %q: expected E_SETENV_KEY_INVALID, got %s", key, out)
		}
	}

	appEnvPath := filepath.Join(env.deployRoot(), "myapp-dev", "app.env")
	if _, err := os.Stat(appEnvPath); !os.IsNotExist(err) {
		t.Fatalf("expected app.env not to exist, got err: %v", err)
	}
}

type promptSpy struct {
	called bool
}

func (p *promptSpy) Ask(q hostcfg.Question, defaultValue string) (string, error) {
	p.called = true
	return "secret", nil
}

func TestSetEnvUnknownInstanceNoPrompt(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	spy := &promptSpy{}
	env.Interactive = true
	env.Prompt = spy
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	code, out, _ := run(t, "set-env", "unknown-dev", "SOME_KEY", "--json")
	if code == 0 {
		t.Fatalf("expected error for unknown instance, got code 0: %s", out)
	}
	if spy.called {
		t.Fatal("expected prompter NOT to be called when instance is unknown")
	}
	var res struct {
		OK    bool          `json:"ok"`
		Error *output.Error `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &res)
	if res.Error == nil || res.Error.Code != "E_INSTANCE_NOT_FOUND" {
		t.Fatalf("expected E_INSTANCE_NOT_FOUND, got %s", out)
	}
}

func TestResumeNotPaused(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	_ = env.Store.Save(&state.State{
		Instance: "myapp-dev",
		Status:   state.StatusOK,
	})

	code, out, _ := run(t, "resume", "myapp-dev", "--json")
	if code == 0 {
		t.Fatalf("expected error resuming OK instance, got code 0: %s", out)
	}
	var res struct {
		OK    bool          `json:"ok"`
		Error *output.Error `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &res)
	if res.Error == nil || res.Error.Code != "E_NOT_PAUSED" {
		t.Fatalf("expected E_NOT_PAUSED, got %s", out)
	}
	if !strings.Contains(res.Error.Hint, "cvctl status myapp-dev") {
		t.Fatalf("expected hint 'cvctl status myapp-dev', got %q", res.Error.Hint)
	}
	events, _ := env.Store.History("myapp-dev", 10)
	if len(events) != 0 {
		t.Fatalf("expected no events recorded on invalid resume, got %d events", len(events))
	}
}

func TestPauseDeploying(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	_ = env.Store.Save(&state.State{
		Instance: "myapp-dev",
		Status:   state.StatusDeploying,
	})

	code, out, _ := run(t, "pause", "myapp-dev", "--json")
	if code == 0 {
		t.Fatalf("expected error pausing deploying instance, got code 0: %s", out)
	}
	var res struct {
		OK    bool          `json:"ok"`
		Error *output.Error `json:"error"`
	}
	_ = json.Unmarshal([]byte(out), &res)
	if res.Error == nil || res.Error.Code != "E_DEPLOY_IN_PROGRESS" {
		t.Fatalf("expected E_DEPLOY_IN_PROGRESS, got %s", out)
	}
	st, _ := env.Store.Load("myapp-dev")
	if st.Status != state.StatusDeploying {
		t.Fatalf("expected status to remain DEPLOYING, got %s", st.Status)
	}
	events, _ := env.Store.History("myapp-dev", 10)
	if len(events) != 0 {
		t.Fatalf("expected no events recorded on refused pause, got %d events", len(events))
	}
}

func TestStatusZeroInstancesJSON(t *testing.T) {
	tmp := t.TempDir()
	stateDir := filepath.Join(tmp, "state")
	cat, _ := kinds.Load()
	store := state.NewStore(sys.NewOSFS(), stateDir)
	env := &OpsEnv{
		FS:         sys.NewOSFS(),
		Run:        sys.NewExecRunner(),
		Store:      store,
		Catalog:    cat,
		DeployRoot: filepath.Join(tmp, "deploy"),
		StateRoot:  stateDir,
	}
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	code, out, errb := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, `"instances":[]`) {
		t.Fatalf("expected JSON to contain '\"instances\":[]', got %s", out)
	}
}

func TestRollbackToIndexesOnlySuccessfulDeploys(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	a := state.Desired{InfraCommit: "commitAAAAAAA"}
	b := state.Desired{InfraCommit: "commitBBBBBBB"}
	bad := state.Desired{InfraCommit: "commitBADBAD"}
	cur := state.Desired{InfraCommit: "commitCURCUR"}
	_ = env.Store.Save(&state.State{Instance: "myapp-dev", Status: state.StatusOK, Current: &cur})
	for i, ev := range []state.Event{
		{Kind: "deploy", Result: "OK", To: &a},
		{Kind: "pause", Result: "PAUSED", To: &bad},
		{Kind: "failed", Result: "FAILED", To: &bad},
		{Kind: "rollback", Result: "FAILED", To: &bad},
		{Kind: "deploy", Result: "OK", To: &b},
	} {
		ev.ID = fmt.Sprintf("ev-%d", i)
		ev.At = time.Now()
		ev.Actor = "cli"
		if err := env.Store.Append("myapp-dev", ev); err != nil {
			t.Fatal(err)
		}
	}

	if code, out, errb := run(t, "rollback", "myapp-dev", "--to", "1", "--json"); code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	st, _ := env.Store.Load("myapp-dev")
	if st.Current.InfraCommit != "commitAAAAAAA" {
		t.Fatalf("expected rollback to A, got %s", st.Current.InfraCommit)
	}

	code, out, _ := run(t, "rollback", "myapp-dev", "--to", "2", "--json")
	if code == 0 || !strings.Contains(out, "E_ROLLBACK_TARGET_INVALID") {
		t.Fatalf("expected E_ROLLBACK_TARGET_INVALID, code %d out=%s", code, out)
	}
}

func TestRollbackFailureUsesRollbackCode(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	env.Deployer.Health = func(ctx context.Context, inst, override string) error {
		return errors.New("unhealthy")
	}
	des1 := state.Desired{InfraCommit: "commit1111111"}
	des2 := state.Desired{InfraCommit: "commit2222222"}
	_ = env.Store.Save(&state.State{Instance: "myapp-dev", Status: state.StatusOK, Current: &des2, Previous: &des1})

	code, out, _ := run(t, "rollback", "myapp-dev", "--json")
	if code == 0 || !strings.Contains(out, "E_ROLLBACK_FAILED") || strings.Contains(out, "E_DEPLOY_FAILED") {
		t.Fatalf("code %d out=%s", code, out)
	}
}

func TestStatusBackupNotConfiguredWarning(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	const msg = "backups are not configured: check CVCTL_S3_* and /etc/cvctl/secrets (run cvctl init)"

	code, out, errb := run(t, "status")
	if code != 0 || !strings.Contains(out, msg) {
		t.Fatalf("human: code=%d out=%s err=%s", code, out, errb)
	}
	code, out, errb = run(t, "status", "--json")
	if code != 0 || !strings.Contains(out, msg) {
		t.Fatalf("json: code=%d out=%s err=%s", code, out, errb)
	}

	sec := hostcfg.NewSecrets()
	for _, k := range []string{"s3_access_key", "s3_secret_key", "walg_libsodium_key"} {
		sec.Set(k, "value-"+k)
	}
	cfg := hostcfg.NewConfig()
	_ = cfg.Set("S3_ENDPOINT", "http://s3:9000")
	_ = cfg.Set("S3_BUCKET", "b")
	env.Host, env.Secrets = cfg, sec
	code, out, errb = run(t, "status", "--json")
	if code != 0 || strings.Contains(out, msg) {
		t.Fatalf("configured: code=%d out=%s err=%s", code, out, errb)
	}
}

func TestDBRestoreRequiresTo(t *testing.T) {
	var stdout, stderr strings.Builder
	code := Execute(NewRoot("test"), []string{"db", "restore", "app-prod", "db"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--to is required") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestDBRestoreHumanOutputPrintsWarnings(t *testing.T) {
	var b strings.Builder
	writeDBRestoreHuman(&b, &backup.RestoreResult{
		Instance: "app-prod", Service: "db", Target: "latest", Volume: "v2", PreviousVolume: "v1",
		Warnings: []string{"could not flush WAL from the running cluster; WAL not yet archived stays in the kept old volume v1"},
	})
	if !strings.Contains(b.String(), "warning: could not flush WAL from the running cluster") {
		t.Fatalf("output: %s", b.String())
	}
}

func TestDBRestoreHasSkipWALFlushFlag(t *testing.T) {
	var stdout, stderr strings.Builder
	code := Execute(NewRoot("test"), []string{"db", "restore", "app-prod", "db", "--skip-wal-flush"}, &stdout, &stderr)
	if code != 2 || !strings.Contains(stderr.String(), "--to is required") {
		t.Fatalf("code=%d stderr=%s", code, stderr.String())
	}
}

func TestStatusNoBackupWarningWhenLabelSaysNone(t *testing.T) {
	tmp, env := setupOpsFixture(t, "myapp-dev")
	compose := strings.Replace(cliValidCompose, "      cvctl.env: DB\n", "      cvctl.env: DB\n      cvctl.backup: none\n", 1)
	if err := os.WriteFile(filepath.Join(tmp, "myapp-dev", "infra", "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	code, out, errb := run(t, "status", "--json")
	if code != 0 || strings.Contains(out, "backups are not configured") {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
}

const statusS3Compose = `
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
      cvctl.version: "RELEASE.2026-09-16T00-00-00Z"
      cvctl.env: S3
      cvctl.expose: "9000"
      cvctl.backup: none
      cvctl.external: allowed
x-cvctl:
  health:
    service: web
    http: /healthz
  s3:
    media:
      buckets:
        products:
          public: true
        assets:
          public: true
        private:
          public: false
`

func writeStatusInstance(t *testing.T, tmp, inst, envName, mode string) {
	t.Helper()
	instDir := filepath.Join(tmp, inst)
	infraDir := filepath.Join(instDir, "infra")
	if err := os.MkdirAll(infraDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "compose.yml"), []byte(statusS3Compose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infraDir, "env.spec.yml"), []byte("vars: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	dec := instance.Decisions{
		Instance:    inst,
		Env:         envName,
		InfraBranch: "develop",
		TrackTag:    "develop",
		InfraPath:   ".",
		Strategy:    "recreate",
		Domains:     map[string]string{"web": "app.example.com", "media": "media.example.com"},
		Digests:     map[string]string{"web": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		Services: map[string]instance.ServiceDecision{
			"media": {
				Mode:    mode,
				Version: "RELEASE.2026-09-16T00-00-00Z",
				Creds: kinds.Credentials{
					User:        "media",
					Password:    "mediapassword1234567890123456",
					AppUser:     "media-app",
					AppPassword: "mediaappsecret1234567890123456",
					DB:          "media",
				},
				External: map[string]string{
					"endpoint":   "https://s3.amazonaws.com",
					"region":     "us-east-1",
					"access_key": "extkey",
					"secret_key": "extsecret",
					"bucket":     "extbucket",

					"bucket_assets": "ext-assets", "bucket_products": "ext-products", "bucket_private": "ext-private",
					"public_url_assets": "https://assets.example.com", "public_url_products": "https://products.example.com",
				},
			},
		},
	}
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), instance.FormatEnv(dec.Entries()), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestStatusWarningPublicLocalBuckets(t *testing.T) {
	tmp, env := setupOpsFixture(t, "demo-dev")
	writeStatusInstance(t, tmp, "demo-dev", "dev", "local")

	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	const want = "demo-dev: media serves public bucket(s) assets, products from local silo; use external storage in prod"

	code, out, errb := run(t, "status")
	if code != 0 || !strings.Contains(out, "warning: "+want) {
		t.Fatalf("human status: code=%d out=%s err=%s", code, out, errb)
	}

	code, out, errb = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("json status: code=%d out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if !slices.Contains(res.Data.Warnings, want) {
		t.Fatalf("expected warning %q in warnings %v", want, res.Data.Warnings)
	}

	// Verify that in prod, this warning is not generated
	writeStatusInstance(t, tmp, "demo-prod", "prod", "local")
	code, out, _ = run(t, "status", "demo-prod", "--json")
	if code != 0 {
		t.Fatalf("code=%d out=%s", code, out)
	}
	var resProd struct {
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	_ = json.Unmarshal([]byte(out), &resProd)
	for _, w := range resProd.Data.Warnings {
		if strings.Contains(w, "serves public bucket(s)") {
			t.Fatalf("unexpected public bucket warning in prod: %s", w)
		}
	}
}

func TestStatusWarningExternalS3(t *testing.T) {
	tmp, env := setupOpsFixture(t, "demo-dev")
	writeStatusInstance(t, tmp, "demo-dev", "dev", "external")

	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	const want = "demo-dev: media is external; cvctl does not back it up (use bucket versioning or replication)"

	code, out, errb := run(t, "status")
	if code != 0 || !strings.Contains(out, "warning: "+want) {
		t.Fatalf("human status: code=%d out=%s err=%s", code, out, errb)
	}

	code, out, errb = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("json status: code=%d out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if !slices.Contains(res.Data.Warnings, want) {
		t.Fatalf("expected warning %q in warnings %v", want, res.Data.Warnings)
	}
}

func TestStatusWarningOrphans(t *testing.T) {
	tmp, env := setupOpsFixture(t, "demo-dev")
	writeStatusInstance(t, tmp, "demo-dev", "dev", "local")

	instStateDir := filepath.Join(env.stateRoot(), "demo-dev")
	if err := os.MkdirAll(instStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphansJSON := `{"media": ["zebra-bucket", "alpha-bucket"]}`
	if err := os.WriteFile(filepath.Join(instStateDir, "s3-orphans.json"), []byte(orphansJSON), 0o640); err != nil {
		t.Fatal(err)
	}

	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	const want = "demo-dev: media has buckets no longer declared: alpha-bucket, zebra-bucket"

	code, out, errb := run(t, "status")
	if code != 0 || !strings.Contains(out, "warning: "+want) {
		t.Fatalf("human status: code=%d out=%s err=%s", code, out, errb)
	}

	code, out, errb = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("json status: code=%d out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if !slices.Contains(res.Data.Warnings, want) {
		t.Fatalf("expected warning %q in warnings %v", want, res.Data.Warnings)
	}
}

func TestStatusUnreadableInstanceSkipped(t *testing.T) {
	tmp, env := setupOpsFixture(t, "good-dev")
	writeStatusInstance(t, tmp, "good-dev", "dev", "external")

	// Create unreadable/corrupted instance with an orphans file that should be skipped
	badDir := filepath.Join(tmp, "bad-dev")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badDir, "cvctl.env"), []byte("INVALID MALFORMED ENV"), 0o640); err != nil {
		t.Fatal(err)
	}
	badStateDir := filepath.Join(env.stateRoot(), "bad-dev")
	if err := os.MkdirAll(badStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(badStateDir, "s3-orphans.json"), []byte(`{"media": ["bad-bucket"]}`), 0o640); err != nil {
		t.Fatal(err)
	}

	// Create instance with unreadable/corrupted contract
	badContractDir := filepath.Join(tmp, "badcontract-dev")
	if err := os.MkdirAll(badContractDir, 0o755); err != nil {
		t.Fatal(err)
	}
	decBad := instance.Decisions{Instance: "badcontract-dev", Env: "dev"}
	if err := os.WriteFile(filepath.Join(badContractDir, "cvctl.env"), instance.FormatEnv(decBad.Entries()), 0o640); err != nil {
		t.Fatal(err)
	}

	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	const goodWarn = "good-dev: media is external; cvctl does not back it up (use bucket versioning or replication)"

	// status must succeed (exit 0) and skip bad-dev without error
	code, out, errb := run(t, "status")
	if code != 0 {
		t.Fatalf("expected exit 0, got %d out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "good-dev") {
		t.Fatalf("expected output to contain good-dev, got %s", out)
	}
	if !strings.Contains(out, "warning: "+goodWarn) {
		t.Fatalf("expected warning for good-dev, got %s", out)
	}
	if strings.Contains(out, "bad-bucket") {
		t.Fatalf("expected bad-dev s3 warnings to be skipped, got %s", out)
	}

	code, out, errb = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("expected json exit 0, got %d out=%s err=%s", code, out, errb)
	}
	var res struct {
		OK   bool `json:"ok"`
		Data struct {
			Instances []instanceStatusItem `json:"instances"`
			Warnings  []string             `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal json: %v out=%s", err, out)
	}
	if !res.OK {
		t.Fatalf("expected ok=true, got %s", out)
	}
	if !slices.Contains(res.Data.Warnings, goodWarn) {
		t.Fatalf("expected good-dev warning in warnings: %v", res.Data.Warnings)
	}
	// An instance that cannot be loaded is reported, not skipped silently.
	var sawBad, sawBadContract bool
	for _, w := range res.Data.Warnings {
		if strings.Contains(w, "bad-bucket") {
			t.Fatalf("orphans of an unreadable instance must not be reported: %s", w)
		}
		sawBad = sawBad || strings.HasPrefix(w, "bad-dev: ")
		sawBadContract = sawBadContract || strings.HasPrefix(w, "badcontract-dev: ")
	}
	if !sawBad || !sawBadContract {
		t.Fatalf("expected load warnings for bad-dev and badcontract-dev: %v", res.Data.Warnings)
	}

	// Calling status directly on unreadable instances also succeeds and skips s3 warnings
	code, out, errb = run(t, "status", "bad-dev", "--json")
	if code != 0 {
		t.Fatalf("expected json exit 0 for bad-dev, got %d out=%s err=%s", code, out, errb)
	}
	code, out, errb = run(t, "status", "badcontract-dev", "--json")
	if code != 0 {
		t.Fatalf("expected json exit 0 for badcontract-dev, got %d out=%s err=%s", code, out, errb)
	}
}

func TestStatusWarnsHostS3CredentialsForBackups(t *testing.T) {
	tmp, env := setupOpsFixture(t, "demo-dev")
	writeStatusInstance(t, tmp, "demo-dev", "dev", "local")
	compose := strings.Replace(statusS3Compose, "cvctl.backup: none", "cvctl.backup: volume", 1)
	if err := os.WriteFile(filepath.Join(tmp, "demo-dev", "infra", "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	env.Host = &hostcfg.Config{Storage: "s3"}
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	const want = "demo-dev: backups use the host S3 credentials; any instance can delete them (set per-instance credentials)"
	code, out, errb := run(t, "status")
	if code != 0 || !strings.Contains(out, "warning: "+want) {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}

	envPath := filepath.Join(tmp, "demo-dev", "cvctl.env")
	raw, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("CVCTL_BACKUP_EXT_ACCESS_KEY=ownkey\nCVCTL_BACKUP_EXT_SECRET_KEY=ownsecret\n")...)
	if err := os.WriteFile(envPath, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	const keysWarn = "demo-dev: backups use the host keys; run cvctl project update demo-dev"
	if code, out, _ := run(t, "status"); code != 0 || strings.Contains(out, want) || !strings.Contains(out, "warning: "+keysWarn) {
		t.Fatalf("per-instance credentials must silence the warning: code=%d out=%s", code, out)
	}
	raw = append(raw, []byte("CVCTL_BACKUP_LIBSODIUM_KEY=abcd\nCVCTL_BACKUP_RESTIC_PASSWORD=pw\n")...)
	if err := os.WriteFile(envPath, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	if code, out, _ := run(t, "status"); code != 0 || strings.Contains(out, keysWarn) {
		t.Fatalf("instance keys must silence the keys warning: code=%d out=%s", code, out)
	}
}

func TestStatusWarnsHostKeysOnLocalStorage(t *testing.T) {
	tmp, env := setupOpsFixture(t, "demo-dev")
	writeStatusInstance(t, tmp, "demo-dev", "dev", "local")
	compose := strings.Replace(statusS3Compose, "cvctl.backup: none", "cvctl.backup: volume", 1)
	if err := os.WriteFile(filepath.Join(tmp, "demo-dev", "infra", "compose.yml"), []byte(compose), 0o644); err != nil {
		t.Fatal(err)
	}
	env.Host = &hostcfg.Config{Storage: "local"}
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	code, out, _ := run(t, "status")
	if code != 0 || !strings.Contains(out, "warning: demo-dev: backups use the host keys; run cvctl project update demo-dev") || strings.Contains(out, "host S3 credentials") {
		t.Fatalf("code=%d out=%s", code, out)
	}
}

// copyingRepo serves a fixed contract and writes it into infra/ on checkout.
type copyingRepo struct {
	*opsStubRepo
	dst string
}

func (r *copyingRepo) Checkout(_ context.Context, _ string) error {
	b, err := os.ReadFile(filepath.Join(r.infraSource, "compose.yml"))
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(r.dst, "compose.yml"), b, 0o644)
}

// A contract in infra/ that fails rules added later must not block deploying
// a fixed one.
func TestDeployRejectedOnDiskContractPicksUpFix(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	infraDir := filepath.Join(env.deployRoot(), "myapp-dev", "infra")
	good := t.TempDir()
	writeCLIInfra(t, good)
	b, err := os.ReadFile(filepath.Join(infraDir, "compose.yml"))
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(b), "services:\n", "services:\n  evil:\n    image: ghcr.io/codeavers/evil\n    privileged: true\n    labels: { cvctl.role: worker }\n", 1)
	if bad == string(b) {
		t.Fatal("fixture compose has no services: line")
	}
	if err := os.WriteFile(filepath.Join(infraDir, "compose.yml"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	repo := &copyingRepo{opsStubRepo: &opsStubRepo{infraSource: good}, dst: infraDir}
	env.RepoFactory = func(dir, keyPath string) project.RepoLike { return repo }
	env.Deployer.Repo = func(string) deploy.CheckoutAPI { return repo }

	code, out, errb := run(t, "deploy", "myapp-dev", "--json")
	if code != 0 {
		t.Fatalf("code %d out=%s err=%s", code, out, errb)
	}
	var res struct {
		Data struct {
			Status   string   `json:"status"`
			Warnings []string `json:"warnings"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v out=%s", err, out)
	}
	if res.Data.Status != "OK" || len(res.Data.Warnings) != 1 || !strings.Contains(res.Data.Warnings[0], "E_CONTRACT_FIELD_FORBIDDEN") {
		t.Fatalf("data = %+v", res.Data)
	}
	if after, _ := os.ReadFile(filepath.Join(infraDir, "compose.yml")); strings.Contains(string(after), "privileged") {
		t.Fatal("infra/ still holds the rejected contract")
	}
}
