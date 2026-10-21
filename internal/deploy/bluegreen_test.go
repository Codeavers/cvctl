package deploy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
)

var _ SwapAPI = (*compose.Project)(nil)

// swapCompose adds the blue-green primitives to boundCompose. Every call goes
// into fake.calls, so its order against Up, health and route writes shows.
type swapCompose struct {
	*boundCompose
	st *swapState
}

type swapState struct {
	containers map[string][]compose.Container
	routePath  string
	routeAt    map[string]string
	nextID     int
	failCheck  error
	failScale  error
	failStop   error
	edgeIP     string
	grace      time.Duration
}

func newSwapState(routePath string) *swapState {
	return &swapState{
		containers: map[string][]compose.Container{},
		routePath:  routePath,
		routeAt:    map[string]string{},
		edgeIP:     "172.20.0.9",
	}
}

func (s *swapCompose) rec(call string) {
	s.fake.mu.Lock()
	defer s.fake.mu.Unlock()
	s.fake.calls = append(s.fake.calls, call)
}

func (s *swapCompose) snapshot(at string) {
	b, _ := os.ReadFile(s.st.routePath)
	s.st.routeAt[at] = string(b)
}

func (s *swapCompose) UpServices(_ context.Context, services []string, orphans bool) error {
	s.rec(fmt.Sprintf("upservices:%s:orphans=%v", strings.Join(services, ","), orphans))
	return nil
}

func (s *swapCompose) UpServicesNoDeps(_ context.Context, services []string, orphans bool) error {
	s.rec(fmt.Sprintf("upservices-nodeps:%s:orphans=%v", strings.Join(services, ","), orphans))
	return nil
}

func (s *swapCompose) ScaleNoRecreate(_ context.Context, svc string, n int) error {
	s.rec(fmt.Sprintf("scale:%s=%d", svc, n))
	if s.st.failScale != nil {
		return s.st.failScale
	}
	s.st.nextID++
	s.st.containers[svc] = append(s.st.containers[svc], compose.Container{
		ID: fmt.Sprintf("new%d", s.st.nextID), Service: svc, State: "running",
		Created: time.Date(2026, 10, 21, 12, 0, s.st.nextID, 0, time.UTC),
	})
	return nil
}

func (s *swapCompose) Containers(_ context.Context, svc string) ([]compose.Container, error) {
	s.rec("containers:" + svc)
	return slices.Clone(s.st.containers[svc]), nil
}

func (s *swapCompose) IP(_ context.Context, id, network string) (string, error) {
	s.rec(fmt.Sprintf("ip:%s:%s", id, network))
	return s.st.edgeIP, nil
}

func (s *swapCompose) CheckContainer(_ context.Context, h contract.HealthSpec, port int, _ *http.Client, id string, httpCheck bool) error {
	s.rec(fmt.Sprintf("check:%s:%s:port=%d:http=%v", h.Service, id, port, httpCheck))
	s.snapshot("check")
	return s.st.failCheck
}

func (s *swapCompose) StopRemove(_ context.Context, id string, grace time.Duration) error {
	s.rec("stopremove:" + id)
	s.snapshot("stopremove")
	s.st.grace = grace
	if s.st.failStop != nil {
		return s.st.failStop
	}
	s.drop(id)
	return nil
}

func (s *swapCompose) RemoveForce(_ context.Context, id string) error {
	s.rec("rmforce:" + id)
	s.drop(id)
	return nil
}

func (s *swapCompose) drop(id string) {
	for svc, cs := range s.st.containers {
		s.st.containers[svc] = slices.DeleteFunc(cs, func(c compose.Container) bool { return c.ID == id })
	}
}

func blueGreenRig(t *testing.T) (*testRig, *swapState) {
	t.Helper()
	rig := setupTestRig(t, "dev", "demo-dev")
	envPath := filepath.Join(rig.instDir, "cvctl.env")
	b, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatal(err)
	}
	b = []byte(strings.Replace(string(b), "CVCTL_STRATEGY=recreate", "CVCTL_STRATEGY=blue-green", 1))
	if err := os.WriteFile(envPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	ss := newSwapState(filepath.Join(rig.tempDir, "traefik", "demo-dev.yml"))
	rig.deployer.Compose = func(inst, override, envFile string) ComposeAPI {
		return &swapCompose{boundCompose: &boundCompose{fake: rig.compose, override: override, envFile: envFile}, st: ss}
	}
	rig.deployer.Sleep = func(context.Context, time.Duration) error {
		rig.compose.mu.Lock()
		rig.compose.calls = append(rig.compose.calls, "settle")
		rig.compose.mu.Unlock()
		b, _ := os.ReadFile(ss.routePath)
		ss.routeAt["settle"] = string(b)
		return nil
	}
	return rig, ss
}

