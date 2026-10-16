// Package systest provides in-memory fakes of sys.Runner and sys.FS.
package systest

import (
	"context"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"sync"
	"syscall"

	"github.com/codeavers/cvctl/internal/sys"
)

type Runner struct {
	Calls       []sys.Cmd
	exact       map[string]sys.Result
	prefixes    map[string]sys.Result
	errors      map[string]error
	errPrefixes map[string]error
}

func (r *Runner) On(cmd string, res sys.Result) {
	if r.exact == nil {
		r.exact = map[string]sys.Result{}
	}
	r.exact[cmd] = res
}

func (r *Runner) OnPrefix(prefix string, res sys.Result) {
	if r.prefixes == nil {
		r.prefixes = map[string]sys.Result{}
	}
	r.prefixes[prefix] = res
}

func (r *Runner) OnError(cmd string, err error) {
	if r.errors == nil {
		r.errors = map[string]error{}
	}
	r.errors[cmd] = err
}

func (r *Runner) OnErrorPrefix(prefix string, err error) {
	if r.errPrefixes == nil {
		r.errPrefixes = map[string]error{}
	}
	r.errPrefixes[prefix] = err
}

// Run matches exact registrations first, then the longest registered prefix;
// anything else exits 127 like a shell would.
func (r *Runner) Run(_ context.Context, c sys.Cmd) (sys.Result, error) {
	r.Calls = append(r.Calls, c)
	key := c.String()
	if err, ok := r.errors[key]; ok {
		return sys.Result{}, err
	}
	bestErr, foundErr := "", false
	for p := range r.errPrefixes {
		if strings.HasPrefix(key, p) && (!foundErr || len(p) > len(bestErr)) {
			bestErr, foundErr = p, true
		}
	}
	if foundErr {
		return sys.Result{}, r.errPrefixes[bestErr]
	}
	if res, ok := r.exact[key]; ok {
		return res, nil
	}
	best, found := "", false
	for p := range r.prefixes {
		if strings.HasPrefix(key, p) && (!found || len(p) > len(best)) {
			best, found = p, true
		}
	}
	if found {
		return r.prefixes[best], nil
	}
	return sys.Result{ExitCode: 127, Stderr: []byte(c.Name + ": command not found")}, nil
}

type entry struct {
	data []byte
	info sys.FileInfo
}

type FS struct {
	mu sync.Mutex
	m  map[string]*entry
}

func NewFS() *FS { return &FS{m: map[string]*entry{}} }

func (f *FS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[path.Clean(p)]
	if !ok || e.info.IsDir {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
	}
	return append([]byte(nil), e.data...), nil
}

func (f *FS) WriteFile(p string, data []byte, mode os.FileMode, o sys.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.put(path.Clean(p), append([]byte(nil), data...), mode, o, false)
	return nil
}

func (f *FS) MkdirAll(p string, mode os.FileMode, o sys.Owner) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	for d := path.Dir(p); d != "/" && d != "." && f.m[d] == nil; d = path.Dir(d) {
		f.put(d, nil, 0o755, sys.Owner{}, true)
	}
	f.put(p, nil, mode, o, true)
	return nil
}

// put keeps the existing owner part when the new one is empty, mirroring
// chown semantics of the real FS.
func (f *FS) put(p string, data []byte, mode os.FileMode, o sys.Owner, dir bool) {
	info := sys.FileInfo{Mode: mode, IsDir: dir}
	if old, ok := f.m[p]; ok {
		info.User, info.Group = old.info.User, old.info.Group
	}
	if o.User != "" {
		info.User = o.User
	}
	if o.Group != "" {
		info.Group = o.Group
	}
	f.m[p] = &entry{data: data, info: info}
}

func (f *FS) Stat(p string) (sys.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[path.Clean(p)]
	if !ok {
		return sys.FileInfo{}, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
	}
	return e.info, nil
}

func (f *FS) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.m, path.Clean(p))
	return nil
}

type fakeDirEntry struct {
	name  string
	isDir bool
	mode  os.FileMode
}

func (e fakeDirEntry) Name() string               { return e.name }
func (e fakeDirEntry) IsDir() bool                { return e.isDir }
func (e fakeDirEntry) Type() os.FileMode          { return e.mode.Type() }
func (e fakeDirEntry) Info() (os.FileInfo, error) { return nil, nil }

func (f *FS) ReadDir(p string) ([]os.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p = path.Clean(p)
	if p != "/" && p != "." {
		e, ok := f.m[p]
		if !ok {
			return nil, &fs.PathError{Op: "readdir", Path: p, Err: fs.ErrNotExist}
		}
		if !e.info.IsDir {
			return nil, &fs.PathError{Op: "readdir", Path: p, Err: syscall.ENOTDIR}
		}
	}
	var entries []os.DirEntry
	seen := map[string]bool{}
	for k, ent := range f.m {
		if k != p && path.Dir(k) == p {
			name := path.Base(k)
			if !seen[name] {
				seen[name] = true
				entries = append(entries, fakeDirEntry{
					name:  name,
					isDir: ent.info.IsDir,
					mode:  ent.info.Mode,
				})
			}
		}
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int {
		return strings.Compare(a.Name(), b.Name())
	})
	return entries, nil
}

// Files returns a snapshot of every file and directory, for assertions.
func (f *FS) Files() map[string]sys.FileInfo {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]sys.FileInfo, len(f.m))
	for p, e := range f.m {
		out[p] = e.info
	}
	return out
}

type LockCall struct {
	Path  string
	Mode  os.FileMode
	Owner sys.Owner
}

// FakeLocker simulates mutual exclusion on file locks and records requests.
type FakeLocker struct {
	mu     sync.Mutex
	locked map[string]bool
	Calls  []LockCall
}

func NewFakeLocker() *FakeLocker {
	return &FakeLocker{locked: make(map[string]bool)}
}

func (fl *FakeLocker) Lock(p string) (func(), error) {
	return fl.LockFile(p, 0o600, sys.Owner{})
}

func (fl *FakeLocker) LockFile(p string, mode os.FileMode, o sys.Owner) (func(), error) {
	fl.mu.Lock()
	defer fl.mu.Unlock()
	cleanPath := path.Clean(p)
	fl.Calls = append(fl.Calls, LockCall{Path: cleanPath, Mode: mode, Owner: o})
	if fl.locked[cleanPath] {
		return nil, sys.ErrLocked
	}
	fl.locked[cleanPath] = true
	return func() {
		fl.mu.Lock()
		defer fl.mu.Unlock()
		delete(fl.locked, cleanPath)
	}, nil
}

// NoopLock is a Lock function that always succeeds with a no-op unlock func.
func NoopLock(_ string) (func(), error) {
	return func() {}, nil
}

// NoopLockFile is a LockFile function that always succeeds with a no-op unlock func.
func NoopLockFile(_ string, _ os.FileMode, _ sys.Owner) (func(), error) {
	return func() {}, nil
}
