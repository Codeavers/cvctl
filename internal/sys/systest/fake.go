// Package systest provides in-memory fakes of sys.Runner and sys.FS.
package systest

import (
	"context"
	"io/fs"
	"os"
	"path"
	"strings"
	"sync"

	"github.com/codeavers/cvctl/internal/sys"
)

type Runner struct {
	Calls    []sys.Cmd
	exact    map[string]sys.Result
	prefixes map[string]sys.Result
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

// Run matches exact registrations first, then the longest registered prefix;
// anything else exits 127 like a shell would.
func (r *Runner) Run(_ context.Context, c sys.Cmd) (sys.Result, error) {
	r.Calls = append(r.Calls, c)
	key := c.String()
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
