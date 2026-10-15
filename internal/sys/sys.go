// Package sys is the only package that touches the real system (processes and
// filesystem). Everything else depends on these interfaces so it can be tested
// against the fakes in systest.
package sys

import (
	"context"
	"os"
	"strings"
)

type Cmd struct {
	Name  string
	Args  []string
	Stdin []byte
	Env   []string
}

// String is the key the fake runner matches on.
func (c Cmd) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

type Result struct {
	Stdout, Stderr []byte
	ExitCode       int
}

type Runner interface {
	// Run returns an error only when the command could not start; a non-zero
	// exit is reported in Result.ExitCode.
	Run(ctx context.Context, c Cmd) (Result, error)
}

// Owner with an empty field leaves that part unchanged.
type Owner struct{ User, Group string }

// FileInfo.Mode holds raw Unix permission bits including setuid/setgid/sticky
// (e.g. 0o2750), not Go's os.FileMode flag encoding, so callers can pass
// octal literals directly.
type FileInfo struct {
	Mode        os.FileMode
	User, Group string
	IsDir       bool
}

type FS interface {
	// ReadFile on a missing path returns an error satisfying errors.Is(err, fs.ErrNotExist).
	ReadFile(path string) ([]byte, error)
	// WriteFile is atomic: temp file + fsync + chmod/chown + rename.
	WriteFile(path string, data []byte, mode os.FileMode, o Owner) error
	// MkdirAll also fixes mode/owner of the last dir if it already exists.
	MkdirAll(path string, mode os.FileMode, o Owner) error
	Stat(path string) (FileInfo, error)
	// Remove treats a missing path as success.
	Remove(path string) error
}
