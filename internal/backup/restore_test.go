package backup_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

const cpBase = "docker compose -p app-prod --env-file /srv/deploy/app-prod/app.env -f /srv/deploy/app-prod/compose.cvctl.yml"
const cp = cpBase + " "

const probe = "docker ps -a --filter label=com.docker.compose.project=app-prod --filter label=com.docker.compose.service=db --format {{.State}}"

const (
	newVol  = "app-prod_db-data-r1767323045"
	recName = "cvctl-restore-app-prod-db-1767323045-0a0b0c0d"
	envPath = "/srv/deploy/app-prod/cvctl.env"
)

type restoreRig struct {
	*rig
	applies   int
	applyErrs []error
	envAt     []string
}

func newRestoreRig(t *testing.T) *restoreRig {
	t.Helper()
	g := verifyRig(t, "")
	rr := &restoreRig{rig: g}
	if err := g.fs.WriteFile(envPath, []byte("CVCTL_INSTANCE=app-prod\nCVCTL_SVC_DB_MODE=local\nCVCTL_SVC_DB_PASSWORD=pw-SECRET-999\n"), 0o640, sys.Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := g.store.Save(&state.State{Instance: "app-prod", Status: state.StatusOK, Current: &state.Desired{InfraCommit: "abc"}, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	g.r.Apply = func(_ context.Context, _ string, target *state.Desired) error {
		if target == nil || target.InfraCommit != "abc" {
			t.Fatalf("apply target: %+v", target)
		}
		raw, _ := g.fs.ReadFile(envPath)
		rr.envAt = append(rr.envAt, string(raw))
		g.run.Calls = append(g.run.Calls, sys.Cmd{Name: "APPLY"})
		rr.applies++
		if len(rr.applyErrs) >= rr.applies && rr.applyErrs[rr.applies-1] != nil {
			return rr.applyErrs[rr.applies-1]
		}
		return nil
	}
	g.r.Sleep = func(time.Duration) {}
	pq := cp + "exec -T -u postgres db psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c "
	pg := cp + "exec -T -u postgres db psql -U app -d postgres -v ON_ERROR_STOP=1 -tA -c "
	g.run.On(probe, sys.Result{Stdout: []byte("running\n")})
	g.run.OnPrefix(pg+"select pg_walfile_name", sys.Result{Stdout: []byte("000000010000000000000005\n")})
	g.run.OnPrefix(pg+"select coalesce", sys.Result{Stdout: []byte("000000010000000000000005\n")})
	g.run.OnPrefix(pq+"select 1", sys.Result{Stdout: []byte("1\n")})
	g.run.On("docker volume inspect "+newVol, sys.Result{ExitCode: 1})
	g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("true\n")})
	g.run.OnPrefix("docker exec "+recName+" psql -U app -d appdb -tA -c select pg_is_in_recovery()", sys.Result{Stdout: []byte("f\n")})
	return rr
}

func (g *rig) index(prefix string) int {
	for i, c := range g.calls() {
		if strings.HasPrefix(c, prefix) {
			return i
		}
	}
	return -1
}

func TestRestoreHappyPathOrder(t *testing.T) {
	g := newRestoreRig(t)
	res, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	if err != nil {
		t.Fatal(err)
	}
	if res.Volume != newVol || res.PreviousVolume != "app-prod_db-data" {
		t.Fatalf("res: %+v", res)
	}
	order := []string{
		"docker volume inspect " + newVol,
		cp + "stop db",
		"docker volume create " + newVol,
		"docker run -d --name " + recName,
		"docker stop -t 120 " + recName,
		"docker rm -f " + recName,
		"APPLY",
		cp + "up -d --wait --wait-timeout 150 db",
		cp + "exec -T -u postgres db pg_isready -U app -d appdb",
		cp + "exec -T -u postgres db psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c select 1",
		cp + "exec -T -u postgres db wal-g backup-push /var/lib/postgresql/data",
	}
	last := -1
	for _, want := range order {
		i := g.index(want)
		if i <= last {
			t.Fatalf("%q out of order (%d <= %d): %v", want, i, last, g.calls())
		}
		last = i
	}
	run := g.calls()[g.index("docker run -d")]
	for _, want := range []string{"-v " + newVol + ":/var/lib/postgresql/data", "-e RESTORE_BACKUP=LATEST", "-c recovery_target_action=promote", `-c "restore_command=wal-g wal-fetch %f %p"`} {
		if !strings.Contains(run, want) {
			t.Fatalf("run missing %q:\n%s", want, run)
		}
	}
	if strings.Contains(run, "RESTORE_TARGET_TIME=") || strings.Contains(run, "RESTORE_TARGET_NAME=") {
		t.Fatalf("latest must set no target: %s", run)
	}
	if !strings.Contains(g.envAt[0], "CVCTL_SVC_DB_VOLUME="+newVol) || !strings.Contains(g.envAt[0], "CVCTL_SVC_DB_PASSWORD=pw-SECRET-999") {
		t.Fatalf("env at apply: %s", g.envAt[0])
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("restore must never delete a volume: %v", g.calls())
	}
	hist, _ := g.store.History("app-prod", 5)
	if len(hist) != 1 || hist[0].Kind != "restore" || hist[0].Result != "ok" || hist[0].Actor != "cli" {
		t.Fatalf("history: %+v", hist)
	}
	if len(g.note.msgs) != 2 || g.note.msgs[0].Level != "info" || !strings.Contains(g.note.msgs[0].Text, "app-prod_db-data") ||
		g.note.msgs[1].Level != "warn" || !strings.Contains(g.note.msgs[1].Text, "re-run cvctl secrets export") {
		t.Fatalf("notify: %+v", g.note.msgs)
	}
	for _, c := range g.run.Calls {
		if strings.Contains(c.String(), "pw-SECRET-999") || strings.Contains(c.String(), secretKey) || strings.Contains(c.String(), libsodium) {
			t.Fatalf("secret in argv: %s", c.String())
		}
	}
}

func TestRestoreFailureAfterSwitchRollsBackAndKeepsVolume(t *testing.T) {
	g := newRestoreRig(t)
	g.applyErrs = []error{errors.New("health check failed " + secretKey), nil}
	res, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if res != nil || !errors.As(err, &oe) || oe.Code != "E_RESTORE_FAILED" {
		t.Fatalf("res=%v err=%v", res, err)
	}
	if !strings.Contains(oe.Message, newVol) || !strings.Contains(oe.Message, "app-prod_db-data") || strings.Contains(oe.Message, secretKey) {
		t.Fatalf("message: %s", oe.Message)
	}
	if g.applies != 2 {
		t.Fatalf("applies=%d", g.applies)
	}
	if strings.Contains(g.envAt[1], "CVCTL_SVC_DB_VOLUME") || !strings.Contains(g.envAt[1], "CVCTL_SVC_DB_PASSWORD=pw-SECRET-999") {
		t.Fatalf("rolled-back env: %s", g.envAt[1])
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("volume deleted: %v", g.calls())
	}
	if len(g.note.msgs) != 1 || g.note.msgs[0].Level != "error" || strings.Contains(g.note.msgs[0].Text, secretKey) {
		t.Fatalf("notify: %+v", g.note.msgs)
	}
	hist, _ := g.store.History("app-prod", 5)
	if len(hist) != 1 || hist[0].Result != "restore_failed" {
		t.Fatalf("history: %+v", hist)
	}
}

func TestRestoreRollbackRestoresExplicitPreviousVolume(t *testing.T) {
	g := newRestoreRig(t)
	g.fs.WriteFile(envPath, []byte("CVCTL_INSTANCE=app-prod\nCVCTL_SVC_DB_VOLUME=custom_vol\n"), 0o640, sys.Owner{})
	g.applyErrs = []error{errors.New("boom"), nil}
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(g.envAt[1], "CVCTL_SVC_DB_VOLUME=custom_vol") {
		t.Fatalf("env: %s", g.envAt[1])
	}
}

func TestRestoreFailureBeforeSwitchRestartsOldVolume(t *testing.T) {
	g := newRestoreRig(t)
	g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("false\n")})
	g.run.OnPrefix("docker logs", sys.Result{Stderr: []byte("wal-g: no such backup")})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_FAILED" {
		t.Fatalf("err=%v", err)
	}
	if g.applies != 1 || strings.Contains(g.envAt[0], "CVCTL_SVC_DB_VOLUME") {
		t.Fatalf("applies=%d env=%v", g.applies, g.envAt)
	}
}

