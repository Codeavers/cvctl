package deploy

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

// netRunner logs docker network commands into the fake compose call list so
// the relative order against compose calls is observable.
type netRunner struct {
	mu          sync.Mutex
	log         *fakeCompose
	failCreate  bool
	failConnect bool
}

func (n *netRunner) Run(_ context.Context, c sys.Cmd) (sys.Result, error) {
	n.log.mu.Lock()
	defer n.log.mu.Unlock()
	cmd := c.String()
	n.log.calls = append(n.log.calls, "net:"+strings.TrimPrefix(cmd, "docker network "))
	if strings.HasPrefix(cmd, "docker network inspect") {
		return sys.Result{ExitCode: 1}, nil
	}
	if (n.failCreate && strings.HasPrefix(cmd, "docker network create")) || (n.failConnect && strings.HasPrefix(cmd, "docker network connect")) {
		return sys.Result{ExitCode: 1, Stderr: []byte("boom")}, nil
	}
	return sys.Result{}, nil
}

func indexOf(calls []string, want string) int {
	for i, c := range calls {
		if c == want {
			return i
		}
	}
	return -1
}

func TestDeployEnsuresThenConnectsEdgeNetwork(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Run = &netRunner{log: rig.compose}
	want := defaultDesired()
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	calls := rig.compose.calls
	iCreate := indexOf(calls, "net:create --label cvctl.managed=true demo-dev_edge")
	iData := -1
	for i, c := range calls {
		if strings.HasPrefix(c, "updata:") {
			iData = i
		}
	}
	iUp := indexOf(calls, "up")
	iConn := indexOf(calls, "net:connect demo-dev_edge cvctl-traefik")
	if iCreate < 0 || iData < iCreate || iUp < iData || iConn < iUp {
		t.Fatalf("order wrong (create=%d updata=%d up=%d connect=%d): %v", iCreate, iData, iUp, iConn, calls)
	}
}

func TestExecuteSwapConnectsEdgeNetwork(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Run = &netRunner{log: rig.compose}
	target := defaultDesired()
	if err := rig.deployer.executeSwap(context.Background(), "demo-dev", &target, map[string]string{}, false); err != nil {
		t.Fatal(err)
	}
	calls := rig.compose.calls
	iUp := indexOf(calls, "up")
	iConn := indexOf(calls, "net:connect demo-dev_edge cvctl-traefik")
	if iUp < 0 || iConn < iUp || indexOf(calls, "net:create --label cvctl.managed=true demo-dev_edge") > iUp {
		t.Fatalf("order wrong: %v", calls)
	}
}

func newWant() state.Desired {
	w := defaultDesired()
	w.InfraCommit = "2222222222222222222222222222222222222222"
	return w
}

func saveOK(t *testing.T, rig *testRig) state.Desired {
	t.Helper()
	cur := defaultDesired()
	cur.InfraCommit = "1111111111111111111111111111111111111111"
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &cur}); err != nil {
		t.Fatal(err)
	}
	return cur
}

func TestDeployEnsureNetworkFailureIsTransient(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Run = &netRunner{log: rig.compose, failCreate: true}
	cur := saveOK(t, rig)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", newWant(), Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := rig.store.Load("demo-dev")
	if res.Status != state.StatusFailed || st.FailedHash != "" || st.Current == nil || st.Current.InfraCommit != cur.InfraCommit {
		t.Fatalf("res=%+v state=%+v", res, st)
	}
	if rig.compose.upCount != 0 {
		t.Fatalf("up ran: %d", rig.compose.upCount)
	}
}

func TestDeployConnectFailureIsTransientNoRollback(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Run = &netRunner{log: rig.compose, failConnect: true}
	saveOK(t, rig)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", newWant(), Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	st, _ := rig.store.Load("demo-dev")
	if res.Status != state.StatusFailed || st.FailedHash != "" {
		t.Fatalf("res=%+v state=%+v", res, st)
	}
	if rig.compose.upCount != 1 {
		t.Fatalf("expected exactly one up (no rollback), got %d", rig.compose.upCount)
	}
}

func TestExecuteSwapConnectFailureReturnsError(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Run = &netRunner{log: rig.compose, failConnect: true}
	target := defaultDesired()
	err := rig.deployer.executeSwap(context.Background(), "demo-dev", &target, map[string]string{}, false)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("err=%v", err)
	}
}

func TestNilRunnerIsMisconfigured(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	rig.deployer.Run = nil
	if err := rig.deployer.connectTraefik(context.Background(), "demo-dev"); err == nil {
		t.Fatal("expected error")
	}
}
