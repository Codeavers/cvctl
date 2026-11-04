// Package upgrade compares the JSON documents printed by `cvctl plan --json`.
package upgrade

import (
	"encoding/json"
	"fmt"
	"sort"
)

const absent = "(absent)"

// Change is one differing leaf between two plans. From and To are display strings.
type Change struct{ Path, From, To string }

// Diff compares two JSON documents without interpreting any field, so new plan
// fields need no change here. Lists and scalars are compared as a whole.
func Diff(from, to []byte) ([]Change, error) {
	var a, b any
	if err := json.Unmarshal(from, &a); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if err := json.Unmarshal(to, &b); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	var out []Change
	walk("", a, b, true, true, &out)
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func walk(path string, a, b any, hasA, hasB bool, out *[]Change) {
	ma, okA := a.(map[string]any)
	mb, okB := b.(map[string]any)
	if hasA && hasB && okA && okB {
		keys := map[string]struct{}{}
		for k := range ma {
			keys[k] = struct{}{}
		}
		for k := range mb {
			keys[k] = struct{}{}
		}
		for k := range keys {
			p := k
			if path != "" {
				p = path + "." + k
			}
			va, ia := ma[k]
			vb, ib := mb[k]
			walk(p, va, vb, ia, ib, out)
		}
		return
	}
	fa, fb := render(a, hasA), render(b, hasB)
	if fa != fb {
		*out = append(*out, Change{Path: path, From: fa, To: fb})
	}
}

func render(v any, present bool) string {
	if !present {
		return absent
	}
	if s, ok := v.(string); ok {
		return s
	}
	raw, _ := json.Marshal(v)
	return string(raw)
}