func TestRestoreFutureTimestampRejectedWithoutTouchingAnything(t *testing.T) {
	g := newRestoreRig(t)
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "2026-01-02T04:00:00Z")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_TARGET_INVALID" {
		t.Fatalf("err=%v", err)
	}
	if len(g.run.Calls) != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestRestoreDeployIDUsesRecoveryTargetName(t *testing.T) {
	g := newRestoreRig(t)
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "deploy-42"); err != nil {
		t.Fatal(err)
	}
	run := g.calls()[g.index("docker run -d")]
	if !strings.Contains(run, "-e RESTORE_TARGET_NAME=deploy-42") || !strings.Contains(run, `-c "recovery_target_name=$RESTORE_TARGET_NAME"`) || !strings.Contains(run, "-c recovery_target_inclusive=true") {
		t.Fatalf("run:\n%s", run)
	}
	if g.index(cp+"exec -T -u postgres db wal-g backup-list") >= 0 {
		t.Fatalf("an unbounded id must use LATEST without listing: %v", g.calls())
	}
}

func TestRestoreRejectsBadTargets(t *testing.T) {
	for _, to := range []string{"deploy-1;drop", "deploy-", "yesterday", "base_1", ""} {
		g := newRestoreRig(t)
		_, err := g.r.Restore(context.Background(), "app-prod", "db", to)
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Code != "E_RESTORE_TARGET_INVALID" || len(g.run.Calls) != 0 {
			t.Fatalf("to=%q err=%v calls=%v", to, err, g.calls())
		}
	}
}

