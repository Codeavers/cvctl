package sys

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"time"
)

// PipeResult holds the outcome of each side of a Pipe.
type PipeResult struct{ From, To Result }

type Piper interface {
	// Pipe connects from's stdout to to's stdin and runs both to completion.
	// An error means a process could not start or the context ended; non-zero
	// exits are reported in the results. Stdin on to and Stdout on from are
	// ignored.
	Pipe(ctx context.Context, from, to Cmd) (PipeResult, error)
}

type execPiper struct{}

func NewExecPiper() Piper { return execPiper{} }

// stderrCap bounds what is kept of each process's stderr; the rest is
// discarded while still being drained so the process never blocks.
const stderrCap = 64 << 10

type capWriter struct {
	buf bytes.Buffer
}

func (w *capWriter) Write(p []byte) (int, error) {
	if room := stderrCap - w.buf.Len(); room > 0 {
		w.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (execPiper) Pipe(ctx context.Context, from, to Cmd) (PipeResult, error) {
	r, w, err := os.Pipe()
	if err != nil {
		return PipeResult{}, err
	}
	// Idempotent: the parent must drop both ends right after Start so that the
	// consumer exiting delivers SIGPIPE to the producer and EOF reaches the
	// consumer when the producer exits.
	closeEnds := func() { _ = r.Close(); _ = w.Close() }
	defer closeEnds()

	var fromErr, toErr capWriter
	var toOut bytes.Buffer
	pc := build(ctx, from)
	cc := build(ctx, to)
	pc.Stdout, pc.Stderr = w, &fromErr
	cc.Stdin, cc.Stderr = r, &toErr
	cc.Stdout = &toOut
	if to.Stdout != nil {
		cc.Stdout = to.Stdout
	}

	if err := pc.Start(); err != nil {
		return PipeResult{}, err
	}
	if err := cc.Start(); err != nil {
		_ = pc.Cancel()
		_ = pc.Wait()
		return PipeResult{}, err
	}
	closeEnds()

	var wg sync.WaitGroup
	var pErr, cErr error
	wg.Add(2)
	go func() { defer wg.Done(); pErr = pc.Wait() }()
	go func() { defer wg.Done(); cErr = cc.Wait() }()
	wg.Wait()

	res := PipeResult{
		From: Result{Stderr: fromErr.buf.Bytes()},
		To:   Result{Stdout: toOut.Bytes(), Stderr: toErr.buf.Bytes()},
	}
	if ctx.Err() != nil {
		return res, ctx.Err()
	}
	var code int
	if code, err = exitCode(pErr); err != nil {
		return res, err
	}
	res.From.ExitCode = code
	if code, err = exitCode(cErr); err != nil {
		return res, err
	}
	res.To.ExitCode = code
	return res, nil
}

func build(ctx context.Context, c Cmd) *exec.Cmd {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Dir = c.Dir
	if c.Env != nil {
		cmd.Env = append(os.Environ(), c.Env...)
	}
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	// Bounds the wait on stderr copying if a grandchild keeps the pipe open
	// after the process is killed.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func exitCode(err error) (int, error) {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode(), nil
	}
	return 0, err
}
