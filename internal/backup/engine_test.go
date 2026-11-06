package backup_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const engineEnv = "/srv/deploy/app-prod/engine-db.env"

// spyRunner calls before each command, then runs it on the fake.
type spyRunner struct {
	*systest.Runner
	before func(ctx context.Context, c sys.Cmd) (sys.Result, bool)
}

func (s spyRunner) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if res, done := s.before(ctx, c); done {
		s.Calls = append(s.Calls, c)
		return res, ctx.Err()
	}
	return s.Runner.Run(ctx, c)
}

func TestEngineHealthProbeHasATimeout(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.r.EngineProbeTimeout = time.Millisecond
	probes := 0
	g.r.Run = spyRunner{Runner: g.run, before: func(ctx context.Context, c sys.Cmd) (sys.Result, bool) {
		if !strings.Contains(c.String(), "sh -c fakeping") {
			return sys.Result{}, false
		}
		probes++
		<-ctx.Done() // a hung engine answers nothing
		return sys.Result{ExitCode: -1}, true
	}}
	done := make(chan error, 1)
	go func() {
		_, err := g.restore("latest")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not healthy after 5m") {
			t.Fatalf("err: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("a hung engine held the restore")
	}
	if probes == 0 {
		t.Fatal("no probe ran")
	}
}

// With only the sleeps counted, slow probes would stretch the 5 minutes.
func TestEngineWaitCountsTheTimeProbesTake(t *testing.T) {
	g := newDumpRestoreRig(t)
	now := dumpNow
	g.r.Clock = func() time.Time { return now }
	probes := 0
	g.r.Run = spyRunner{Runner: g.run, before: func(_ context.Context, c sys.Cmd) (sys.Result, bool) {
		if !strings.Contains(c.String(), "sh -c fakeping") {
			return sys.Result{}, false
		}
		probes++
		now = now.Add(30 * time.Second)
		return sys.Result{ExitCode: 1}, true
	}}
	if _, err := g.restore("latest"); err == nil {
		t.Fatal("want error")
	}
	if probes > 11 {
		t.Fatalf("%d probes of 30s went past the 5 minute limit", probes)
	}
}

func TestEngineEnvFileGoesAsSoonAsDockerRunReturns(t *testing.T) {
	g := newDumpRestoreRig(t)
	checked := false
	g.r.Run = spyRunner{Runner: g.run, before: func(_ context.Context, c sys.Cmd) (sys.Result, bool) {
		if strings.Contains(c.String(), "sh -c fakeping") {
			checked = true
			if _, err := g.fs.Stat(engineEnv); err == nil {
				t.Errorf("the credentials stayed on disk while the engine started")
			}
		}
		return sys.Result{}, false
	}}
	if _, err := g.restore("latest"); err != nil {
		t.Fatal(err)
	}
	if !checked {
		t.Fatal("no probe ran")
	}
}

func TestEngineEnvFileGoesWhenDockerRunFails(t *testing.T) {
	g := newDumpRestoreRig(t)
	g.run.OnPrefix("docker run -d", sys.Result{ExitCode: 125})
	if _, err := g.restore("latest"); err == nil {
		t.Fatal("want error")
	}
	if _, err := g.fs.Stat(engineEnv); err == nil {
		t.Fatal("env file left behind")
	}
}