func TestRestoreTimestampPicksNewestBackupBeforeTarget(t *testing.T) {
	g := newRestoreRig(t)
	list := `[{"backup_name":"base_0000000100000000000000A1","time":"2026-01-01T01:00:00.123Z"},` +
		`{"backup_name":"base_0000000100000000000000B2","time":"2026-01-01T09:00:00Z"},` +
		`{"backup_name":"base_0000000100000000000000C3","time":"2026-01-02T01:00:00Z"}]`
	g.run.OnPrefix(cp+"exec -T -u postgres db wal-g backup-list", sys.Result{Stdout: []byte(list)})
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "2026-01-01T12:00:00Z"); err != nil {
		t.Fatal(err)
	}
	run := g.calls()[g.index("docker run -d")]
	if !strings.Contains(run, "-e RESTORE_BACKUP=base_0000000100000000000000B2") || !strings.Contains(run, "-e RESTORE_TARGET_TIME=2026-01-01 12:00:00+00") {
		t.Fatalf("run:\n%s", run)
	}
	if g.index(cp+"exec") > g.index(cp+"stop") {
		t.Fatalf("list must precede stop: %v", g.calls())
	}
}

func TestRestoreTimestampWithoutEarlierBackupFailsBeforeStopping(t *testing.T) {
	g := newRestoreRig(t)
	g.run.OnPrefix("docker compose -p app-prod", sys.Result{Stdout: []byte(`[{"backup_name":"base_0000000100000000000000C3","time":"2026-01-02T01:00:00Z"}]`)})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "2026-01-01T12:00:00Z")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_NO_BACKUP" || !strings.Contains(oe.Hint, "older generations") {
		t.Fatalf("err=%v", err)
	}
	if g.has(cp+"stop") != 0 || g.applies != 0 {
		t.Fatalf("must not stop: %v", g.calls())
	}
}

