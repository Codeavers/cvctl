package host

import (
	"context"
	"errors"
	"testing"
)

type fakeStep struct {
	name    string
	plan    Plan
	checkEr error
	applyEr error
	pending string
	applied int
}

func (f *fakeStep) Name() string                              { return f.name }
func (f *fakeStep) Check(context.Context, *Env) (Plan, error) { return f.plan, f.checkEr }
func (f *fakeStep) Apply(context.Context, *Env, Plan) (string, error) {
	f.applied++
	return f.pending, f.applyEr
}

func statuses(rs []StepResult) []Status {
	var s []Status
	for _, r := range rs {
		s = append(s, r.Status)
	}
	return s
}

func TestRunStatuses(t *testing.T) {
	steps := []Step{
		&fakeStep{name: "a"},
		&fakeStep{name: "b", plan: Plan{Changes: []string{"x"}}},
		&fakeStep{name: "c", plan: Plan{Skip: "not needed"}},
		&fakeStep{name: "d", plan: Plan{Changes: []string{"y"}}, pending: "do it by hand"},
	}
	rs := Run(context.Background(), &Env{}, steps, false)
	want := []Status{"unchanged", "changed", "skipped", "pending"}
	for i, w := range want {
		if rs[i].Status != w {
			t.Fatalf("step %d: got %s want %s", i, rs[i].Status, w)
		}
	}
	if rs[2].Note != "not needed" || rs[3].Note != "do it by hand" {
		t.Fatalf("notes: %+v", rs)
	}
	if steps[0].(*fakeStep).applied != 0 || steps[2].(*fakeStep).applied != 0 {
		t.Fatal("Apply called on unchanged/skipped step")
	}
}

func TestRunStopsOnFailure(t *testing.T) {
	third := &fakeStep{name: "c", plan: Plan{Changes: []string{"z"}}}
	steps := []Step{
		&fakeStep{name: "a"},
		&fakeStep{name: "b", plan: Plan{Changes: []string{"x"}}, applyEr: errors.New("boom")},
		third,
	}
	rs := Run(context.Background(), &Env{}, steps, false)
	if rs[1].Status != "failed" || rs[1].Error == nil || rs[1].Error.Code != "E_STEP_FAILED" || rs[1].Error.Hint == "" {
		t.Fatalf("got %+v", rs[1])
	}
	if rs[2].Status != "skipped" || rs[2].Note != "previous step failed" || third.applied != 0 {
		t.Fatalf("got %+v", rs[2])
	}
}

func TestRunCheckFailure(t *testing.T) {
	rs := Run(context.Background(), &Env{}, []Step{&fakeStep{name: "a", checkEr: errors.New("x")}}, false)
	if rs[0].Status != "failed" {
		t.Fatalf("got %+v", rs[0])
	}
}

func TestRunDryRun(t *testing.T) {
	s := &fakeStep{name: "a", plan: Plan{Changes: []string{"x"}}}
	rs := Run(context.Background(), &Env{}, []Step{s}, true)
	if rs[0].Status != "planned" || s.applied != 0 || len(rs[0].Changes) != 1 {
		t.Fatalf("got %+v applied=%d", rs[0], s.applied)
	}
}
