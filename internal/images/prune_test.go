package images

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const (
	dA = "sha256:aaaa"
	dB = "sha256:bbbb"
	dC = "sha256:cccc"
)

const lsCmd = "docker image ls --digests --format {{json .}}"

func listing(rows ...string) sys.Result {
	return sys.Result{Stdout: []byte(strings.Join(rows, "\n") + "\n")}
}

func row(repo, tag, digest, size string) string {
	return `{"Repository":"` + repo + `","Tag":"` + tag + `","Digest":"` + digest + `","ID":"id1","Size":"` + size + `"}`
}

func rmCalls(r *systest.Runner) []string {
	var out []string
	for _, c := range r.Calls {
		if s := c.String(); strings.HasPrefix(s, "docker image rm") {
			out = append(out, s)
		}
	}
	return out
}

func plan() Plan {
	return Plan{Keep: map[string]bool{dB: true}, Candidates: map[string]bool{dA: true, dB: true}}
}

func TestPruneRemovesByRepoDigest(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(row("reg/app", "<none>", dA, "12.5MB")))
	r.On("docker image rm reg/app@"+dA, sys.Result{})
	items, err := Pruner{Run: r}.Prune(context.Background(), plan(), false)
	if err != nil {
		t.Fatal(err)
	}
	if got := rmCalls(r); len(got) != 1 || got[0] != "docker image rm reg/app@"+dA {
		t.Fatalf("calls: %v", got)
	}
	if len(items) != 1 || !items[0].Removed || items[0].Ref != "reg/app@"+dA || items[0].Size != 12500000 {
		t.Fatalf("items: %+v", items)
	}
}

func TestPruneDryRunRemovesNothing(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(row("reg/app", "<none>", dA, "1.5GB")))
	items, err := Pruner{Run: r}.Prune(context.Background(), plan(), true)
	if err != nil {
		t.Fatal(err)
	}
	if len(rmCalls(r)) != 0 {
		t.Fatalf("removed in dry run: %v", rmCalls(r))
	}
	if len(items) != 1 || items[0].Removed || items[0].Size != 1500000000 {
		t.Fatalf("items: %+v", items)
	}
}

func TestPruneSkipsMissingDigest(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(row("reg/app", "v1", "<none>", "1MB"), row("<none>", "<none>", dA, "1MB")))
	items, err := Pruner{Run: r}.Prune(context.Background(), plan(), false)
	if err != nil || len(items) != 0 || len(rmCalls(r)) != 0 {
		t.Fatalf("items=%+v calls=%v err=%v", items, rmCalls(r), err)
	}
}

func TestPruneIgnoresRejection(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(row("reg/app", "<none>", dA, "1MB"), row("reg/web", "<none>", dA, "1MB")))
	r.On("docker image rm reg/app@"+dA, sys.Result{ExitCode: 1, Stderr: []byte("image is being used by container x")})
	r.On("docker image rm reg/web@"+dA, sys.Result{})
	items, err := Pruner{Run: r}.Prune(context.Background(), plan(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || items[0].Removed || !strings.Contains(items[0].Reason, "being used") || !items[1].Removed {
		t.Fatalf("items: %+v", items)
	}
}

func TestPruneLeavesForeignImages(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(
		row("reg/app", "v9", dC, "1MB"), // not a candidate
		row("reg/app", "v2", dB, "1MB"), // candidate but kept
	))
	items, err := Pruner{Run: r}.Prune(context.Background(), plan(), false)
	if err != nil || len(items) != 0 || len(rmCalls(r)) != 0 {
		t.Fatalf("items=%+v calls=%v err=%v", items, rmCalls(r), err)
	}
}

func TestPruneListFailure(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, sys.Result{ExitCode: 1, Stderr: []byte("daemon down")})
	if _, err := (Pruner{Run: r}).Prune(context.Background(), plan(), false); err == nil {
		t.Fatal("want error")
	}
}