func TestRestoreTargetNotReachedIsCodedAndRolledBack(t *testing.T) {
	g := newRestoreRig(t)
	g.run.OnPrefix(cp+"exec -T -u postgres db wal-g backup-list", sys.Result{Stdout: []byte(`[{"backup_name":"base_0000000100000000000000A1","time":"2026-01-01T01:00:00Z"}]`)})
	g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("false\n")})
	g.run.OnPrefix("docker logs", sys.Result{Stderr: []byte("FATAL:  recovery ended before configured recovery target was reached")})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "2026-01-01T12:00:00Z")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_TARGET" || !strings.Contains(oe.Message, newVol) {
		t.Fatalf("err=%v", err)
	}
	if g.applies != 1 {
		t.Fatalf("instance must be restarted: applies=%d", g.applies)
	}
}

func TestRestoreFailsFastWhenInstanceLocked(t *testing.T) {
	g := newRestoreRig(t)
	sys.LockFile = func(p string, _ os.FileMode, _ sys.Owner) (func(), error) {
		if strings.HasSuffix(p, "/data.lock") {
			return func() {}, nil
		}
		return nil, sys.ErrLocked
	}
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_INSTANCE_LOCKED" || len(g.run.Calls) != 0 {
		t.Fatalf("err=%v calls=%v", err, g.calls())
	}
}

func TestRestoreRefusesExistingVolume(t *testing.T) {
	g := newRestoreRig(t)
	g.run.On("docker volume inspect "+newVol, sys.Result{})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_VOLUME_EXISTS" || g.applies != 0 || g.has("docker run") != 0 {
		t.Fatalf("err=%v calls=%v", err, g.calls())
	}
}

func TestRestoreResticSnapshot(t *testing.T) {
	g := newRestoreRig(t)
	vol := "app-prod_files-data-r1767323045"
	g.run.On("docker volume inspect "+vol, sys.Result{ExitCode: 1})
	g.fs.WriteFile(envPath, []byte("CVCTL_INSTANCE=app-prod\n"), 0o640, sys.Owner{})
	if _, err := g.r.Restore(context.Background(), "app-prod", "files", "a1b2c3d4"); err != nil {
		t.Fatal(err)
	}
	want := "docker run --rm --network app-prod_backup -v " + vol + ":/restore --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 restore a1b2c3d4:/data --target /restore"
	if g.index(want) < 0 {
		t.Fatalf("missing %q in %v", want, g.calls())
	}
	if !strings.Contains(g.envAt[0], "CVCTL_SVC_FILES_VOLUME="+vol) {
		t.Fatalf("env: %s", g.envAt[0])
	}
	for _, to := range []string{"2026-01-01T00:00:00Z", "deploy-1"} {
		if _, err := g.r.Restore(context.Background(), "app-prod", "files", to); err == nil {
			t.Fatalf("restic must reject %q", to)
		}
	}
}

func cleanupRig(t *testing.T) *rig {
	t.Helper()
	g := newRig(t)
	p := &contract.Project{Data: []contract.DataService{{Name: "db", Kind: "postgres", Backup: "wal-g"}}}
	d := &instance.Decisions{Instance: "app-prod", Services: map[string]instance.ServiceDecision{"db": {Mode: "local", Volume: "app-prod_db-data-r5"}}}
	g.r.Load = func(string) (*contract.Project, *instance.Decisions, error) { return p, d, nil }
	g.r.Catalog = kinds.Catalog{}
	g.fs.WriteFile(envPath, []byte("CVCTL_SVC_DB_VOLUME=app-prod_db-data-r5\n"), 0o640, sys.Owner{})
	vols := "app-prod_db-data\napp-prod_db-data-r5\napp-prod_db-data-r9\napp-prod_db-data-r7\napp-prod_uploads\napp-prod-x_db-data\nother-prod_db-data\napp-prod_db-data-rx\n"
	g.run.On("docker volume ls -q", sys.Result{Stdout: []byte(vols)})
	g.run.OnPrefix("docker ps -a -q --filter volume=", sys.Result{})
	g.run.On("docker ps -a -q --filter volume=app-prod_db-data-r7", sys.Result{Stdout: []byte("abc123\n")})
	g.run.OnPrefix("docker volume rm", sys.Result{})
	return g
}

