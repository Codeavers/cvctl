package images

import (
	"fmt"
	"reflect"
	"sort"
	"testing"

	"github.com/codeavers/cvctl/internal/state"
)

func dg(n int) string { return fmt.Sprintf("sha256:%064d", n) }

func des(m map[string]string) *state.Desired { return &state.Desired{Digests: m} }

// ev builds a deploy event; events are passed newest first, as Store.History returns them.
func ev(from, to map[string]string) state.Event {
	e := state.Event{Kind: "deploy", Result: "OK"}
	if from != nil {
		e.From = des(from)
	}
	if to != nil {
		e.To = des(to)
	}
	return e
}

func evk(kind, result string, from, to map[string]string) state.Event {
	e := ev(from, to)
	e.Kind, e.Result = kind, result
	return e
}

func api(n int) map[string]string { return map[string]string{"api": dg(n)} }

func sorted(m map[string]bool) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestDecide(t *testing.T) {
	fiveVersions := []state.Event{
		ev(api(4), api(5)), ev(api(3), api(4)), ev(api(2), api(3)), ev(api(1), api(2)), ev(nil, api(1)),
	}
	tests := []struct {
		name       string
		all        []Instance
		keepPrev   int
		wantKeep   []string
		wantCandNo []string // candidates that must not be kept
	}{
		{
			name:       "current and two previous kept",
			all:        []Instance{{Name: "a", State: &state.State{Current: des(api(5)), Previous: des(api(4))}, Events: fiveVersions}},
			keepPrev:   2,
			wantKeep:   []string{dg(3), dg(4), dg(5)},
			wantCandNo: []string{dg(1), dg(2)},
		},
		{
			name: "repeated rollback counts once",
			all: []Instance{{Name: "a", State: &state.State{Current: des(api(5)), Previous: des(api(4))}, Events: []state.Event{
				ev(api(4), api(5)), ev(api(5), api(4)), ev(api(4), api(5)), ev(api(3), api(4)), ev(api(2), api(3)), ev(api(1), api(2)),
			}}},
			keepPrev:   2,
			wantKeep:   []string{dg(3), dg(4), dg(5)},
			wantCandNo: []string{dg(1), dg(2)},
		},
		{
			name: "digest kept by another instance",
			all: []Instance{
				{Name: "a", State: &state.State{Current: des(api(5)), Previous: des(api(4))}, Events: fiveVersions},
				{Name: "b", State: &state.State{Current: des(api(1))}},
			},
			keepPrev:   2,
			wantKeep:   []string{dg(1), dg(3), dg(4), dg(5)},
			wantCandNo: []string{dg(2)},
		},
		{
			name:       "previous outside the window is kept",
			all:        []Instance{{Name: "a", State: &state.State{Current: des(api(5)), Previous: des(api(1))}, Events: fiveVersions}},
			keepPrev:   2,
			wantKeep:   []string{dg(1), dg(3), dg(4), dg(5)},
			wantCandNo: []string{dg(2)},
		},
		{
			name: "retired service keeps its last three",
			all: []Instance{{Name: "a", State: &state.State{Current: des(map[string]string{"web": dg(9)})}, Events: []state.Event{
				ev(api(4), map[string]string{"web": dg(9)}),
				ev(api(3), api(4)), ev(api(2), api(3)), ev(api(1), api(2)),
			}}},
			keepPrev:   2,
			wantKeep:   []string{dg(2), dg(3), dg(4), dg(9)},
			wantCandNo: []string{dg(1)},
		},
		{
			name:     "deploy in progress keeps every digest of the instance",
			all:      []Instance{{Name: "a", State: &state.State{Status: state.StatusDeploying, Current: des(api(5))}, Events: fiveVersions}},
			keepPrev: 2,
			wantKeep: []string{dg(1), dg(2), dg(3), dg(4), dg(5)},
		},
		{
			name:     "missing state keeps every digest of the instance",
			all:      []Instance{{Name: "a", Events: fiveVersions}},
			keepPrev: 2,
			wantKeep: []string{dg(1), dg(2), dg(3), dg(4), dg(5)},
		},
		{
			name: "services are windowed independently",
			all: []Instance{{Name: "a", State: &state.State{Current: des(map[string]string{"api": dg(5), "web": dg(15)})}, Events: []state.Event{
				ev(map[string]string{"api": dg(4), "web": dg(14)}, map[string]string{"api": dg(5), "web": dg(15)}),
				ev(map[string]string{"api": dg(3), "web": dg(13)}, map[string]string{"api": dg(4), "web": dg(14)}),
				ev(map[string]string{"api": dg(2), "web": dg(12)}, map[string]string{"api": dg(3), "web": dg(13)}),
			}}},
			keepPrev:   2,
			wantKeep:   []string{dg(3), dg(4), dg(5), dg(13), dg(14), dg(15)},
			wantCandNo: []string{dg(2), dg(12)},
		},
		{
			name:     "no history means no candidates",
			all:      []Instance{{Name: "a", State: &state.State{Current: des(api(5)), Previous: des(api(4))}}},
			keepPrev: 2,
			wantKeep: []string{dg(4), dg(5)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Decide(tc.all, tc.keepPrev)
			if got := sorted(p.Keep); !reflect.DeepEqual(got, tc.wantKeep) {
				t.Errorf("Keep = %v, want %v", got, tc.wantKeep)
			}
			for _, d := range tc.wantCandNo {
				if !p.Candidates[d] || p.Keep[d] {
					t.Errorf("%s: want candidate and not kept (candidate=%v keep=%v)", d, p.Candidates[d], p.Keep[d])
				}
			}
			if len(tc.wantCandNo) == 0 {
				for d := range p.Candidates {
					if !p.Keep[d] {
						t.Errorf("%s is deletable, want everything kept", d)
					}
				}
			}
		})
	}
}

