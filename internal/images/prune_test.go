package images

import (
	"context"
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
	r.On(lsCmd, listing(row("reg/app", "v1", dA, "12.5MB")))
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
	r.On(lsCmd, listing(row("reg/app", "v1", dA, "1.5GB")))
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
	r.On(lsCmd, listing(row("reg/app", "v1", dA, "1MB"), row("reg/web", "v1", dA, "1MB")))
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
