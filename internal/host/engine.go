// Package host holds the idempotent steps `cvctl init` runs to bring a machine
// to the desired state, and the engine that sequences them.
package host

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

type Env struct {
	Run         sys.Runner
	FS          sys.FS
	Cfg         *hostcfg.Config
	Secrets     *hostcfg.Secrets
	Prompt      hostcfg.Prompter
	Interactive bool
	Adopt       bool
	EUID        int
	Out         io.Writer
}

type Action struct {
	Kind   string
	Target string
	Arg    string
}

// Plan is what Check found. No Changes and no Skip means the step is already
// in the desired state. Pending is something only a person can do.
type Plan struct {
	Changes []string
	Actions []Action
	Skip    string
	Pending string
}

// Step: Check must not mutate anything; Apply does the work. A non-empty
// pending return means Apply finished partially and needs manual action.
type Step interface {
	Name() string
	Check(ctx context.Context, e *Env) (Plan, error)
	Apply(ctx context.Context, e *Env, p Plan) (pending string, err error)
}

type Status string

const (
	StatusUnchanged Status = "unchanged"
	StatusChanged   Status = "changed"
	StatusSkipped   Status = "skipped"
	StatusPending   Status = "pending"
	StatusFailed    Status = "failed"
	StatusPlanned   Status = "planned"
)

type StepResult struct {
	Name    string        `json:"name"`
	Status  Status        `json:"status"`
	Changes []string      `json:"changes"`
	Note    string        `json:"note,omitempty"`
	Error   *output.Error `json:"error,omitempty"`
}

// DefaultSteps is the final step order; later tasks append here.
func DefaultSteps() []Step {
	return []Step{
		PreflightStep{},
		PackagesStep{},
		DockerDaemonStep{},
		AccountsStep{},
		SSHStep{},
		Fail2banStep{},
		FirewallStep{},
		UnattendedStep{},
		TraefikStep{},
		StorageStep{},
		NotifierStep{},
		AgentStep{},
		BackupTimersStep{},
	}
}

func stepErr(name string, err error) *output.Error {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe
	}
	return &output.Error{
		Code:    "E_STEP_FAILED",
		Message: fmt.Sprintf("step %s failed: %v", name, err),
		Hint:    "fix the cause and re-run `cvctl init`; completed steps are idempotent",
	}
}

// Run executes steps in order. After a failure the remaining steps are
// reported as skipped and not run.
func Run(ctx context.Context, e *Env, steps []Step, dryRun bool) []StepResult {
	results := make([]StepResult, 0, len(steps))
	failed := false
	for _, s := range steps {
		r := StepResult{Name: s.Name(), Changes: []string{}}
		if failed {
			r.Status, r.Note = StatusSkipped, "previous step failed"
			results = append(results, r)
			continue
		}
		plan, err := s.Check(ctx, e)
		switch {
		case err != nil:
			r.Status, r.Error, failed = StatusFailed, stepErr(s.Name(), err), true
		case plan.Skip != "":
			r.Status, r.Note = StatusSkipped, plan.Skip
		case len(plan.Changes) == 0:
			r.Changes = []string{}
			if plan.Pending != "" {
				r.Status, r.Note = StatusPending, plan.Pending
			} else {
				r.Status = StatusUnchanged
			}
		case dryRun:
			r.Status, r.Changes, r.Note = StatusPlanned, plan.Changes, plan.Pending
		default:
			r.Changes = plan.Changes
			pending, aerr := s.Apply(ctx, e, plan)
			switch {
			case aerr != nil:
				r.Status, r.Error, failed = StatusFailed, stepErr(s.Name(), aerr), true
			case pending != "":
				r.Status, r.Note = StatusPending, pending
			default:
				r.Status, r.Note = StatusChanged, plan.Pending
			}
		}
		results = append(results, r)
	}
	return results
}
