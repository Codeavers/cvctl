package backup_test

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

const verifyExec = "docker exec cvctl-verify-app-prod-db-1767323045-0a0b0c0d psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c "

func TestVerifySQLMustReturnATruthyRow(t *testing.T) {
	for out, ok := range map[string]bool{
		"":            false,
		"0\n":         false,
		"f\n":         false,
		"false\n":     false,
		"NULL\n":      false,
		"\n":          false,
		"12\n":        true,
		"t\n":         true,
		"5|alice\n":   true,
		"0|x\n7|y\n":  false,
		"yes|0\n0\n":  true,
		" 3 \n":       true,
		"False|7\n":   false,
		"something\n": true,
	} {
		g := verifyRig(t, "select count(*) from users")
		g.run.On(verifyExec+"select count(*) from users", sys.Result{Stdout: []byte(out)})
		res, err := g.r.Verify(context.Background(), "app-prod", "db")
		if ok && err != nil {
			t.Fatalf("%q: %v", out, err)
		}
		if !ok && (err == nil || !strings.Contains(res[0].Error, "x-cvctl.verify.sql")) {
			t.Fatalf("%q: res=%+v err=%v", out, res, err)
		}
	}
}

func TestTempNamesCarryARandomSuffix(t *testing.T) {
	g := verifyRig(t, "")
	g.r.Suffix = nil
	var names []string
	for range 2 {
		g.run.Calls = nil
		_, _ = g.r.Verify(context.Background(), "app-prod", "db")
		names = append(names, strings.TrimPrefix(g.calls()[1], "docker volume create "))
	}
	re := regexp.MustCompile(`^cvctl-verify-app-prod-db-1767323045-[0-9a-f]{8}$`)
	if !re.MatchString(names[0]) || !re.MatchString(names[1]) || names[0] == names[1] {
		t.Fatalf("names: %v", names)
	}
}

func TestVerifyWaitsAsLongAsARestore(t *testing.T) {
	g := verifyRig(t, "")
	g.run.OnPrefix("docker exec cvctl-verify-app-prod-db-1767323045-0a0b0c0d psql -U app -d appdb -tA -c select pg_is_in_recovery()", sys.Result{Stdout: []byte("t\n")})
	var waited time.Duration
	g.r.Sleep = func(d time.Duration) { waited += d }
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err == nil || !strings.Contains(res[0].Error, "2h0m0s") || waited < 2*time.Hour {
		t.Fatalf("waited=%v res=%+v err=%v", waited, res, err)
	}
}

func TestRestoreHealthFailureHintsAtRollback(t *testing.T) {
	g := newRestoreRig(t)
	g.applyErrs = []error{errors.New("health check failed"), nil}
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || !strings.Contains(oe.Hint, "if the restore target predates a migration, roll the app back first (cvctl rollback)") {
		t.Fatalf("err=%v", err)
	}
}

func TestRestoreFailedRollbackIsRecordedAndBlocksCleanup(t *testing.T) {
	g := newRestoreRig(t)
	g.applyErrs = []error{errors.New("boom"), errors.New("still broken")}
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err == nil {
		t.Fatal("expected failure")
	}
	hist, _ := g.store.History("app-prod", 1)
	if len(hist) != 1 || hist[0].Result != "rollback_failed" {
		t.Fatalf("history: %+v", hist)
	}
	g.run.On("docker volume ls -q", sys.Result{Stdout: []byte("app-prod_db-data\n")})
	_, err := g.r.Cleanup(context.Background(), "app-prod", true)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_CLEANUP_UNSAFE" || oe.Hint == "" {
		t.Fatalf("err=%v", err)
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("removed a volume: %v", g.calls())
	}
	// a later successful restore clears the block
	if err := g.store.Append("app-prod", state.Event{At: time.Now(), ID: "restore-2", Kind: "restore", Result: "ok", Actor: "cli"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.r.Cleanup(context.Background(), "app-prod", false); err != nil {
		t.Fatalf("after a good restore: %v", err)
	}
}