func oldContainer(id string, sec int) compose.Container {
	return compose.Container{ID: id, Service: "api", State: "running", Created: time.Date(2026, 10, 20, 12, 0, sec, 0, time.UTC)}
}

func seedBGCurrent(t *testing.T, rig *testRig) {
	t.Helper()
	old := state.Desired{
		InfraCommit: "0000000000000000000000000000000000000000",
		Digests:     map[string]string{"api": "sha256:0000000000000000000000000000000000000000000000000000000000000000"},
	}
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &old}); err != nil {
		t.Fatal(err)
	}
}

func assertOrder(t *testing.T, calls []string, want ...string) {
	t.Helper()
	pos := -1
	for _, w := range want {
		i := -1
		for j := pos + 1; j < len(calls); j++ {
			if calls[j] == w {
				i = j
				break
			}
		}
		if i < 0 {
			t.Fatalf("call %q missing or out of order after index %d in %v", w, pos, calls)
		}
		pos = i
	}
}

func TestBlueGreenSwapOrder(t *testing.T) {
	rig, ss := blueGreenRig(t)
	seedBGCurrent(t, rig)
	ss.containers["api"] = []compose.Container{oldContainer("old1", 0)}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}

	assertOrder(t, rig.compose.calls,
		"upservices-nodeps:main-db:orphans=true",
		"containers:api",
		"scale:api=2",
		"containers:api",
		"check:api:new1:port=3000:http=true",
		"ip:new1:demo-dev_edge",
		"settle",
		"stopremove:old1",
		"up",
	)

	alias := "http://demo-dev--api:3000"
	if !strings.Contains(ss.routeAt["check"], alias) {
		t.Fatalf("route before pin must be the alias form:\n%s", ss.routeAt["check"])
	}
	for _, at := range []string{"settle", "stopremove"} {
		r := ss.routeAt[at]
		if !strings.Contains(r, "http://172.20.0.9:3000") || strings.Contains(r, alias) {
			t.Fatalf("route at %s must be pinned to the new container:\n%s", at, r)
		}
	}
	final, _ := os.ReadFile(ss.routePath)
	if !strings.Contains(string(final), alias) || strings.Contains(string(final), "172.20.0.9") {
		t.Fatalf("route must be back to the alias form:\n%s", final)
	}
	if ss.grace != defaultStopGrace {
		t.Fatalf("grace = %v, want %v", ss.grace, defaultStopGrace)
	}
	if len(ss.containers["api"]) != 1 || ss.containers["api"][0].ID != "new1" {
		t.Fatalf("only the new container must remain: %v", ss.containers["api"])
	}
	hist, err := rig.store.History("demo-dev", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) == 0 || !slices.Equal(hist[len(hist)-1].BlueGreen, []string{"api"}) {
		t.Fatalf("history must record the swapped service: %+v", hist)
	}
}

func TestBlueGreenUnhealthyNewContainerRollsBack(t *testing.T) {
	rig, ss := blueGreenRig(t)
	seedBGCurrent(t, rig)
	ss.containers["api"] = []compose.Container{oldContainer("old1", 0)}
	ss.failCheck = errors.New("unhealthy")

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusFailed {
		t.Fatalf("status = %v", res.Status)
	}
	calls := rig.compose.calls
	assertOrder(t, calls, "scale:api=2", "check:api:new1:port=3000:http=true", "rmforce:new1", "up")
	for _, c := range calls {
		if strings.HasPrefix(c, "stopremove:") || c == "settle" || strings.HasPrefix(c, "ip:") {
			t.Fatalf("old container must stay and the route must not be pinned, saw %q in %v", c, calls)
		}
	}
	if rig.compose.upCount != 1 {
		t.Fatalf("rollback must run exactly one Up, got %d", rig.compose.upCount)
	}
	r, _ := os.ReadFile(ss.routePath)
	if strings.Contains(string(r), "172.20.0.9") {
		t.Fatalf("route was pinned:\n%s", r)
	}
	if len(ss.containers["api"]) != 1 || ss.containers["api"][0].ID != "old1" {
		t.Fatalf("old container must survive: %v", ss.containers["api"])
	}
}

func TestBlueGreenScaleFailureRemovesNew(t *testing.T) {
	rig, ss := blueGreenRig(t)
	seedBGCurrent(t, rig)
	ss.containers["api"] = []compose.Container{oldContainer("old1", 0)}
	ss.failScale = errors.New("scale boom")

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusFailed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "stopremove:") || c == "settle" {
			t.Fatalf("unexpected %q", c)
		}
	}
}

