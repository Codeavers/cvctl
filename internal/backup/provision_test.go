package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const siloPS = "docker ps --filter label=com.docker.compose.project=cvctl-storage --filter label=com.docker.compose.service=s3 --format {{.ID}}"

func instanceKeys(d *instance.Decisions) {
	d.Backup = instance.BackupDecision{AccessKey: "bk-app-prod", SecretKey: "INST-SECRET-KEY-1", LibsodiumKey: "INST-LIBSODIUM-1", ResticPassword: "INST-RESTIC-1"}
}

func TestProvisionUserRunsScriptInSiloThenConnects(t *testing.T) {
	run := &systest.Runner{}
	run.On(siloPS, sys.Result{Stdout: []byte("abc123\n")})
	run.On("docker exec -i abc123 sh -s", sys.Result{})
	run.On("docker network connect --alias cvctl-storage app-prod_backup abc123", sys.Result{})
	tg := &render.BackupTarget{Host: "h1", Bucket: "cvctl", Local: true, Instance: "app-prod", AccessKey: "bk-app-prod", SecretKey: "INST-SECRET-KEY-1"}
	if err := backup.ProvisionUser(context.Background(), run, tg, "app-prod"); err != nil {
		t.Fatal(err)
	}
	if len(run.Calls) != 3 {
		t.Fatalf("calls: %v", run.Calls)
	}
	exec := run.Calls[1]
	if !strings.Contains(string(exec.Stdin), "mc admin user add local 'bk-app-prod'") ||
		!strings.Contains(string(exec.Stdin), "arn:aws:s3:::cvctl/h1/app-prod/*") ||
		!strings.Contains(string(exec.Stdin), "mc admin policy create local 'cvctl-bk-app-prod'") {
		t.Fatalf("script: %s", exec.Stdin)
	}
	for _, c := range run.Calls {
		if strings.Contains(c.String(), "INST-SECRET") {
			t.Fatalf("secret in argv: %s", c)
		}
		if len(c.Env) != 1 || c.Env[0] != "DOCKER_CONFIG=/var/lib/cvctl/docker" {
			t.Fatalf("missing DOCKER_CONFIG: %v", c)
		}
	}
}

func TestProvisionUserSkipsNonLocalTargets(t *testing.T) {
	run := &systest.Runner{}
	for _, tg := range []*render.BackupTarget{nil, {Instance: "app-prod"}} {
		if err := backup.ProvisionUser(context.Background(), run, tg, "app-prod"); err != nil {
			t.Fatal(err)
		}
	}
	if len(run.Calls) != 0 {
		t.Fatalf("calls: %v", run.Calls)
	}
}

// An instance still on the host credentials gets no user, but its backup
// containers reach the silo only through <inst>_backup.
func TestProvisionUserConnectsHostTargetWithoutUser(t *testing.T) {
	run := &systest.Runner{}
	run.On(siloPS, sys.Result{Stdout: []byte("abc123\n")})
	run.On("docker network connect --alias cvctl-storage app-prod_backup abc123", sys.Result{})
	if err := backup.ProvisionUser(context.Background(), run, &render.BackupTarget{Local: true}, "app-prod"); err != nil {
		t.Fatal(err)
	}
	if len(run.Calls) != 2 || !strings.Contains(run.Calls[1].String(), "network connect") {
		t.Fatalf("calls: %v", run.Calls)
	}
}

func TestProvisionUserFailureIsRedacted(t *testing.T) {
	run := &systest.Runner{}
	run.On(siloPS, sys.Result{Stdout: []byte("abc123\n")})
	run.On("docker exec -i abc123 sh -s", sys.Result{ExitCode: 1, Stderr: []byte("bad secret INST-SECRET-KEY-1")})
	tg := &render.BackupTarget{Local: true, Instance: "app-prod", AccessKey: "bk-app-prod", SecretKey: "INST-SECRET-KEY-1"}
	err := backup.ProvisionUser(context.Background(), run, tg, "app-prod")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_BACKUP_PROVISION" || strings.Contains(oe.Message, "INST-SECRET") || oe.Hint == "" {
		t.Fatalf("got %v", err)
	}
	run2 := &systest.Runner{}
	run2.On(siloPS, sys.Result{})
	if err := backup.ProvisionUser(context.Background(), run2, tg, "app-prod"); !errors.As(err, &oe) || oe.Code != "E_BACKUP_PROVISION" {
		t.Fatalf("no silo: %v", err)
	}
}

func TestBackupNowProvisionsInstanceUserOnSilo(t *testing.T) {
	g := newRestoreRig(t)
	_, d, _ := g.r.Load("app-prod")
	instanceKeys(d)
	g.r.Host.Local = true
	g.run.On(siloPS, sys.Result{})
	_, err := g.r.BackupNow(context.Background(), "app-prod", "db")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_BACKUP_PROVISION" || !strings.Contains(oe.Hint, "cvctl init") {
		t.Fatalf("no silo keeps the silo hint: %v", err)
	}
	g.run.On(siloPS, sys.Result{Stdout: []byte("silo1\n")})
	g.run.On("docker exec -i silo1 sh -s", sys.Result{ExitCode: 1, Stderr: []byte("denied")})
	_, err = g.r.BackupNow(context.Background(), "app-prod", "db")
	if !errors.As(err, &oe) || oe.Code != "E_BACKUP_PROVISION" || oe.Hint != "deploy the instance first: cvctl deploy app-prod" {
		t.Fatalf("user step failed: %v", err)
	}
}