func TestCleanupListsOnlyUnusedUnreferencedVolumesOfTheInstance(t *testing.T) {
	g := cleanupRig(t)
	out, err := g.r.Cleanup(context.Background(), "app-prod", false)
	if err != nil {
		t.Fatal(err)
	}
	got := []string{}
	for _, v := range out {
		got = append(got, v.Name)
		if v.Removed {
			t.Fatal("dry run removed")
		}
	}
	if strings.Join(got, ",") != "app-prod_db-data,app-prod_db-data-r9" {
		t.Fatalf("candidates: %v", got)
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("no --yes must not delete: %v", g.calls())
	}
}

func TestCleanupYesRemovesOnlyCandidates(t *testing.T) {
	g := cleanupRig(t)
	out, err := g.r.Cleanup(context.Background(), "app-prod", true)
	if err != nil || len(out) != 2 || !out[0].Removed || !out[1].Removed {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	for _, c := range g.calls() {
		if strings.HasPrefix(c, "docker volume rm") && c != "docker volume rm app-prod_db-data" && c != "docker volume rm app-prod_db-data-r9" {
			t.Fatalf("unexpected removal: %s", c)
		}
	}
}

func TestCleanupFailsClosedWhenInUseCheckFails(t *testing.T) {
	g := cleanupRig(t)
	g.run.OnPrefix("docker ps -a -q --filter volume=", sys.Result{ExitCode: 1, Stderr: []byte("daemon down")})
	if _, err := g.r.Cleanup(context.Background(), "app-prod", true); err == nil || g.has("docker volume rm") != 0 {
		t.Fatalf("err=%v calls=%v", err, g.calls())
	}
}

func TestRestoreRecoveryFlagsNeverTouchAutoConf(t *testing.T) {
	g := newRestoreRig(t)
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err != nil {
		t.Fatal(err)
	}
	run := g.calls()[g.index("docker run -d")]
	if strings.Contains(run, "postgresql.auto.conf") || strings.Contains(run, "tee") {
		t.Fatalf("recovery settings must not be persisted:\n%s", run)
	}
	if !strings.Contains(run, `exec gosu postgres postgres "$@"`) || !strings.Contains(run, "recovery.signal") {
		t.Fatalf("run:\n%s", run)
	}
}

func TestRestoreTimeTargetKeepsSubsecondPrecision(t *testing.T) {
	g := newRestoreRig(t)
	g.run.OnPrefix(cp+"exec -T -u postgres db wal-g backup-list", sys.Result{Stdout: []byte(`[{"backup_name":"base_0000000100000000000000A1","time":"2026-01-01T01:00:00Z"}]`)})
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "2026-01-01T12:00:00.250Z"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.calls()[g.index("docker run -d")], "-e RESTORE_TARGET_TIME=2026-01-01 12:00:00.25+00") {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestRestoreAdvancesGenerationAndBacksUp(t *testing.T) {
	g := newRestoreRig(t)
	g.fs.WriteFile(envPath, []byte("CVCTL_INSTANCE=app-prod\nCVCTL_SVC_DB_BACKUP_GEN=2\nCVCTL_SVC_DB_BACKUP_GEN_MAX=3\n"), 0o640, sys.Owner{})
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(g.envAt[0], "CVCTL_SVC_DB_BACKUP_GEN=4") || !strings.Contains(g.envAt[0], "CVCTL_SVC_DB_BACKUP_GEN_MAX=4") {
		t.Fatalf("env: %s", g.envAt[0])
	}
	if g.index(cp+"exec -T -u postgres db wal-g backup-push") < g.index("APPLY") {
		t.Fatalf("backup-push must follow the switch: %v", g.calls())
	}
}

func TestRestoreRollbackRestoresGenerationButKeepsMax(t *testing.T) {
	g := newRestoreRig(t)
	g.fs.WriteFile(envPath, []byte("CVCTL_INSTANCE=app-prod\nCVCTL_SVC_DB_BACKUP_GEN=2\nCVCTL_SVC_DB_BACKUP_GEN_MAX=3\n"), 0o640, sys.Owner{})
	g.applyErrs = []error{errors.New("boom"), nil}
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(g.envAt[1], "CVCTL_SVC_DB_BACKUP_GEN=2\n") || !strings.Contains(g.envAt[1], "CVCTL_SVC_DB_BACKUP_GEN_MAX=4") {
		t.Fatalf("rolled-back env: %s", g.envAt[1])
	}
	if g.index(cp+"exec -T -u postgres db wal-g backup-push") >= 0 {
		t.Fatal("no base backup after a rollback")
	}
}

func TestRestoreBackupPushFailureIsWarningNotRollback(t *testing.T) {
	g := newRestoreRig(t)
	g.run.OnPrefix(cp+"exec -T -u postgres db wal-g backup-push", sys.Result{ExitCode: 1, Stderr: []byte("s3 down " + secretKey)})
	res, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	if err != nil || len(res.Warnings) != 2 || !strings.Contains(res.Warnings[0], "base backup of the new generation failed") || strings.Contains(res.Warnings[0], secretKey) || g.applies != 1 {
		t.Fatalf("res=%+v err=%v applies=%d", res, err, g.applies)
	}
	warned := false
	for _, m := range g.note.msgs {
		warned = warned || m.Level == "warn"
	}
	if !warned {
		t.Fatalf("notify: %+v", g.note.msgs)
	}
}

func TestRestoreRollsBackWhenDataServiceDoesNotServe(t *testing.T) {
	for name, fail := range map[string]string{
		"wait":     cp + "up -d --wait --wait-timeout 150 db",
		"isready":  cp + "exec -T -u postgres db pg_isready",
		"select 1": cp + "exec -T -u postgres db psql -U app -d appdb -v ON_ERROR_STOP=1 -tA -c select 1",
	} {
		g := newRestoreRig(t)
		g.run.OnPrefix(fail, sys.Result{ExitCode: 1, Stderr: []byte("not ready")})
		_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Code != "E_RESTORE_FAILED" || g.applies != 2 {
			t.Fatalf("%s: err=%v applies=%d", name, err, g.applies)
		}
	}
}

func TestRestoreArchiveLagAbortsWithoutTouchingVolumesOrDecisions(t *testing.T) {
	g := newRestoreRig(t)
	g.r.ArchiveTimeout = 10 * time.Second
	g.run.OnPrefix(cp+"exec -T -u postgres db psql -U app -d postgres -v ON_ERROR_STOP=1 -tA -c select coalesce", sys.Result{Stdout: []byte("000000010000000000000004\n")})
	before, _ := g.fs.ReadFile(envPath)
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_ARCHIVE_LAG" {
		t.Fatalf("err=%v", err)
	}
	after, _ := g.fs.ReadFile(envPath)
	if string(before) != string(after) || g.applies != 0 || g.has("docker volume create") != 0 || g.has("docker run -d") != 0 {
		t.Fatalf("state changed: %v", g.calls())
	}
	if g.index(cp+"stop db") >= 0 {
		t.Fatalf("the database must keep running: %v", g.calls())
	}
	if g.index(cp+"up -d") < 0 {
		t.Fatalf("apps must be restarted: %v", g.calls())
	}
}

func TestRestoreStopPointBeforeConsistentIsTargetError(t *testing.T) {
	g := newRestoreRig(t)
	g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("false\n")})
	g.run.OnPrefix("docker logs", sys.Result{Stderr: []byte("FATAL:  requested recovery stop point is before consistent recovery point")})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "deploy-42")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_TARGET" || !strings.Contains(oe.Hint, "newer than the restore point") {
		t.Fatalf("err=%v", err)
	}
}

