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
	mu  sync.Mutex
	log *fakeCompose
}

func (n *netRunner) Run(_ context.Context, c sys.Cmd) (sys.Result, error) {
	n.log.mu.Lock()
	defer n.log.mu.Unlock()
	cmd := c.String()
	n.log.calls = append(n.log.calls, "net:"+strings.TrimPrefix(cmd, "docker network "))
	if strings.HasPrefix(cmd, "docker network inspect") {
		return sys.Result{ExitCode: 1}, nil
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