func TestPruneMarksOnlyContainerConflictsAsInUse(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		inUse  bool
	}{
		{"stopped container", `Error response from daemon: conflict: unable to remove repository reference "r" (must force) - container 12 is using its referenced image 34`, true},
		{"running container", `Error response from daemon: conflict: unable to delete 34 (cannot be forced) - image is being used by running container 12`, true},
		{"child images", `Error response from daemon: conflict: unable to delete 34 (cannot be forced) - image has dependent child images`, false},
		{"daemon down", `Cannot connect to the Docker daemon at unix:///var/run/docker.sock`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &systest.Runner{}
			r.On(lsCmd, listing(row("repo", "<none>", dA, "1MB")))
			r.On("docker image rm repo@"+dA, sys.Result{ExitCode: 1, Stderr: []byte(tc.stderr)})

			items, err := Pruner{Run: r}.Prune(context.Background(), Plan{Candidates: map[string]bool{dA: true}}, false)
			if err != nil {
				t.Fatal(err)
			}
			if len(items) != 1 || items[0].Removed || items[0].InUse != tc.inUse || items[0].Reason != tc.stderr {
				t.Fatalf("items = %+v, want InUse %v", items, tc.inUse)
			}
		})
	}
}

func TestPruneExecFailureIsNotInUse(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(row("repo", "<none>", dA, "1MB")))
	r.OnError("docker image rm repo@"+dA, errors.New("conflict with container runtime"))

	items, err := Pruner{Run: r}.Prune(context.Background(), Plan{Candidates: map[string]bool{dA: true}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].InUse || items[0].Reason == "" {
		t.Fatalf("items = %+v", items)
	}
}

func TestPruneSkipsDigestWithRealTag(t *testing.T) {
	cases := []struct {
		name string
		rows []string
	}{
		{"tag on the same repo", []string{row("repo", "<none>", dA, "1MB"), row("repo", "develop", dA, "1MB")}},
		{"tag listed first", []string{row("repo", "develop", dA, "1MB"), row("repo", "<none>", dA, "1MB")}},
		{"tag on another repo", []string{row("repo", "<none>", dA, "1MB"), row("other", "stable", dA, "1MB")}},
		{"tag on a row without repo", []string{row("repo", "<none>", dA, "1MB"), row("<none>", "stable", dA, "1MB")}},
	}
	for _, dryRun := range []bool{false, true} {
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				r := &systest.Runner{}
				r.On(lsCmd, listing(tc.rows...))
				r.OnPrefix("docker image rm", sys.Result{})
				items, err := Pruner{Run: r}.Prune(context.Background(), plan(), dryRun)
				if err != nil || len(items) != 0 || len(rmCalls(r)) != 0 {
					t.Fatalf("items=%+v calls=%v err=%v", items, rmCalls(r), err)
				}
			})
		}
	}
}

func TestPruneTreatsMissingTagAsUntagged(t *testing.T) {
	r := &systest.Runner{}
	r.On(lsCmd, listing(row("repo", "", dA, "1MB"), row("other", "<none>", dA, "1MB")))
	r.OnPrefix("docker image rm", sys.Result{})
	items, err := Pruner{Run: r}.Prune(context.Background(), plan(), false)
	if err != nil || len(items) != 2 || len(rmCalls(r)) != 2 {
		t.Fatalf("items=%+v calls=%v err=%v", items, rmCalls(r), err)
	}
}

// A row that cannot be read may be the one holding the tag of a digest.
func TestPruneRemovesNothingWhenTheListingIsUnreadable(t *testing.T) {
	cases := []struct {
		name string
		bad  string
	}{
		{"not json", "not json"},
		{"longer than the scanner buffer", strings.Repeat("x", 2<<20)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &systest.Runner{}
			r.On(lsCmd, listing(row("repo", "<none>", dA, "1MB"), tc.bad))
			r.OnPrefix("docker image rm", sys.Result{})
			if _, err := (Pruner{Run: r}).Prune(context.Background(), plan(), false); err == nil {
				t.Fatal("want error")
			}
			if got := rmCalls(r); len(got) != 0 {
				t.Fatalf("removed %v", got)
			}
		})
	}
}
