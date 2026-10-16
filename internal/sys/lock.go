package sys

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLocked is returned when an exclusive advisory lock cannot be acquired because
// another process holds it.
var ErrLocked = errors.New("lock already held")

// Lock acquires an exclusive, non-blocking advisory flock on path.
// It returns an unlock function on success. In tests it can be replaced with a fake.
var Lock = realLock

func realLock(path string) (func(), error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrLocked
		}
		return nil, err
	}
	unlocked := false
	return func() {
		if !unlocked {
			unlocked = true
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		}
	}, nil
}
