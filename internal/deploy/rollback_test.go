package deploy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
)

const (
	rbOldCommit = "0000000000000000000000000000000000000000"
	rbCurCommit = "1111111111111111111111111111111111111111"
	rbOldDigest = "sha256:" + "00000000000000000000000000000000" + "00000000000000000000000000000000"
	rbCurDigest = "sha256:" + "00000000000000000000000000000000" + "00000000000000000000000000000001"
	rbImageRef  = "ghcr.io/org/project-api@" + rbOldDigest
)

// seedRollback leaves demo-dev running a current version, with the files a
// deploy of it would have written.
func seedRollback(t *testing.T, rig *testRig) (target state.Desired, files []string) {
	t.Helper()
	target = state.Desired{InfraCommit: rbOldCommit, Digests: map[string]string{"api": rbOldDigest}}
	cur := state.Desired{InfraCommit: rbCurCommit, Digests: map[string]string{"api": rbCurDigest}}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &cur, Previous: &target}); err != nil {
		t.Fatal(err)
	}
	if err := rig.store.Append("demo-dev", state.Event{Kind: "deploy", Result: "OK", From: &target, To: &cur}); err != nil {
		t.Fatal(err)
	}
	files = []string{
		filepath.Join(rig.instDir, "compose.cvctl.yml"),
		filepath.Join(rig.instDir, "app.env"),
		filepath.Join(rig.deployer.TraefikDir, "demo-dev.yml"),
	}
	for _, f := range files {
		if err := os.WriteFile(f, []byte("A=running version\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	return target, append(files,
		filepath.Join(rig.store.Root(), "demo-dev", "state.json"),
		filepath.Join(rig.store.Root(), "demo-dev", "history.jsonl"))
}

func snapshot(t *testing.T, files []string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		out[f] = string(b)
	}
	return out
}

func TestRollbackPullFailureChangesNothing(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	target, files := seedRollback(t, rig)
	before := snapshot(t, files)
	rig.compose.failEnsure = map[string]error{rbImageRef: errors.New("manifest unknown")}

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", &target, "cli:admin")

	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_ROLLBACK_IMAGE" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, want := range []string{rbImageRef, "removed from the registry", "manifest unknown"} {
		if !strings.Contains(oe.Message, want) {
			t.Errorf("message lacks %q: %s", want, oe.Message)
		}
	}
	for f, b := range snapshot(t, files) {
		if b != before[f] {
			t.Errorf("%s changed:\n%s", f, b)
		}
	}
	if rig.compose.upCount != 0 {
		t.Errorf("Up ran %d times", rig.compose.upCount)
	}
	if want := []string{rbOldCommit, rbCurCommit}; !slices.Equal(rig.repo.checkouts, want) {
		t.Errorf("checkouts = %v, want the target then current again", rig.repo.checkouts)
	}
	if len(rig.notifier.messages) != 0 {
		t.Errorf("notified: %+v", rig.notifier.messages)
	}
}

func TestRollbackPullFailureRedactsSecrets(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	target, _ := seedRollback(t, rig)
	if err := os.WriteFile(filepath.Join(rig.instDir, "app.env"), []byte("TOKEN=s3cr3t-token-value\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	rig.compose.failEnsure = map[string]error{rbImageRef: errors.New("denied for s3cr3t-token-value")}

	_, err := rig.deployer.Rollback(context.Background(), "demo-dev", &target, "cli:admin")
	if err == nil || strings.Contains(err.Error(), "s3cr3t-token-value") {
		t.Fatalf("err = %v", err)
	}
}

// With infra/ stuck on the target revision the instance no longer matches its
// state, which only the failure path records.
func TestRollbackPullFailureWithFailedRestoreIsRecorded(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	target, _ := seedRollback(t, rig)
	rig.compose.failEnsure = map[string]error{rbImageRef: errors.New("manifest unknown")}
	rig.repo.failAt[rbCurCommit] = errors.New("git broke")

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", &target, "cli:admin")
	if err != nil || res.Status != state.StatusFailed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != state.StatusFailed || !st.RollbackFailed {
		t.Fatalf("state: %+v", st)
	}
}

func TestRollbackEnsuresImagesBeforeWriting(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	target, _ := seedRollback(t, rig)

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", &target, "cli:admin")
	if err != nil || res.Status != state.StatusPaused {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	ensure := slices.Index(rig.compose.calls, "ensure:"+rbImageRef)
	up := slices.Index(rig.compose.calls, "up")
	if ensure < 0 || up < ensure {
		t.Fatalf("calls: %v", rig.compose.calls)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != state.StatusPaused || st.Current.InfraCommit != rbOldCommit || st.Previous.InfraCommit != rbCurCommit {
		t.Fatalf("state: %+v", st)
	}
	override, err := os.ReadFile(filepath.Join(rig.instDir, "compose.cvctl.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(override), rbImageRef) {
		t.Fatalf("override not rendered:\n%s", override)
	}
}

func TestRollbackTargetWithoutDigestsPullsNothing(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	target, _ := seedRollback(t, rig)
	target.Digests = nil

	res, err := rig.deployer.Rollback(context.Background(), "demo-dev", &target, "cli:admin")
	if err != nil || res.Status != state.StatusPaused {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if got := ensureCalls(rig); len(got) != 0 {
		t.Fatalf("pulled %v", got)
	}
}

func ensureCalls(rig *testRig) []string {
	var out []string
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "ensure:") {
			out = append(out, c)
		}
	}
	return out
}

// The automatic rollback returns to what was running, whose image is on disk;
// a registry outage must not stop it.
func TestAutoRollbackPullsNothing(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedRollback(t, rig)
	rig.deployer.Health = func(context.Context, string, string) error {
		if rig.compose.upCount == 1 {
			return fmt.Errorf("unhealthy")
		}
		return nil
	}

	want := state.Desired{InfraCommit: strings.Repeat("2", 40), Digests: map[string]string{"api": rbOldDigest}}
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(rig.repo.checkouts, rbCurCommit) || rig.compose.upCount < 2 {
		t.Fatalf("no rollback ran: res=%+v checkouts=%v ups=%d", res, rig.repo.checkouts, rig.compose.upCount)
	}
	if got := ensureCalls(rig); len(got) != 0 {
		t.Fatalf("pulled %v", got)
	}
}
