package backup_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

func verifyRig(t *testing.T, verifySQL string) *rig {
	t.Helper()
	g := newRig(t)
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	g.r.Catalog = cat
	p, d, _ := g.r.Load("app-prod")
	if verifySQL != "" {
		p.X.Verify = &contract.VerifySpec{Service: "db", SQL: verifySQL}
	}
	sd := d.Services["db"]
	sd.Version = "16"
	sd.Creds = kinds.Credentials{User: "app", Password: "pw-SECRET-999", DB: "appdb"}
	d.Services["db"] = sd
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }
	g.r.Sleep = func(_ time.Duration) {}
	g.okAll()
	g.run.OnPrefix("docker inspect", sys.Result{Stdout: []byte("true\n")})
	g.run.OnPrefix("docker exec cvctl-verify-app-prod-db-1767323045-0a0b0c0d psql -U app -d appdb -tA -c select pg_is_in_recovery()", sys.Result{Stdout: []byte("f\n")})
	g.run.OnPrefix("docker exec cvctl-verify-app-prod-db-1767323045-0a0b0c0d psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c select count", sys.Result{Stdout: []byte("3\n")})
	return g
}

func (g *rig) has(prefix string) int {
	n := 0
	for _, c := range g.calls() {
		if strings.HasPrefix(c, prefix) {
			n++
		}
	}
	return n
}

func TestVerifyWalgSequenceAndCleanup(t *testing.T) {
	g := verifyRig(t, "")
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err != nil || len(res) != 1 || !res[0].OK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	name := "cvctl-verify-app-prod-db-1767323045-0a0b0c0d"
	calls := g.calls()
	if calls[0] != "docker network inspect app-prod_backup" || calls[1] != "docker volume create "+name {
		t.Fatalf("calls: %v", calls)
	}
	run := calls[2]
	for _, want := range []string{
		"docker run -d --name " + name + " --network app-prod_backup --env-file /srv/deploy/app-prod/backup-db.env",
		"-v " + name + ":/var/lib/postgresql/data", "-e PGDATA=/var/lib/postgresql/data", "-e PGUSER=app", "-e PGDATABASE=appdb",
		"--entrypoint sh ghcr.io/codeavers/cvctl-postgres:16 -c ", "gosu postgres wal-g backup-fetch", "recovery.signal", "exec gosu postgres postgres",
	} {
		if !strings.Contains(run, want) {
			t.Fatalf("run missing %q:\n%s", want, run)
		}
	}
	n := len(calls)
	if calls[n-2] != "docker rm -f "+name || calls[n-1] != "docker volume rm -f "+name {
		t.Fatalf("cleanup: %v", calls[n-2:])
	}
	for _, c := range g.run.Calls {
		if strings.Contains(c.String(), "pw-SECRET-999") || strings.Contains(c.String(), secretKey) {
			t.Fatalf("secret in argv: %s", c.String())
		}
	}
	st, _ := g.store.Load("app-prod")
	if st.Backups["db"].LastVerifyOK.IsZero() || st.Backups["db"].LastVerifyError != "" {
		t.Fatalf("status: %+v", st.Backups["db"])
	}
}

func TestVerifyZeroTablesFailsEmptyAndCleans(t *testing.T) {
	g := verifyRig(t, "")
	g.run.OnPrefix("docker exec cvctl-verify-app-prod-db-1767323045-0a0b0c0d psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c select count", sys.Result{Stdout: []byte("0\n")})
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err == nil || res[0].OK || !strings.Contains(res[0].Error, "no tables") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if g.has("docker rm -f") != 1 || g.has("docker volume rm -f") != 1 {
		t.Fatalf("cleanup missing: %v", g.calls())
	}
	st, _ := g.store.Load("app-prod")
	if st.Backups["db"].LastVerifyError == "" || len(g.note.msgs) != 1 || g.note.msgs[0].Level != "error" {
		t.Fatalf("status/notify: %+v %v", st.Backups["db"], g.note.msgs)
	}
	// Same failure again must not page again.
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err == nil {
		t.Fatal("expected failure")
	}
	if len(g.note.msgs) != 1 {
		t.Fatalf("notification not deduped: %d", len(g.note.msgs))
	}
}

func TestVerifyRunsContractSQL(t *testing.T) {
	g := verifyRig(t, "select 1 from users")
	g.run.On(verifyExec+"select 1 from users", sys.Result{Stdout: []byte("1\n")})
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if g.has("docker exec cvctl-verify-app-prod-db-1767323045-0a0b0c0d psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c select 1 from users") != 1 {
		t.Fatalf("verify sql missing: %v", g.calls())
	}
}

func TestVerifyContainerExitCleansAndFails(t *testing.T) {
	g := verifyRig(t, "")
	g.run.OnPrefix("docker inspect", sys.Result{Stdout: []byte("false\n")})
	g.run.OnPrefix("docker logs", sys.Result{Stderr: []byte("wal-g: no backups found " + secretKey)})
	res, err := g.r.Verify(context.Background(), "app-prod", "db")
	if err == nil || res[0].OK || strings.Contains(res[0].Error, secretKey) {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if g.has("docker rm -f") != 1 || g.has("docker volume rm -f") != 1 {
		t.Fatalf("cleanup: %v", g.calls())
	}
}

func TestVerifyCleansOnCancel(t *testing.T) {
	g := verifyRig(t, "")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := g.r.Verify(ctx, "app-prod", "db"); err == nil {
		t.Fatal("expected failure")
	}
	if g.has("docker rm -f") != 1 || g.has("docker volume rm -f") != 1 {
		t.Fatalf("cleanup: %v", g.calls())
	}
}

func TestVerifyResticEmptyFails(t *testing.T) {
	g := verifyRig(t, "")
	g.run.OnPrefix("docker run --name", sys.Result{Stdout: []byte("0\n")})
	res, err := g.r.Verify(context.Background(), "app-prod", "files")
	if err == nil || res[0].OK || !strings.Contains(res[0].Error, "no files") {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if g.has("docker rm -f") != 1 {
		t.Fatalf("cleanup: %v", g.calls())
	}
	g.run.OnPrefix("docker run --name", sys.Result{Stdout: []byte("42\n")})
	if _, err := g.r.Verify(context.Background(), "app-prod", "files"); err != nil {
		t.Fatal(err)
	}
}

func TestVerifyContainerDisablesArchiving(t *testing.T) {
	g := verifyRig(t, "")
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if run := g.calls()[2]; !strings.Contains(run, "archive_mode=off") {
		t.Fatalf("verify script must disable archiving:\n%s", run)
	}
}
