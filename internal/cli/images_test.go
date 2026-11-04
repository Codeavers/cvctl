package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/images"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const (
	imgListCmd = "docker image ls --digests --format {{json .}}"
	imgRepo    = "ghcr.io/acme/api"
)

func imgDig(c string) string { return "sha256:" + strings.Repeat(c, 64) }

func imgDesired(c string) *state.Desired {
	return &state.Desired{InfraCommit: strings.Repeat(c, 40), Digests: map[string]string{"api": imgDig(c)}}
}

// seedImgInstance leaves inst as if it had deployed versions in order.
func seedImgInstance(t *testing.T, s *state.Store, inst string, versions ...string) {
	t.Helper()
	for i := 1; i < len(versions); i++ {
		if err := s.Append(inst, state.Event{
			Kind: "deploy", Result: string(state.StatusOK),
			From: imgDesired(versions[i-1]), To: imgDesired(versions[i]),
		}); err != nil {
			t.Fatal(err)
		}
	}
	n := len(versions)
	if err := s.Save(&state.State{
		Instance: inst, Status: state.StatusOK,
		Current: imgDesired(versions[n-1]), Previous: imgDesired(versions[n-2]),
	}); err != nil {
		t.Fatal(err)
	}
}

// imagesRig wires newOpsEnv to a fixture with a fake docker holding onHost.
func imagesRig(t *testing.T, onHost ...string) (*OpsEnv, *systest.Runner) {
	t.Helper()
	_, env := setupOpsFixture(t, "app-dev")
	var rows []string
	for _, c := range onHost {
		rows = append(rows, fmt.Sprintf(`{"Repository":%q,"Digest":%q,"Size":"10MB"}`, imgRepo, imgDig(c)))
	}
	run := &systest.Runner{}
	run.On(imgListCmd, sys.Result{Stdout: []byte(strings.Join(rows, "\n") + "\n")})
	run.OnPrefix("docker image rm", sys.Result{})
	env.Deployer.Images = &images.Pruner{Run: run}
	old := newOpsEnv
	t.Cleanup(func() { newOpsEnv = old })
	newOpsEnv = func(opsOpts) (*OpsEnv, error) { return env, nil }
	return env, run
}

func rmCalls(run *systest.Runner) []string {
	var out []string
	for _, c := range run.Calls {
		if strings.HasPrefix(c.String(), "docker image rm ") {
			out = append(out, c.Args[len(c.Args)-1])
		}
	}
	return out
}

func TestImagesPruneDryRun(t *testing.T) {
	env, fake := imagesRig(t, "a", "b", "c", "d")
	seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d")

	code, out, errb := run(t, "images", "prune", "--dry-run")
	if code != 0 {
		t.Fatalf("code %d err=%s", code, errb)
	}
	if got := rmCalls(fake); len(got) != 0 {
		t.Fatalf("dry run removed %v", got)
	}
	want := imgRepo + "@" + imgDig("a")
	if !strings.Contains(out, want) || !strings.Contains(out, "10.0MB") || !strings.Contains(out, "would remove") {
		t.Fatalf("output lacks image, size and reason:\n%s", out)
	}
	if strings.Contains(out, imgDig("d")) {
		t.Fatalf("current image listed:\n%s", out)
	}
	if !strings.Contains(out, "total: would free 10.0MB") {
		t.Fatalf("no total:\n%s", out)
	}
}

func TestImagesPruneDryRunJSONDedupesTotal(t *testing.T) {
	env, fake := imagesRig(t, "a", "b", "c", "d")
	seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d")
	// The same digest under a second repository counts once in the total.
	fake.On(imgListCmd, sys.Result{Stdout: []byte(
		fmt.Sprintf(`{"Repository":"r1","Digest":%q,"Size":"10MB"}`, imgDig("a")) + "\n" +
			fmt.Sprintf(`{"Repository":"r2","Digest":%q,"Size":"10MB"}`, imgDig("a")) + "\n")})

	code, out, errb := run(t, "images", "prune", "--dry-run", "--json")
	if code != 0 {
		t.Fatalf("code %d err=%s", code, errb)
	}
	var res struct {
		Data struct {
			Items []struct {
				Image  string `json:"image"`
				Size   int64  `json:"size"`
				Reason string `json:"reason"`
			} `json:"items"`
			TotalBytes int64 `json:"total_bytes"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if len(res.Data.Items) != 2 || res.Data.TotalBytes != 10_000_000 {
		t.Fatalf("items=%d total=%d", len(res.Data.Items), res.Data.TotalBytes)
	}
	if res.Data.Items[0].Reason != "would remove" {
		t.Fatalf("reason %q", res.Data.Items[0].Reason)
	}
}

func TestImagesPruneInstance(t *testing.T) {
	env, fake := imagesRig(t, "a", "b", "c", "d", "1", "2", "3", "4")
	seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d")
	seedImgInstance(t, env.Store, "other-dev", "1", "2", "3", "4")

	if code, _, errb := run(t, "images", "prune", "--instance", "app-dev"); code != 0 {
		t.Fatalf("code %d err=%s", code, errb)
	}
	got := rmCalls(fake)
	if len(got) != 1 || got[0] != imgRepo+"@"+imgDig("a") {
		t.Fatalf("removed %v, want only app-dev's surplus", got)
	}
}

func TestImagesPruneUnreadableInstance(t *testing.T) {
	env, fake := imagesRig(t, "a", "b", "c", "d")
	seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d")
	dir := filepath.Join(env.Store.Root(), "broken-dev")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{"), 0o640); err != nil {
		t.Fatal(err)
	}

	code, out, _ := run(t, "images", "prune", "--json")
	if code == 0 || !strings.Contains(out, "broken-dev") {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if got := rmCalls(fake); len(got) != 0 {
		t.Fatalf("removed %v with an unreadable instance", got)
	}
}
