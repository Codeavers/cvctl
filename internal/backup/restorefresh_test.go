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
