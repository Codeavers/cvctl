package sys

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
)

type execRunner struct{}

func NewExecRunner() Runner { return execRunner{} }

func (execRunner) Run(ctx context.Context, c Cmd) (Result, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	if c.Env != nil {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if c.Stdout != nil {
		cmd.Stdout = c.Stdout
	}
	if c.Inherit {
		cmd.Stdin, cmd.Stderr = os.Stdin, os.Stderr
	}
	err := cmd.Run()
	res := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		// A context kill also surfaces as ExitError (code -1); report it as an
		// error so callers do not mistake a timeout for a command failure.
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		res.ExitCode = ee.ExitCode()
		return res, nil
	}
	return res, err
}
