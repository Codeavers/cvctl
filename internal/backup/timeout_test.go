package backup_test

import (
	"context"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/sys"
)

// deadlineRunner records how long each command was allowed to run.
type deadlineRunner struct {
	sys.Runner
	left []time.Duration
}

func (d *deadlineRunner) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if dl, ok := ctx.Deadline(); ok {
		d.left = append(d.left, time.Until(dl))
	} else {
		d.left = append(d.left, 0)
	}
	return d.Runner.Run(ctx, c)
}

func TestBackupBoundsEachServiceRun(t *testing.T) {
	g := newRig(t)
	g.okAll()
	dr := &deadlineRunner{Runner: g.run}
	g.r.Run = dr
	if _, err := g.r.BackupNow(context.Background(), "app-prod", ""); err != nil {
		t.Fatal(err)
	}
	if len(dr.left) == 0 {
		t.Fatal("no commands ran")
	}
	for _, l := range dr.left {
		if l <= 2*time.Hour+59*time.Minute || l > 3*time.Hour {
			t.Fatalf("deadline %v, want 3h per service", l)
		}
	}
}

func TestVerifyBoundsEachServiceRun(t *testing.T) {
	g := verifyRig(t, "")
	dr := &deadlineRunner{Runner: g.run}
	g.r.Run = dr
	if _, err := g.r.Verify(context.Background(), "app-prod", "db"); err != nil {
		t.Fatal(err)
	}
	for _, l := range dr.left {
		// cleanup runs on its own short context after the service ctx ends
		if l != 0 && l <= time.Minute {
			continue
		}
		if l <= time.Hour+59*time.Minute || l > 2*time.Hour {
			t.Fatalf("deadline %v, want 2h per service", l)
		}
	}
}
