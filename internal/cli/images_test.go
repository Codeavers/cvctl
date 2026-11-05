package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/images"
	"github.com/codeavers/cvctl/internal/output"
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
	if !strings.Contains(out, want) || !strings.Contains(out, "10.0MB") || !strings.Contains(out, "would remove unless in use") {
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
	if res.Data.Items[0].Reason != "would remove unless in use" {
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

func imgRow(repo, c string) string {
	return fmt.Sprintf(`{"Repository":%q,"Tag":"<none>","Digest":%q,"Size":"10MB"}`, repo, imgDig(c))
}

func TestImagesPruneRendersEachOutcome(t *testing.T) {
	env, fake := imagesRig(t)
	seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d", "e", "f")
	fake.On(imgListCmd, sys.Result{Stdout: []byte(strings.Join([]string{
		imgRow("r1", "a"), imgRow("r2", "b"), imgRow("r3", "c"),
	}, "\n") + "\n")})
	fake.On("docker image rm r2@"+imgDig("b"), sys.Result{ExitCode: 1, Stderr: []byte("Error response from daemon: conflict: unable to delete 34 (cannot be forced) - image is being used by running container 12")})
	fake.On("docker image rm r3@"+imgDig("c"), sys.Result{ExitCode: 1, Stderr: []byte("image has dependent child images\nsecond line")})

	code, out, errb := run(t, "images", "prune")
	if code != 0 {
		t.Fatalf("code %d err=%s", code, errb)
	}
	for _, want := range []string{
		"r1@" + imgDig("a") + "  10.0MB  removed\n",
		"r2@" + imgDig("b") + "  10.0MB  in use\n",
		"r3@" + imgDig("c") + "  10.0MB  image has dependent child images\n",
		"total: freed 10.0MB\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "second line") {
		t.Errorf("multi-line reason printed whole:\n%s", out)
	}
}

// cancelOnFirstRm stands for Ctrl-C after the first removal.
type cancelOnFirstRm struct {
	*systest.Runner
	cancel context.CancelFunc
}

func (c *cancelOnFirstRm) Run(ctx context.Context, cmd sys.Cmd) (sys.Result, error) {
	res, err := c.Runner.Run(ctx, cmd)
	if strings.HasPrefix(cmd.String(), "docker image rm") {
		c.cancel()
	}
	return res, err
}

func TestImagesPruneInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want []string
	}{
		{"human", nil, []string{"10.0MB  removed\n", "total: freed 10.0MB\n"}},
		{"json", []string{"--json"}, []string{`"E_IMAGES_INTERRUPTED"`, `"removed":true`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env, fake := imagesRig(t, "a", "b", "c", "d", "e")
			seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d", "e")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			env.Deployer.Images = &images.Pruner{Run: &cancelOnFirstRm{Runner: fake, cancel: cancel}}

			var out bytes.Buffer
			p := &output.Printer{JSON: len(tc.args) > 0, Out: &out, Err: &out}
			root := NewRoot("1.2.3")
			root.SetOut(&out)
			root.SetErr(&out)
			root.SetArgs(append([]string{"images", "prune"}, tc.args...))
			err := root.ExecuteContext(ctx)
			if err == nil {
				t.Fatalf("want an error:\n%s", out.String())
			}
			if code := p.Fail(err); code == output.ExitOK {
				t.Fatalf("exit %d", code)
			}
			if got := rmCalls(fake); len(got) != 1 {
				t.Fatalf("removed %v", got)
			}
			for _, want := range tc.want {
				if !strings.Contains(out.String(), want) {
					t.Errorf("output lacks %q:\n%s", want, out.String())
				}
			}
		})
	}
}

// deniedFS refuses every state.json like a host read without sudo.
type deniedFS struct{ sys.FS }

func (d deniedFS) ReadFile(path string) ([]byte, error) {
	if filepath.Base(path) == "state.json" {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrPermission}
	}
	return d.FS.ReadFile(path)
}

func TestImagesPruneSudoHintOnlyOnPermissionErrors(t *testing.T) {
	t.Run("permission denied", func(t *testing.T) {
		env, _ := imagesRig(t, "a", "b", "c", "d")
		seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d")
		denied := state.NewStore(deniedFS{sys.NewOSFS()}, env.Store.Root())
		env.Store, env.Deployer.Store = denied, denied

		code, out, _ := run(t, "images", "prune", "--json")
		if code == 0 || !strings.Contains(out, "run with sudo") {
			t.Fatalf("code=%d out=%s", code, out)
		}
	})
	t.Run("corrupt state", func(t *testing.T) {
		env, _ := imagesRig(t, "a", "b", "c", "d")
		seedImgInstance(t, env.Store, "app-dev", "a", "b", "c", "d")
		if err := os.WriteFile(filepath.Join(env.Store.Root(), "app-dev", "state.json"), []byte("{"), 0o640); err != nil {
			t.Fatal(err)
		}

		code, out, _ := run(t, "images", "prune", "--json")
		if code == 0 || strings.Contains(out, "sudo") || !strings.Contains(out, "nothing was removed") {
			t.Fatalf("code=%d out=%s", code, out)
		}
	})
}
