// Package images decides which image digests cvctl may remove from a host.
package images

import "github.com/codeavers/cvctl/internal/state"

// Instance is one deployed instance: its state and its history.
// Events must be newest first, the order state.Store.History returns.
type Instance struct {
	Name   string
	State  *state.State
	Events []state.Event
}

// Plan lists digests by role. Candidates are the only digests that may ever
// be removed; Keep always wins over Candidates.
type Plan struct {
	Keep, Candidates map[string]bool
}

// Decide errs towards keeping: a deletion is irreversible, a kept image is not.
func Decide(all []Instance, keepPrevious int) Plan {
	p := Plan{Keep: map[string]bool{}, Candidates: map[string]bool{}}
	for _, in := range all {
		decideInstance(&p, in, keepPrevious)
	}
	return p
}

func decideInstance(p *Plan, in Instance, keepPrevious int) {
	// Per service, distinct digests newest first. Within an event To is newer than From.
	hist := map[string][]string{}
	seen := map[string]map[string]bool{}
	add := func(d *state.Desired) {
		if d == nil {
			return
		}
		for svc, dig := range d.Digests {
			if dig == "" {
				continue
			}
			p.Candidates[dig] = true
			if seen[svc] == nil {
				seen[svc] = map[string]bool{}
			}
			if !seen[svc][dig] {
				seen[svc][dig] = true
				hist[svc] = append(hist[svc], dig)
			}
		}
	}
	for _, ev := range in.Events {
		add(ev.To)
		add(ev.From)
	}

	// Without a readable state the current version is unknown, and a deploy in
	// progress has a target the state does not record.
	if in.State == nil || in.State.Status == state.StatusDeploying {
		for _, digs := range hist {
			for _, dig := range digs {
				p.Keep[dig] = true
			}
		}
	}
	if in.State == nil {
		return
	}

	for _, d := range []*state.Desired{in.State.Current, in.State.Previous} {
		if d == nil {
			continue
		}
		for _, dig := range d.Digests {
			if dig != "" {
				p.Keep[dig] = true
			}
		}
	}

	for svc, digs := range hist {
		var cur string
		if in.State.Current != nil {
			cur = in.State.Current.Digests[svc]
		}
		// A service gone from Current has no current digest to anchor the
		// window, so its newest one counts as the "current" and is kept too.
		limit := keepPrevious
		if cur == "" {
			limit++
		}
		n := 0
		for _, dig := range digs {
			if n >= limit {
				break
			}
			if dig == cur {
				continue
			}
			p.Keep[dig] = true
			n++
		}
	}
}