func TestVerifyScriptHasNoPersistedRecoverySettings(t *testing.T) {
	g := verifyRig(t, "")
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	run := g.calls()[2]
	if strings.Contains(run, "postgresql.auto.conf") || !strings.Contains(run, `-c "restore_command=wal-g wal-fetch %f %p"`) || !strings.Contains(run, "-c recovery_target_action=promote") {
		t.Fatalf("run:\n%s", run)
	}
}

func TestRestoreNotRunningClusterSkipsFlushWithWarning(t *testing.T) {
	for name, ps := range map[string]string{"exited": "exited\n", "never created": ""} {
		g := newRestoreRig(t)
		g.run.On(probe, sys.Result{Stdout: []byte(ps)})
		res, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		found := false
		for _, w := range res.Warnings {
			if strings.Contains(w, "could not flush WAL from the running cluster") && strings.Contains(w, "app-prod_db-data") {
				found = true
			}
		}
		if !found || g.has(cp+"exec -T -u postgres db psql -U app -d postgres") != 0 {
			t.Fatalf("%s: warnings=%v calls=%v", name, res.Warnings, g.calls())
		}
		if len(g.note.msgs) != 3 || g.note.msgs[1].Level != "warn" || !strings.Contains(g.note.msgs[1].Title, "WAL flush skipped during restore") || strings.Contains(g.note.msgs[1].Title, "backup needed") {
			t.Fatalf("%s: notify: %+v", name, g.note.msgs)
		}
	}
}

