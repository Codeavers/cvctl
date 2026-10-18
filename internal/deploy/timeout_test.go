package deploy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

// blockingCompose blocks the chosen operation until its context ends, like the
// real runner, which kills the process; systest.Runner ignores contexts.
type blockingCompose struct {
	ComposeAPI
	blockPull bool
	blockHook bool
	onBlock   func()
}

func (b *blockingCompose) Pull(ctx context.Context, services ...string) error {
	if !b.blockPull {
		return b.ComposeAPI.Pull(ctx, services...)
	}
	if b.onBlock != nil {
		b.onBlock()
	}
	<-ctx.Done()
	return ctx.Err()
}

func (b *blockingCompose) RunHook(ctx context.Context, service, script string) (sys.Result, error) {
	if !b.blockHook {
		return b.ComposeAPI.RunHook(ctx, service, script)
	}
	<-ctx.Done()
	return sys.Result{}, ctx.Err()
}

func blockWith(rig *testRig, b *blockingCompose) {
	inner := rig.deployer.Compose
	rig.deployer.Compose = func(inst, override, envFile string) ComposeAPI {
		cp := *b
		cp.ComposeAPI = inner(inst, override, envFile)
		return &cp
	}
}

func seedCurrent(t *testing.T, rig *testRig) {
	t.Helper()
	cur := state.Desired{InfraCommit: "0000000000000000000000000000000000000000", Digests: map[string]string{"api": "sha256:" + strings.Repeat("0", 64)}}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &cur}); err != nil {
		t.Fatal(err)
	}
}

