package sys_test

import (
	"errors"
	"path/filepath"
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