func TestDecideFiveVersionsDeletable(t *testing.T) {
	p := Decide([]Instance{{Name: "api", State: &state.State{Current: des(api(5)), Previous: des(api(4))}, Events: []state.Event{
		ev(api(4), api(5)), ev(api(3), api(4)), ev(api(2), api(3)), ev(api(1), api(2)), ev(nil, api(1)),
	}}}, 2)
	n := 0
	for d := range p.Candidates {
		if !p.Keep[d] {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("deletable = %d, want 2", n)
	}
}

func TestDecideIgnoresDeploysThatNeverRan(t *testing.T) {
	ok := func(from, to map[string]string) state.Event { return evk("deploy", "OK", from, to) }
	tests := []struct {
		name     string
		cur      int
		prev     int
		events   []state.Event
		wantKeep []string
		wantDel  []string
	}{
		{
			name: "failed deploys use no slot",
			cur:  5, prev: 4,
			events: []state.Event{
				evk("failed", "FAILED", api(5), api(7)),
				evk("failed", "FAILED", api(5), api(6)),
				ok(api(4), api(5)), ok(api(3), api(4)), ok(api(2), api(3)), ok(nil, api(2)),
			},
			wantKeep: []string{dg(3), dg(4), dg(5)},
			wantDel:  []string{dg(2), dg(6), dg(7)},
		},
		{
			name: "rollback_failed and blocked use no slot",
			cur:  5, prev: 4,
			events: []state.Event{
				evk("rollback", "rollback_failed", api(5), api(8)),
				evk("blocked", "BLOCKED", api(5), api(9)),
				ok(api(4), api(5)), ok(api(3), api(4)), ok(api(2), api(3)),
			},
			wantKeep: []string{dg(3), dg(4), dg(5)},
			wantDel:  []string{dg(2), dg(8), dg(9)},
		},
		{
			name: "interrupted event does not take a slot",
			cur:  5, prev: 4,
			events: []state.Event{
				evk("interrupted", "interrupted", api(5), api(6)),
				ok(api(4), api(5)), ok(api(3), api(4)), ok(api(2), api(3)),
			},
			wantKeep: []string{dg(3), dg(4), dg(5)},
			wantDel:  []string{dg(2), dg(6)},
		},
		{
			name: "repeated rollbacks keep true order",
			cur:  3, prev: 1,
			events: []state.Event{
				ok(api(1), api(3)),                    // C after the rollback
				evk("rollback", "OK", api(2), api(1)), // back to A
				ok(api(1), api(2)),                    // B
				ok(nil, api(1)),                       // A
			},
			wantKeep: []string{dg(1), dg(2), dg(3)},
		},
		{
			name: "rollback window order",
			cur:  4, prev: 1,
			events: []state.Event{
				ok(api(1), api(4)),
				evk("rollback", "OK", api(3), api(1)),
				ok(api(2), api(3)),
				ok(api(1), api(2)),
				ok(nil, api(1)),
				ok(nil, api(0)),
			},
			wantKeep: []string{dg(1), dg(3), dg(4)},
			wantDel:  []string{dg(0), dg(2)},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := Decide([]Instance{{Name: "a", State: &state.State{Current: des(api(tc.cur)), Previous: des(api(tc.prev))}, Events: tc.events}}, 2)
			if got := sorted(p.Keep); !reflect.DeepEqual(got, tc.wantKeep) {
				t.Errorf("Keep = %v, want %v", got, tc.wantKeep)
			}
			for _, d := range tc.wantDel {
				if !p.Candidates[d] || p.Keep[d] {
					t.Errorf("%s: want deletable (candidate=%v keep=%v)", d, p.Candidates[d], p.Keep[d])
				}
			}
		})
	}
}
