package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const freshVol = "app-prod_db-data"

func TestRestoreFreshRecoversIntoNewVolumeFromSourceGeneration(t *testing.T) {
	g := newRestoreRig(t)
	p, d, _ := g.r.Load("app-prod")
	sd := d.Services["db"]
	sd.BackupGen = 2
	d.Services["db"] = sd
	g.run.On("docker volume inspect "+freshVol, sys.Result{ExitCode: 1})
	g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("true\n")})
	g.run.OnPrefix("docker exec "+recName+" psql", sys.Result{Stdout: []byte("f\n")})

	if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, freshVol); err != nil {
		t.Fatal(err)
	}
	order := []string{"docker volume inspect " + freshVol, "docker volume create " + freshVol, "docker run -d --name " + recName, "docker stop -t 120 " + recName}
	last := -1
	for _, want := range order {
		i := g.index(want)
		if i <= last {
			t.Fatalf("%q out of order: %v", want, g.calls())
		}
		last = i
	}
	run := g.calls()[g.index("docker run -d")]
	if !strings.Contains(run, "-v "+freshVol+":/var/lib/postgresql/data") || !strings.Contains(run, "-e RESTORE_BACKUP=LATEST") {
		t.Fatalf("run: %s", run)
	}
	body, err := g.fs.ReadFile("/srv/deploy/app-prod/backup-db.env")
	if err != nil || !strings.Contains(string(body), "/app-prod/db/g2") {
		t.Fatalf("env file must point at the source generation: %q %v", body, err)
	}
	if g.has("docker volume rm") != 0 {
		t.Fatalf("no volume may be removed: %v", g.calls())
	}
}

func TestRestoreFreshRefusesExistingVolume(t *testing.T) {
	g := newRestoreRig(t)
	p, d, _ := g.r.Load("app-prod")
	g.run.On("docker volume inspect "+freshVol, sys.Result{ExitCode: 0})
	err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, freshVol)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_RESTORE_VOLUME_EXISTS" || oe.Hint == "" {
		t.Fatalf("got %v", err)
	}
	if g.has("docker volume create") != 0 || g.has("docker run") != 0 {
		t.Fatalf("nothing may run: %v", g.calls())
	}
}

func TestRestoreFreshResticOnly(t *testing.T) {
	g := newRestoreRig(t)
	p, d, _ := g.r.Load("app-prod")
	vol := "app-prod_files-data"
	g.run.On("docker volume inspect "+vol, sys.Result{ExitCode: 1})
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "files", p, d, vol); err != nil {
		t.Fatal(err)
	}
	calls := g.calls()
	want := "docker run --rm --network cvctl-backup -v " + vol + ":/restore --env-file /srv/deploy/app-prod/restic-files.env restic/restic:0.19.1 restore latest:/data --target /restore"
	if g.index(want) < 0 || g.index("docker volume create "+vol) > g.index(want) {
		t.Fatalf("calls: %v", calls)
	}
	if g.index("docker run -d") >= 0 {
		t.Fatalf("restic must not start a postgres container: %v", calls)
	}
	for _, c := range calls {
		if strings.Contains(c, resticPass) || strings.Contains(c, secretKey) {
			t.Fatalf("secret in argv: %s", c)
		}
	}
}

func TestRestoreFreshRemovesOnlyItsOwnVolumeOnFailure(t *testing.T) {
	g := newRestoreRig(t)
	p, d, _ := g.r.Load("app-prod")
	g.run.On("docker volume inspect "+freshVol, sys.Result{ExitCode: 1})
	g.run.On("docker inspect -f {{.State.Running}} "+recName, sys.Result{Stdout: []byte("false\n")})
	g.run.OnPrefix("docker logs", sys.Result{Stderr: []byte("wal-g: no such backup")})
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, freshVol); err == nil {
		t.Fatal("expected failure")
	}
	if g.index("docker volume rm -f "+freshVol) < 0 || g.index("docker rm -f "+recName) > g.index("docker volume rm -f "+freshVol) {
		t.Fatalf("the created volume is removed after its container: %v", g.calls())
	}

	g = newRestoreRig(t)
	p, d, _ = g.r.Load("app-prod")
	vol := "app-prod_files-data"
	g.run.On("docker volume inspect "+vol, sys.Result{ExitCode: 1})
	g.run.OnErrorPrefix("docker run --rm --network cvctl-backup -v "+vol, errors.New("restic failed"))
	if err := g.r.RestoreFresh(context.Background(), "app-prod", "files", p, d, vol); err == nil {
		t.Fatal("expected failure")
	}
	if g.index("docker volume rm -f "+vol) < 0 {
		t.Fatalf("restic volume must be removed: %v", g.calls())
	}
}

func TestRestoreFreshExistingVolumeIsNeverRemoved(t *testing.T) {
	g := newRestoreRig(t)
	p, d, _ := g.r.Load("app-prod")
	g.run.On("docker volume inspect "+freshVol, sys.Result{ExitCode: 0})
	_ = g.r.RestoreFresh(context.Background(), "app-prod", "db", p, d, freshVol)
	if g.has("docker volume rm") != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestRemoveVolume(t *testing.T) {
	g := newRestoreRig(t)
	if err := g.r.RemoveVolume(context.Background(), freshVol); err != nil {
		t.Fatal(err)
	}
	if g.index("docker volume rm "+freshVol) != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}
