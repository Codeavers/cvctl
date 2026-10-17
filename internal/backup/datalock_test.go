package backup_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const dataLock = "/var/lib/cvctl/data.lock"

func holdDataLock(t *testing.T) (*systest.FakeLocker, func()) {
	t.Helper()
	fl := systest.NewFakeLocker()
	sys.LockFile = fl.LockFile
	unlock, err := fl.LockFile(dataLock, 0o640, sys.Owner{})
	if err != nil {
		t.Fatal(err)
	}
	return fl, unlock
}

func TestBackupWaitsForTheDataLock(t *testing.T) {
	g := newRig(t)
	g.okAll()
	_, unlock := holdDataLock(t)
	sleeps := 0
	g.r.Sleep = func(time.Duration) {
		sleeps++
		if sleeps == 3 {
			unlock()
		}
	}
	if _, err := g.r.BackupNow(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if sleeps != 3 || g.has("docker compose") == 0 {
		t.Fatalf("sleeps=%d calls=%v", sleeps, g.calls())
	}
}

func TestBackupGivesUpOnTheDataLockAfterThreeHours(t *testing.T) {
	g := newRig(t)
	g.okAll()
	holdDataLock(t)
	var waited time.Duration
	g.r.Sleep = func(d time.Duration) { waited += d }
	_, err := g.r.BackupNow(context.Background(), "app-prod", "db")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_DATA_BUSY" || waited < 3*time.Hour || waited > 3*time.Hour+time.Minute {
		t.Fatalf("err=%v waited=%v", err, waited)
	}
	if len(g.run.Calls) != 0 {
		t.Fatalf("ran without the lock: %v", g.calls())
	}
}

func TestVerifyWaitsForTheDataLock(t *testing.T) {
	g := verifyRig(t, "")
	_, unlock := holdDataLock(t)
	sleeps := 0
	g.r.Sleep = func(time.Duration) {
		sleeps++
		if sleeps == 2 {
			unlock()
		}
	}
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if sleeps < 2 {
		t.Fatalf("sleeps=%d", sleeps)
	}
}

func TestRestoreFailsFastWhenDataBusy(t *testing.T) {
	g := newRestoreRig(t)
	holdDataLock(t)
	_, err := g.r.Restore(context.Background(), "app-prod", "db", "latest")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_DATA_BUSY" || oe.Hint == "" {
		t.Fatalf("err=%v", err)
	}
	if len(g.run.Calls) != 0 || g.applies != 0 {
		t.Fatalf("restore touched something: %v", g.calls())
	}
}

func TestBackupHoldsTheDataLockWhileRunning(t *testing.T) {
	g := newRig(t)
	g.okAll()
	fl := systest.NewFakeLocker()
	sys.LockFile = fl.LockFile
	held := false
	g.r.Run = runHook{g.run, func() {
		if _, err := fl.LockFile(dataLock, 0o640, sys.Owner{}); err == sys.ErrLocked {
			held = true
		}
	}}
	if _, err := g.r.BackupNow(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	if !held {
		t.Fatal("data lock not held during the backup")
	}
	if _, err := fl.LockFile(dataLock, 0o640, sys.Owner{}); err != nil {
		t.Fatalf("data lock not released: %v", err)
	}
}

type runHook struct {
	sys.Runner
	hook func()
}

func (h runHook) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	h.hook()
	return h.Runner.Run(ctx, c)
}

func TestDataLockWaitStopsOnCancel(t *testing.T) {
	g := newRig(t)
	g.okAll()
	holdDataLock(t)
	ctx, cancel := context.WithCancel(context.Background())
	sleeps := 0
	g.r.Sleep = func(time.Duration) {
		sleeps++
		if sleeps == 2 {
			cancel()
		}
	}
	_, err := g.r.BackupNow(ctx, "app-prod", "db")
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_CANCELLED" || sleeps != 2 {
		t.Fatalf("err=%v sleeps=%d", err, sleeps)
	}
	if len(g.run.Calls) != 0 {
		t.Fatalf("ran without the lock: %v", g.calls())
	}
}
