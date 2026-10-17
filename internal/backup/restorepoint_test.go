package backup_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/sys"
)

const psAll = "docker ps -a --filter label=com.docker.compose.project=app-prod --filter label=com.docker.compose.service=db --format {{.State}}"

func TestRestorePointCommands(t *testing.T) {
	g := newRig(t)
	g.okAll()
	g.run.On(psAll, sys.Result{Stdout: []byte("running\n")})
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load}
	if err := rp.Create(context.Background(), "app-prod", "deploy-1700000000"); err != nil {
		t.Fatal(err)
	}
	c := g.calls()[1:]
	if len(c) != 2 {
		t.Fatalf("calls: %v", c)
	}
	if !strings.HasSuffix(c[0], `exec -T -u postgres db psql -v ON_ERROR_STOP=1 -c select pg_create_restore_point('deploy-1700000000')`) {
		t.Fatalf("restore point: %s", c[0])
	}
	if !strings.HasSuffix(c[1], `exec -T -u postgres db psql -v ON_ERROR_STOP=1 -c select pg_switch_wal()`) {
		t.Fatalf("switch: %s", c[1])
	}
}

func TestRestorePointRejectsBadID(t *testing.T) {
	g := newRig(t)
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load}
	for _, id := range []string{"", "deploy-", "deploy-1'; drop table x;--", "x-1", "deploy-1\n"} {
		err := rp.Create(context.Background(), "app-prod", id)
		if err == nil || !strings.Contains(err.Error(), "E_RESTORE_POINT_ID") {
			t.Fatalf("id %q: err=%v", id, err)
		}
	}
	if len(g.run.Calls) != 0 {
		t.Fatal("no command may run for a bad id")
	}
}

func TestRestorePointNoWalgTargetsIsNoop(t *testing.T) {
	g := newRig(t)
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: func(string) (*contract.Project, *instance.Decisions, error) {
		return &contract.Project{Data: []contract.DataService{{Name: "files", Backup: "volume"}}},
			&instance.Decisions{Instance: "app-prod", Services: map[string]instance.ServiceDecision{"files": {Mode: "local"}}}, nil
	}}
	if err := rp.Create(context.Background(), "app-prod", "deploy-5"); err != nil {
		t.Fatal(err)
	}
	if len(g.run.Calls) != 0 {
		t.Fatalf("calls: %v", g.calls())
	}
}

func TestRestorePointFailureIsCoded(t *testing.T) {
	g := newRig(t)
	g.run.OnPrefix("docker", sys.Result{ExitCode: 1, Stderr: []byte("no")})
	g.run.On(psAll, sys.Result{Stdout: []byte("exited\n")})
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load}
	err := rp.Create(context.Background(), "app-prod", "deploy-5")
	if err == nil || !strings.Contains(err.Error(), "E_RESTORE_POINT_FAILED") {
		t.Fatalf("err=%v", err)
	}
}

func TestRestorePointNeverCreatedContainerIsFreshInstance(t *testing.T) {
	g := newRig(t)
	g.okAll()
	// a fresh instance has no app.env or compose.cvctl.yml yet: any compose call fails
	g.run.OnPrefix("docker compose", sys.Result{ExitCode: 1, Stderr: []byte("open /srv/deploy/app-prod/app.env: no such file or directory")})
	g.run.On(psAll, sys.Result{})
	rp := &backup.RestorePoint{Compose: g.r.Compose, Load: g.r.Load}
	err := rp.Create(context.Background(), "app-prod", "deploy-5")
	if !errors.Is(err, backup.ErrFreshInstance) {
		t.Fatalf("err=%v", err)
	}
	if c := g.calls(); len(c) != 1 || c[0] != psAll {
		t.Fatalf("only the label probe may run: %v", c)
	}
}