func TestRestoreFreshOfOldGenerationUsesHostKey(t *testing.T) {
	for _, tc := range []struct {
		gen  int
		want string
	}{{2, libsodium}, {3, "INST-LIBSODIUM-1"}} {
		g := newRestoreRig(t)
		p, d, _ := g.r.Load("app-prod")
		instanceKeys(d)
		g.r.Host.Local = true
		g.run.On(siloPS, sys.Result{Stdout: []byte("silo1\n")})
		g.run.On("docker exec -i silo1 sh -s", sys.Result{})
		g.run.On("docker network connect --alias cvctl-storage app-prod_backup silo1", sys.Result{})
		sd := d.Services["db"]
		sd.BackupGen, sd.BackupGenMax, sd.BackupKeyGen = tc.gen, 3, 3
		d.Services["db"] = sd
		g.run.On("docker volume inspect "+freshVol, sys.Result{ExitCode: 1})
		g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("true\n")})
		g.run.OnPrefix("docker exec "+recName+" psql", sys.Result{Stdout: []byte("f\n")})
		if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, freshVol); err != nil {
			t.Fatal(err)
		}
		body, err := g.fs.ReadFile("/srv/deploy/app-prod/backup-db.env")
		if err != nil {
			t.Fatal(err)
		}
		env := string(body)
		if !strings.Contains(env, "WALG_LIBSODIUM_KEY="+tc.want+"\n") || !strings.Contains(env, "AWS_ACCESS_KEY_ID=bk-app-prod\n") {
			t.Fatalf("gen %d env:\n%s", tc.gen, env)
		}
	}
}

const resticSnaps = "docker run --rm --network app-prod_backup --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 snapshots --json"

func movedRestic(t *testing.T, g *restoreRig) {
	t.Helper()
	_, d, _ := g.r.Load("app-prod")
	instanceKeys(d)
	sd := d.Services["files"]
	sd.BackupKeyGen = 1
	d.Services["files"] = sd
}

func TestResticRestoreFallsBackToHostRepository(t *testing.T) {
	g := newRestoreRig(t)
	movedRestic(t, g)
	p, d, _ := g.r.Load("app-prod")
	vol := "app-prod_files-data"
	g.run.On("docker volume inspect "+vol, sys.Result{ExitCode: 1})
	g.run.On(resticSnaps, sys.Result{Stdout: []byte("[]")})
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "files", p, d, vol); err != nil {
		t.Fatal(err)
	}
	if g.index(resticSnaps) < 0 || g.index(resticSnaps) > g.index("docker run --rm --network app-prod_backup -v "+vol+":/restore") {
		t.Fatalf("r1 must be checked before restoring: %v", g.calls())
	}
	body, _ := g.fs.ReadFile("/srv/deploy/app-prod/restic-files.env")
	env := string(body)
	if !strings.Contains(env, "/app-prod/files\n") || strings.Contains(env, "/r1") || !strings.Contains(env, "RESTIC_PASSWORD="+resticPass+"\n") {
		t.Fatalf("restore must read r0 with the host password:\n%s", env)
	}
}

func TestResticRestoreUsesNewRepositoryWhenItHasSnapshots(t *testing.T) {
	g := newRestoreRig(t)
	movedRestic(t, g)
	p, d, _ := g.r.Load("app-prod")
	vol := "app-prod_files-data"
	g.run.On("docker volume inspect "+vol, sys.Result{ExitCode: 1})
	g.run.On(resticSnaps, sys.Result{Stdout: []byte(`[{"short_id":"abcd1234","time":"2026-01-01T00:00:00Z"}]`)})
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "files", p, d, vol); err != nil {
		t.Fatal(err)
	}
	body, _ := g.fs.ReadFile("/srv/deploy/app-prod/restic-files.env")
	if !strings.Contains(string(body), "/app-prod/files/r1\n") || !strings.Contains(string(body), "RESTIC_PASSWORD=INST-RESTIC-1\n") {
		t.Fatalf("restore must read r1 with the instance password:\n%s", body)
	}
}

func TestResticListTagsOlderRepository(t *testing.T) {
	g := newRestoreRig(t)
	movedRestic(t, g)
	g.run.On(resticSnaps, sys.Result{Stdout: []byte(`[{"short_id":"abcd1234","time":"2026-01-01T00:00:00Z"}]`)})
	snaps, err := g.r.List(context.Background(), "app-prod", "files")
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 || snaps[0].Repo != "" || snaps[1].Repo != "r0" {
		t.Fatalf("snaps: %+v", snaps)
	}
}

func TestRestoreHintNamesHostKeyBelowKeyGen(t *testing.T) {
	g := newRestoreRig(t)
	_, d, _ := g.r.Load("app-prod")
	instanceKeys(d)
	sd := d.Services["db"]
	sd.BackupGen, sd.BackupGenMax, sd.BackupKeyGen = 3, 3, 3
	d.Services["db"] = sd
	g.run.OnPrefix("docker compose -p app-prod", sys.Result{Stdout: []byte(`[{"backup_name":"base_0000000100000000000000C3","time":"2026-01-02T01:00:00Z"}]`)})
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "2026-01-01T12:00:00Z")
	var oe *output.Error
	if !errors.As(err, &oe) || !strings.Contains(oe.Hint, "generations below 3 are encrypted with the host key (/etc/cvctl/secrets/walg_libsodium_key)") {
		t.Fatalf("err=%v", err)
	}
}
