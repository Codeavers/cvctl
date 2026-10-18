package backup_test

import (
	"context"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
)

const (
	stLs     = "docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/walg-probe-db.env --entrypoint wal-g ghcr.io/codeavers/cvctl-postgres:16 st ls"
	lsHeader = "type size last modified                  name\n"
)

func lsDir(name string) string {
	return "dir  0    0001-01-01 00:00:00 +0000 UTC     " + name + "  \n"
}

func lsObj(name string) string {
	return "obj  812  2026-01-01 10:00:00 +0000 UTC     " + name + "  \n"
}

func sentinel(n int) string {
	return lsObj("base_00000001000000000000000" + string(rune('0'+n)) + "_backup_stop_sentinel.json")
}

func TestBucketGenerationsHighestUsedAndWithBase(t *testing.T) {
	g := verifyRig(t, "")
	g.run.On(stLs, sys.Result{Stdout: []byte(lsHeader + lsDir("basebackups_005/") + lsDir("g1/") + lsDir("g3/") + lsDir("g12/") + lsDir("wal_005/") + lsObj("stray.json"))})
	// g12 holds WAL only (its base backup failed), g3 has a base backup
	g.run.On(stLs+" g12/basebackups_005", sys.Result{Stdout: []byte(lsHeader)})
	g.run.On(stLs+" g3/basebackups_005", sys.Result{Stdout: []byte(lsHeader + lsDir("base_000000020000000000000004/") + sentinel(4))})
	used, withBase, err := g.r.BucketGenerations(context.Background(), "app-prod", "db")
	if err != nil {
		t.Fatal(err)
	}
	if used != 12 || withBase != 3 {
		t.Fatalf("used=%d withBase=%d", used, withBase)
	}
	if g.has(stLs+" g1/") != 0 {
		t.Fatalf("must stop at the highest generation with a base backup: %v", g.calls())
	}
	env, err := g.fs.ReadFile("/srv/deploy/app-prod/walg-probe-db.env")
	if err == nil {
		t.Fatalf("probe env file must be removed after use: %s", env)
	}
	for _, c := range g.run.Calls {
		if strings.Contains(c.String(), secretKey) || strings.Contains(c.String(), libsodium) {
			t.Fatalf("secret in argv: %s", c.String())
		}
	}
}

func TestBucketGenerationsLegacyOnly(t *testing.T) {
	g := verifyRig(t, "")
	g.run.On(stLs, sys.Result{Stdout: []byte(lsHeader + lsDir("basebackups_005/") + lsDir("wal_005/"))})
	g.run.On(stLs+" basebackups_005", sys.Result{Stdout: []byte(lsHeader + sentinel(2))})
	used, withBase, err := g.r.BucketGenerations(context.Background(), "app-prod", "db")
	if err != nil || used != 0 || withBase != 0 {
		t.Fatalf("used=%d withBase=%d err=%v", used, withBase, err)
	}
}

func TestBucketGenerationsEmptyBucket(t *testing.T) {
	g := verifyRig(t, "")
	g.run.On(stLs, sys.Result{Stdout: []byte(lsHeader)})
	used, withBase, err := g.r.BucketGenerations(context.Background(), "app-prod", "db")
	if err != nil || used != -1 || withBase != -1 {
		t.Fatalf("used=%d withBase=%d err=%v", used, withBase, err)
	}
}

func TestBucketGenerationsListFailureIsError(t *testing.T) {
	g := verifyRig(t, "")
	g.run.On(stLs, sys.Result{ExitCode: 1, Stderr: []byte("access denied " + secretKey)})
	_, _, err := g.r.BucketGenerations(context.Background(), "app-prod", "db")
	if err == nil || !strings.Contains(err.Error(), "E_BACKUP_FAILED") || strings.Contains(err.Error(), secretKey) {
		t.Fatalf("err=%v", err)
	}
}

func TestRestoreNewGenerationIsAboveTheBucket(t *testing.T) {
	g := newRestoreRig(t)
	g.fs.WriteFile(envPath, []byte("CVCTL_INSTANCE=app-prod\nCVCTL_SVC_DB_BACKUP_GEN=2\nCVCTL_SVC_DB_BACKUP_GEN_MAX=3\n"), 0o640, sys.Owner{})
	// another host (or a restore made from a stale bundle) already used g7
	g.run.On(stLs, sys.Result{Stdout: []byte(lsHeader + lsDir("g2/") + lsDir("g7/"))})
	g.run.On(stLs+" g7/basebackups_005", sys.Result{Stdout: []byte(lsHeader + sentinel(3))})
	res, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.envAt[0], "CVCTL_SVC_DB_BACKUP_GEN=8\n") || !strings.Contains(g.envAt[0], "CVCTL_SVC_DB_BACKUP_GEN_MAX=8\n") {
		t.Fatalf("env: %s", g.envAt[0])
	}
	if g.index(stLs) > g.index(cp+"stop") {
		t.Fatalf("the bucket must be read before anything is stopped: %v", g.calls())
	}
	found := false
	for _, w := range res.Warnings {
		found = found || strings.Contains(w, "re-run cvctl secrets export")
	}
	notified := false
	for _, m := range g.note.msgs {
		notified = notified || (m.Level == "warn" && strings.Contains(m.Text, "re-run cvctl secrets export"))
	}
	if !found || !notified {
		t.Fatalf("warnings=%v notes=%+v", res.Warnings, g.note.msgs)
	}
}

func TestRestoreBucketListFailureStopsBeforeTouchingAnything(t *testing.T) {
	g := newRestoreRig(t)
	g.run.On(stLs, sys.Result{ExitCode: 1, Stderr: []byte("no route")})
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err == nil {
		t.Fatal("expected failure")
	}
	if g.index(cp+"stop") >= 0 || g.applies != 0 {
		t.Fatalf("nothing may be stopped: %v", g.calls())
	}
}
