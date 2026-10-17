package sys_test

import (
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
)

func TestRealLockMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	unlock1, err := sys.Lock(lockPath)
	if err != nil {
		t.Fatalf("first Lock failed: %v", err)
	}

	unlock2, err := sys.Lock(lockPath)
	if err == nil {
		unlock2()
		t.Fatal("expected second Lock to fail with ErrLocked, got nil")
	}
	if !errors.Is(err, sys.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}

	unlock1()

	unlock3, err := sys.Lock(lockPath)
	if err != nil {
		t.Fatalf("Lock after unlock failed: %v", err)
	}
	unlock3()
}

func TestRealLockFileModeAndMutualExclusion(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "instance.lock")

	unlock1, err := sys.LockFile(lockPath, 0o660, sys.Owner{User: "deployer", Group: "deploy"})
	if err != nil {
		t.Fatalf("first LockFile failed: %v", err)
	}
	defer unlock1()

	fi, err := os.Stat(lockPath)
	if err != nil {
		t.Fatalf("stat lock file failed: %v", err)
	}
	if fi.Mode().Perm() != 0o660 {
		t.Errorf("expected mode 0660, got %o", fi.Mode().Perm())
	}

	if os.Geteuid() == 0 {
		stat, ok := fi.Sys().(*syscall.Stat_t)
		if ok {
			u, err := user.Lookup("deployer")
			if err == nil {
				wantUid, _ := strconv.Atoi(u.Uid)
				if int(stat.Uid) != wantUid {
					t.Errorf("expected uid %d, got %d", wantUid, stat.Uid)
				}
			}
		}
	}

	unlock2, err := sys.LockFile(lockPath, 0o660, sys.Owner{User: "deployer", Group: "deploy"})
	if err == nil {
		unlock2()
		t.Fatal("expected second LockFile to fail with ErrLocked, got nil")
	}
	if !errors.Is(err, sys.ErrLocked) {
		t.Fatalf("expected ErrLocked, got %v", err)
	}
}
