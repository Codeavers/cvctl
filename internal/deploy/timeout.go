package deploy

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/codeavers/cvctl/internal/output"
)

// timeoutCause is the context cause that names the step that ran out of time.
type timeoutCause struct {
	step string
	d    time.Duration
}

func (t *timeoutCause) Error() string {
	return fmt.Sprintf("%s timed out after %s", t.step, t.d)
}

func withStepTimeout(ctx context.Context, d time.Duration, step string) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(ctx, d, &timeoutCause{step: step, d: d})
}

// WithStepTimeout bounds ctx by d; if it expires, TimeoutError names step.
func WithStepTimeout(ctx context.Context, d time.Duration, step string) (context.Context, context.CancelFunc) {
	return withStepTimeout(ctx, d, step)
}

// TimeoutError returns an E_TIMEOUT error when ctx ran out of time, nil
// otherwise. The message names the step set by WithStepTimeout. A timeout is
// transient: callers retry on the next cycle.
func TimeoutError(ctx context.Context) *output.Error {
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil
	}
	msg := "operation timed out"
	var tc *timeoutCause
	if errors.As(context.Cause(ctx), &tc) {
		msg = tc.Error()
	}
	return &output.Error{
		Code:    "E_TIMEOUT",
		Message: msg,
		Hint:    "raise the matching CVCTL_TIMEOUT_* key in /etc/cvctl/host.env or find why the step hangs",
	}
}

// stepErrText is the failure text for err from a step run under ctx: the
// E_TIMEOUT text when ctx expired, err otherwise.
func stepErrText(ctx context.Context, err error) string {
	if te := TimeoutError(ctx); te != nil {
		return te.Error()
	}
	return err.Error()
}

func cancelled(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.Canceled)
}