func TestPullTimeoutIsTransient(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedCurrent(t, rig)
	if err := rig.deployer.Host.Set("TIMEOUT_PULL", "50ms"); err != nil {
		t.Fatal(err)
	}
	blockWith(rig, &blockingCompose{blockPull: true})

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusFailed || !strings.Contains(res.Detail, "E_TIMEOUT: pull timed out after 50ms") || !strings.Contains(res.Detail, "will retry next cycle") {
		t.Fatalf("unexpected result: %+v", res)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.FailedHash != "" {
		t.Fatalf("a timeout must stay retryable, FailedHash = %q", st.FailedHash)
	}
}

func TestHookTimeoutIsTransient(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedCurrent(t, rig)
	if err := rig.deployer.Host.Set("TIMEOUT_HOOK", "50ms"); err != nil {
		t.Fatal(err)
	}
	blockWith(rig, &blockingCompose{blockHook: true})

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusFailed || !strings.Contains(res.Detail, "E_TIMEOUT: pre-deploy hook api timed out after 50ms") {
		t.Fatalf("unexpected result: %+v", res)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.FailedHash != "" {
		t.Fatalf("a timeout must stay retryable, FailedHash = %q", st.FailedHash)
	}
}

func TestCancelledDeployIsInterruptedNotDeploying(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedCurrent(t, rig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	blockWith(rig, &blockingCompose{blockPull: true, onBlock: cancel})

	res, err := rig.deployer.Deploy(ctx, "demo-dev", defaultDesired(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected the last stable status OK, got %+v", res)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.Status != state.StatusOK || st.FailedHash != "" {
		t.Fatalf("unexpected state after cancel: %+v", st)
	}
	hist, err := rig.store.History("demo-dev", 5)
	if err != nil || len(hist) == 0 || hist[len(hist)-1].Kind != "interrupted" {
		t.Fatalf("expected an interrupted history entry, got %v (%v)", hist, err)
	}
	if rig.compose.upCount != 0 {
		t.Fatal("a cancelled deploy must not reach Up")
	}
}

func TestExecuteSwapRejectsForbiddenContract(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	curr := state.Desired{InfraCommit: "1111111111111111111111111111111111111111", Digests: map[string]string{"api": "sha256:" + strings.Repeat("1", 64)}}
	prev := state.Desired{InfraCommit: "0000000000000000000000000000000000000000", Digests: map[string]string{"api": "sha256:" + strings.Repeat("0", 64)}}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &curr, Previous: &prev}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(rig.infraDir, "compose.yml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := strings.Replace(string(raw), "services:\n", "services:\n", 1)
	bad = strings.Replace(bad, "    image:", "    privileged: true\n    image:", 1)
	if bad == string(raw) {
		t.Fatal("fixture has no service image line to patch")
	}
	if err := os.WriteFile(path, []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", nil, "cli:admin")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusFailed || !strings.HasPrefix(res.Detail, "E_DEPLOY_INVALID: E_CONTRACT_FIELD_FORBIDDEN: ") {
		t.Fatalf("expected a failed rollback naming the forbidden field, got %+v", res)
	}
	if rig.compose.upCount != 0 {
		t.Fatal("a rejected contract must not reach Up")
	}
	st, _ := rig.store.Load("demo-dev")
	if !st.RollbackFailed {
		t.Fatal("expected RollbackFailed")
	}
}

func TestWithStepTimeoutNamesStep(t *testing.T) {
	ctx, cancel := withStepTimeout(context.Background(), 10*time.Millisecond, "fetch")
	defer cancel()
	<-ctx.Done()
	te := TimeoutError(ctx)
	if te == nil || te.Code != "E_TIMEOUT" || !strings.Contains(te.Message, "fetch") {
		t.Fatalf("unexpected timeout error: %+v", te)
	}
	if TimeoutError(context.Background()) != nil {
		t.Fatal("no error expected without a deadline")
	}
}

func TestCancelDuringHealthRollsBack(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedCurrent(t, rig)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	healthCalls := 0
	rig.deployer.Health = func(hctx context.Context, inst, override string) error {
		healthCalls++
		if healthCalls == 1 {
			cancel()
			return hctx.Err()
		}
		return nil
	}

	res, err := rig.deployer.Deploy(ctx, "demo-dev", defaultDesired(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("expected OK after the rollback, got %+v", res)
	}
	if rig.compose.upCount != 2 || healthCalls != 2 {
		t.Fatalf("expected the new release and then the rollback to come up and be checked, up=%d health=%d", rig.compose.upCount, healthCalls)
	}
	cur := "0000000000000000000000000000000000000000"
	if n := len(rig.repo.checkouts); n == 0 || rig.repo.checkouts[n-1] != cur {
		t.Fatalf("expected the current commit checked out last, got %v", rig.repo.checkouts)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.Status != state.StatusOK || st.Current.InfraCommit != cur || st.Promoted || st.RollbackFailed || st.FailedHash != "" {
		t.Fatalf("inconsistent state after rollback: %+v", st)
	}
	hist, _ := rig.store.History("demo-dev", 5)
	if len(hist) == 0 || hist[len(hist)-1].Kind != "interrupted" {
		t.Fatalf("expected an interrupted history entry, got %v", hist)
	}
}

func TestRecoverPromotedDeployingForcesRedeploy(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	cur := state.Desired{InfraCommit: "0000000000000000000000000000000000000000"}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusDeploying, PriorStatus: state.StatusOK, Promoted: true, Current: &cur, FailedHash: "stale"}); err != nil {
		t.Fatal(err)
	}
	ok, err := rig.store.RecoverInterrupted("demo-dev", rig.fixedNow)
	if err != nil || !ok {
		t.Fatalf("recover: ok=%v err=%v", ok, err)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.Status != state.StatusFailed || st.FailedHash != "" || !st.RollbackFailed || st.Promoted {
		t.Fatalf("a promoted crash must end FAILED with an empty FailedHash: %+v", st)
	}
}

func TestInterruptKeepsPriorStatus(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	cur := state.Desired{InfraCommit: "0000000000000000000000000000000000000000"}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusDeploying, PriorStatus: state.StatusFailed, Current: &cur}); err != nil {
		t.Fatal(err)
	}
	if _, err := rig.store.RecoverInterrupted("demo-dev", rig.fixedNow); err != nil {
		t.Fatal(err)
	}
	st, _ := rig.store.Load("demo-dev")
	if st.Status != state.StatusFailed {
		t.Fatalf("expected the prior FAILED status, got %s", st.Status)
	}
}

func TestWaitTimeout(t *testing.T) {
	p := &contract.Project{}
	if got := waitTimeout(p); got != 150*time.Second {
		t.Fatalf("default = %s, want 150s", got)
	}
	p.X.Health.Timeout = 90 * time.Second
	if got := waitTimeout(p); got != 210*time.Second {
		t.Fatalf("= %s, want 210s", got)
	}
	if got := rollbackCap(&contract.Project{}); got != 5*time.Minute {
		t.Fatalf("rollback cap = %s, want 5m", got)
	}
	p.X.Health.Timeout = 10 * time.Minute
	if got := rollbackCap(p); got != 12*time.Minute {
		t.Fatalf("rollback cap = %s, want 12m", got)
	}
}
