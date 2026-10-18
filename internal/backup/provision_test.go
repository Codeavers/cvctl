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

func TestProvisionUserSkipsHostTargets(t *testing.T) {
	run := &systest.Runner{}
	for _, tg := range []*render.BackupTarget{nil, {Local: true}, {Instance: "app-prod"}} {
		if err := backup.ProvisionUser(context.Background(), run, tg, "app-prod"); err != nil {
			t.Fatal(err)
		}
	}
	if len(run.Calls) != 0 {
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
	if !errors.As(err, &oe) || oe.Code != "E_BACKUP_PROVISION" || oe.Hint != "deploy the instance first: cvctl deploy app-prod" {
		t.Fatalf("got %v", err)
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