func TestBlueGreenFirstDeployUsesPlainUp(t *testing.T) {
	rig, _ := blueGreenRig(t)

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	assertOrder(t, rig.compose.calls, "upservices-nodeps:main-db:orphans=true", "containers:api", "upservices:api:orphans=false", "up")
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "scale:") || strings.HasPrefix(c, "stopremove:") {
			t.Fatalf("first deploy must not swap, saw %q", c)
		}
	}
	hist, _ := rig.store.History("demo-dev", 10)
	if len(hist) == 0 || len(hist[len(hist)-1].BlueGreen) != 0 {
		t.Fatalf("nothing was swapped: %+v", hist)
	}
}

func TestBlueGreenLeftoverRemovedBeforeSwap(t *testing.T) {
	rig, ss := blueGreenRig(t)
	seedBGCurrent(t, rig)
	ss.containers["api"] = []compose.Container{oldContainer("newer", 30), oldContainer("oldest", 0)}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	assertOrder(t, rig.compose.calls, "containers:api", "rmforce:newer", "scale:api=2", "stopremove:oldest")
	for _, c := range rig.compose.calls {
		if c == "rmforce:oldest" || c == "stopremove:newer" {
			t.Fatalf("the oldest container is the one to keep: %q", c)
		}
	}
}

func TestBlueGreenStopFailureRestoresAlias(t *testing.T) {
	rig, ss := blueGreenRig(t)
	seedBGCurrent(t, rig)
	ss.containers["api"] = []compose.Container{oldContainer("old1", 0)}
	ss.failStop = errors.New("stop boom")

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusFailed {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	r, _ := os.ReadFile(ss.routePath)
	if strings.Contains(string(r), "172.20.0.9") {
		t.Fatalf("route must be back to the alias form:\n%s", r)
	}
}

func TestBlueGreenWorkerOnlyUsesPlainUp(t *testing.T) {
	rig, _ := blueGreenRig(t)
	p := &contract.Project{Apps: []contract.AppService{{Name: "worker", Role: "worker"}}}
	c := rig.deployer.Compose("demo-dev", "o", "e")

	swapped, err := rig.deployer.bringUp(context.Background(), "demo-dev", p, &instance.Decisions{Strategy: "blue-green"}, c)
	if err != nil || len(swapped) != 0 {
		t.Fatalf("swapped=%v err=%v", swapped, err)
	}
	if !slices.Equal(rig.compose.calls, []string{"up", "up:--remove-orphans"}) {
		t.Fatalf("calls = %v", rig.compose.calls)
	}
}

func TestRecreateUsesPlainUp(t *testing.T) {
	rig, ss := blueGreenRig(t)
	ss.containers["api"] = []compose.Container{oldContainer("old1", 0)}
	envPath := filepath.Join(rig.instDir, "cvctl.env")
	b, _ := os.ReadFile(envPath)
	_ = os.WriteFile(envPath, []byte(strings.Replace(string(b), "blue-green", "recreate", 1)), 0o644)

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "upservices:") || strings.HasPrefix(c, "scale:") || strings.HasPrefix(c, "containers:") {
			t.Fatalf("recreate must only run Up, saw %q", c)
		}
	}
	if rig.compose.upCount != 1 {
		t.Fatalf("upCount = %d", rig.compose.upCount)
	}
}

func TestBlueGreenNeedsSwapAPI(t *testing.T) {
	rig, _ := blueGreenRig(t)
	p := &contract.Project{Apps: []contract.AppService{{Name: "api", Role: "app", ExposePort: 3000}}}
	c := &boundCompose{fake: rig.compose}

	_, err := rig.deployer.bringUp(context.Background(), "demo-dev", p, &instance.Decisions{Strategy: "blue-green"}, c)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_DEPLOY_FAILED" {
		t.Fatalf("err = %v", err)
	}
	if len(rig.compose.calls) != 0 {
		t.Fatalf("nothing must run: %v", rig.compose.calls)
	}
}

func TestBlueGreenWorkerDependingOnExposedKeepsAPISwapped(t *testing.T) {
	rig, ss := blueGreenRig(t)
	seedBGCurrent(t, rig)
	ss.containers["api"] = []compose.Container{oldContainer("old1", 0)}
	cp := filepath.Join(rig.infraDir, "compose.yml")
	y, _ := os.ReadFile(cp)
	y = append(y, []byte(`  worker:
    image: ghcr.io/org/project-worker
    depends_on: [api]
    labels:
      cvctl.role: worker
    deploy: { resources: { limits: { memory: 256m } } }
`)...)
	const dg = "sha256:0000000000000000000000000000000000000000000000000000000000000002"
	envPath := filepath.Join(rig.instDir, "cvctl.env")
	eb, _ := os.ReadFile(envPath)
	if err := os.WriteFile(envPath, append(eb, []byte("CVCTL_DIGEST_WORKER="+dg+"\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	want := defaultDesired()
	want.Digests["worker"] = dg
	if err := os.WriteFile(cp, y, 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	assertOrder(t, rig.compose.calls, "upservices-nodeps:main-db,worker:orphans=true", "scale:api=2", "stopremove:old1")
}