func TestRestoreFlushProbesByLabelNotCompose(t *testing.T) {
	g := newRestoreRig(t)
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err != nil {
		t.Fatal(err)
	}
	if g.has(probe) != 1 || g.has(cpBase+" ps") != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestRestoreRunningClusterFlushFailureIsHardStop(t *testing.T) {
	g := newRestoreRig(t)
	g.run.OnPrefix(cp+"exec -T -u postgres db psql -U app -d postgres -v ON_ERROR_STOP=1 -tA -c select pg_walfile_name", sys.Result{ExitCode: 1, Stderr: []byte("FATAL: the database system is starting up")})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_WAL_FLUSH" || !strings.Contains(oe.Hint, "--skip-wal-flush") {
		t.Fatalf("err=%v", err)
	}
	if g.has("docker volume create") != 0 || g.applies != 0 {
		t.Fatalf("nothing may change: %v", g.calls())
	}
}

func TestRestoreSkipWALFlushFlag(t *testing.T) {
	g := newRestoreRig(t)
	g.run.OnPrefix(cp+"exec -T -u postgres db psql -U app -d postgres", sys.Result{ExitCode: 1, Stderr: []byte("broken")})
	res, err := g.r.RestoreWith(context.Background(), "app-prod", "db", "latest", backup.RestoreOptions{SkipWALFlush: true})
	if err != nil {
		t.Fatal(err)
	}
	if g.has(cp+"exec -T -u postgres db psql -U app -d postgres") != 0 || len(res.Warnings) == 0 || !strings.Contains(res.Warnings[0], "could not flush WAL") {
		t.Fatalf("warnings=%v calls=%v", res.Warnings, g.calls())
	}
}

func TestRestoreArchiveLagRecordsHistory(t *testing.T) {
	g := newRestoreRig(t)
	g.r.ArchiveTimeout = 10 * time.Second
	g.run.OnPrefix(cp+"exec -T -u postgres db psql -U app -d postgres -v ON_ERROR_STOP=1 -tA -c select coalesce", sys.Result{Stdout: []byte("000000010000000000000004\n")})
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err == nil {
		t.Fatal("expected lag error")
	}
	hist, _ := g.store.History("app-prod", 5)
	if len(hist) != 1 || hist[0].Kind != "restore" || hist[0].Result != "restore_failed" || !strings.Contains(hist[0].Detail, "E_RESTORE_ARCHIVE_LAG") {
		t.Fatalf("history: %+v", hist)
	}
}

func TestRestorePollIgnoresNonSegmentArchiveNames(t *testing.T) {
	g := newRestoreRig(t)
	g.r.ArchiveTimeout = 10 * time.Second
	g.run.OnPrefix(cp+"exec -T -u postgres db psql -U app -d postgres -v ON_ERROR_STOP=1 -tA -c select coalesce", sys.Result{Stdout: []byte("00000001000000000000000A.00000028.backup\n")})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_ARCHIVE_LAG" {
		t.Fatalf("err=%v", err)
	}
}

func TestRecoveryContainersDisableArchiving(t *testing.T) {
	g := newRestoreRig(t)
	if _, err := g.r.Restore(context.Background(), "app-prod", "db", "latest"); err != nil {
		t.Fatal(err)
	}
	if run := g.calls()[g.index("docker run -d")]; !strings.Contains(run, "archive_mode=off") {
		t.Fatalf("restore script must disable archiving:\n%s", run)
	}
}
