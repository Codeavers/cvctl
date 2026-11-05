package deploy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/images"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const imageListCmd = "docker image ls --digests --format {{json .}}"

const imageRepo = "ghcr.io/acme/api"

func imgDigest(c string) string { return "sha256:" + strings.Repeat(c, 64) }

// observedRunner lets a test look at the world at the moment a command runs.
type observedRunner struct {
	*systest.Runner
	before func(ctx context.Context, c sys.Cmd)
}

func (r *observedRunner) Run(ctx context.Context, c sys.Cmd) (sys.Result, error) {
	if r.before != nil {
		r.before(ctx, c)
	}
	return r.Runner.Run(ctx, c)
}

// usePruner gives the rig a pruner over a host that holds onHost, and captures
// its log.
func usePruner(rig *testRig, onHost ...string) (*observedRunner, *bytes.Buffer) {
	var rows []string
	for _, c := range onHost {
		rows = append(rows, fmt.Sprintf(`{"Repository":%q,"Digest":%q,"Size":"10MB"}`, imageRepo, imgDigest(c)))
	}
	run := &observedRunner{Runner: &systest.Runner{}}
	run.On(imageListCmd, sys.Result{Stdout: []byte(strings.Join(rows, "\n") + "\n")})
	run.OnPrefix("docker image rm", sys.Result{})
	log := &bytes.Buffer{}
	rig.deployer.Images = &images.Pruner{Run: run}
	rig.deployer.Log = log
	return run, log
}

func removedImages(run *observedRunner) []string {
	var out []string
	for _, c := range run.Calls {
		if strings.HasPrefix(c.String(), "docker image rm ") {
			out = append(out, c.Args[len(c.Args)-1])
		}
	}
	slices.Sort(out)
	return out
}

func imageRefs(cs ...string) []string {
	var out []string
	for _, c := range cs {
		out = append(out, imageRepo+"@"+imgDigest(c))
	}
	slices.Sort(out)
	return out
}

func apiDesired(c string) *state.Desired {
	return &state.Desired{
		InfraCommit: strings.Repeat(c, 40),
		Digests:     map[string]string{"api": imgDigest(c)},
	}
}

// appendDeploys records one successful deploy per version, oldest first.
func appendDeploys(t *testing.T, s *state.Store, inst string, versions ...string) {
	t.Helper()
	for i := 1; i < len(versions); i++ {
		if err := s.Append(inst, state.Event{
			Kind: "deploy", Result: string(state.StatusOK),
			From: apiDesired(versions[i-1]), To: apiDesired(versions[i]),
		}); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// seedInstance leaves inst as if it had deployed versions in order.
func seedInstance(t *testing.T, s *state.Store, inst string, versions ...string) {
	t.Helper()
	appendDeploys(t, s, inst, versions...)
	n := len(versions)
	if err := s.Save(&state.State{
		Instance: inst, Status: state.StatusOK,
		Current: apiDesired(versions[n-1]), Previous: apiDesired(versions[n-2]),
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}
}

func deployOK(t *testing.T, rig *testRig) {
	t.Helper()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", wantNew(), Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("status = %v (%s), want OK", res.Status, res.Detail)
	}
}

func wantNew() state.Desired { return *apiDesired("f") }

func TestDeployPrunesAfterSuccess(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
	// Shares b with demo-dev; its own surplus digest 9 is not this deploy's.
	seedInstance(t, rig.store, "other-dev", "9", "8", "7", "6", "b")
	run, log := usePruner(rig, "a", "b", "c", "d", "e", "f", "9")

	before, err := rig.store.History("demo-dev", 0)
	if err != nil {
		t.Fatal(err)
	}
	var eventsAtList int
	var deadline time.Time
	var hasDeadline bool
	run.before = func(ctx context.Context, c sys.Cmd) {
		if c.String() == imageListCmd {
			ev, err := rig.store.History("demo-dev", 0)
			if err != nil {
				t.Errorf("History: %v", err)
			}
			eventsAtList = len(ev)
			deadline, hasDeadline = ctx.Deadline()
		}
	}

	deployOK(t, rig)

	if got, want := removedImages(run), imageRefs("a", "c"); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v", got, want)
	}
	if eventsAtList != len(before)+1 {
		t.Fatalf("history had %d events at cleanup, want the deploy event too (%d)", eventsAtList, len(before)+1)
	}
	if left := time.Until(deadline); !hasDeadline || left <= time.Minute || left > 2*time.Minute {
		t.Fatalf("deadline in %v (set: %v), want about 2 minutes", left, hasDeadline)
	}
	for _, c := range run.Calls {
		if !slices.Contains(c.Env, "DOCKER_CONFIG=/var/lib/cvctl/docker") {
			t.Fatalf("%s runs without the host docker config: %v", c, c.Env)
		}
	}
	for _, ref := range imageRefs("a", "c") {
		if !strings.Contains(log.String(), "removed "+ref) {
			t.Fatalf("removal of %s not logged: %q", ref, log)
		}
	}
}

func TestPruneLogsRejectionsExceptImagesInUse(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "9", "a", "b", "c", "d", "e")
	run, log := usePruner(rig, "9", "a", "b", "c")
	rm := "docker image rm "
	run.On(rm+imageRefs("a")[0], sys.Result{})
	run.On(rm+imageRefs("b")[0], sys.Result{ExitCode: 1, Stderr: []byte(
		`Error response from daemon: conflict: unable to remove repository reference "x" (must force) - container 1234 is using its referenced image 5678`)})
	run.On(rm+imageRefs("c")[0], sys.Result{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon")})
	run.OnError(rm+imageRefs("9")[0], errors.New("exec: docker not found"))

	deployOK(t, rig)

	out := log.String()
	for _, want := range []string{
		"removed " + imageRefs("a")[0],
		imageRefs("c")[0] + ": Cannot connect to the Docker daemon",
		imageRefs("9")[0] + ": exec: docker not found",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("log lacks %q: %q", want, out)
		}
	}
	if strings.Contains(out, imageRefs("b")[0]) {
		t.Fatalf("an image in use must stay quiet: %q", out)
	}
}

func TestPruneFailureKeepsDeployOK(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
	run, log := usePruner(rig)
	run.OnError(imageListCmd, errors.New("docker daemon gone"))

	deployOK(t, rig)

	if !strings.Contains(log.String(), "demo-dev") || !strings.Contains(log.String(), "docker daemon gone") {
		t.Fatalf("failure not logged: %q", log)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.Status != state.StatusOK || st.Current.Hash() != apiDesired("f").Hash() {
		t.Fatalf("state changed by the cleanup: %+v", st)
	}
}

func TestPruneSurvivesCancelledDeployContext(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e", "f")
	run, _ := usePruner(rig, "a")
	var ctxErr error
	run.before = func(ctx context.Context, _ sys.Cmd) { ctxErr = errors.Join(ctxErr, ctx.Err()) }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rig.deployer.pruneImages(ctx, "demo-dev")

	if len(run.Calls) == 0 || ctxErr != nil {
		t.Fatalf("calls %d, ctx err %v: the cleanup must outlive the deploy context", len(run.Calls), ctxErr)
	}
}

func TestPruneTimeoutIsReported(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e", "f")
	run, _ := usePruner(rig, "a")
	run.OnErrorPrefix("docker image rm", context.Canceled)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rig.deployer.removeSurplusImages(ctx, "demo-dev"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the expired context", err)
	}
}

func TestPruneSkippedWhenInstanceUnreadable(t *testing.T) {
	cases := []struct {
		name   string
		damage func(t *testing.T, rig *testRig)
	}{
		{"corrupt state", func(t *testing.T, rig *testRig) {
			writeOther(t, rig, "state.json", "{not json")
		}},
		{"corrupt history", func(t *testing.T, rig *testRig) {
			writeOther(t, rig, "history.jsonl", "{not json\n")
		}},
		{"state read error", func(t *testing.T, rig *testRig) {
			path := filepath.Join(rig.tempDir, "state", "other-dev", "state.json")
			useStoreFS(rig, &errorFS{FS: rig.fs, failReadFile: map[string]error{path: os.ErrPermission}})
		}},
		{"history read error", func(t *testing.T, rig *testRig) {
			path := filepath.Join(rig.tempDir, "state", "other-dev", "history.jsonl")
			useStoreFS(rig, &errorFS{FS: rig.fs, failReadFile: map[string]error{path: os.ErrPermission}})
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rig := setupTestRig(t, "dev", "demo-dev")
			seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
			seedInstance(t, rig.store, "other-dev", "9", "8")
			run, log := usePruner(rig, "a", "b", "c")
			tc.damage(t, rig)

			deployOK(t, rig)

			if len(run.Calls) != 0 {
				t.Fatalf("docker ran with an unreadable instance: %v", run.Calls)
			}
			if !strings.Contains(log.String(), "other-dev") {
				t.Fatalf("unreadable instance not logged: %q", log)
			}
		})
	}
}

func writeOther(t *testing.T, rig *testRig, name, content string) {
	t.Helper()
	path := filepath.Join(rig.tempDir, "state", "other-dev", name)
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func useStoreFS(rig *testRig, fsys sys.FS) {
	rig.store = state.NewStore(fsys, filepath.Join(rig.tempDir, "state"))
	rig.deployer.Store = rig.store
}

func TestPruneSkippedWhenDeployRootUnreadable(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e", "f")
	run, log := usePruner(rig, "a")
	rig.deployer.DeployRoot = filepath.Join(rig.tempDir, "missing")

	rig.deployer.pruneImages(context.Background(), "demo-dev")

	if len(run.Calls) != 0 {
		t.Fatalf("docker ran without the full instance list: %v", run.Calls)
	}
	if log.Len() == 0 {
		t.Fatal("failure not logged")
	}
}

// A deployed directory with history but no state file yet is not in the
// store's list, and Store.Load would describe it as an empty healthy state.
func TestPruneKeepsHistoryOfInstanceWithoutState(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
	appendDeploys(t, rig.store, "new-dev", "a", "1", "2", "3", "4", "5")
	if err := os.MkdirAll(filepath.Join(rig.tempDir, "deploy", "new-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	run, log := usePruner(rig, "a", "b", "c")

	deployOK(t, rig)

	if got, want := removedImages(run), imageRefs("b", "c"); !slices.Equal(got, want) {
		t.Fatalf("removed %v, want %v (log %q)", got, want, log)
	}
}

func TestPruneNotRunOnFailedDeploy(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
	run, _ := usePruner(rig, "a", "b", "c")
	healthCalls := 0
	rig.deployer.Health = func(context.Context, string, string) error {
		healthCalls++
		if healthCalls == 1 {
			return errors.New("unhealthy")
		}
		return nil
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", wantNew(), Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("status = %v, want FAILED", res.Status)
	}
	if len(run.Calls) != 0 {
		t.Fatalf("cleanup ran on a failed deploy: %v", run.Calls)
	}
}

// With the event or the state unsaved, what is on disk no longer describes
// what is running.
func TestPruneNotRunWhenDeployEventNotSaved(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
	run, _ := usePruner(rig, "a", "b", "c")
	path := filepath.Join(rig.tempDir, "state", "demo-dev", "history.jsonl")
	useStoreFS(rig, &errorFS{FS: rig.fs, failWriteFile: map[string]error{path: errors.New("disk full")}})

	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", wantNew(), Options{Actor: "agent"}); err == nil {
		t.Fatal("expected the history error")
	}
	if len(run.Calls) != 0 {
		t.Fatalf("cleanup ran after an unsaved event: %v", run.Calls)
	}
}

func TestPruneNilPruner(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	seedInstance(t, rig.store, "demo-dev", "a", "b", "c", "d", "e")
	rig.deployer.Images = nil

	deployOK(t, rig)
}

func TestWireSetsPrunerOnDeployerRunner(t *testing.T) {
	run := &systest.Runner{}
	d := &Deployer{DeployRoot: "/srv/deploy", FS: systest.NewFS()}

	Wire(d, run)

	if d.Images == nil || d.Images.Run != sys.Runner(run) {
		t.Fatalf("Images = %+v, want a pruner over the deployer runner", d.Images)
	}
}
