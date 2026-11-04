package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

type labelCall struct{ image, digest string }

type fakeLabels struct {
	mu     sync.Mutex
	calls  []labelCall
	labels map[string]map[string]string // by image
	wait   func(ctx context.Context)
	// barrier makes every read wait until n reads are in flight.
	barrier *sync.WaitGroup
}

func (f *fakeLabels) Labels(ctx context.Context, image, digest string) (map[string]string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, labelCall{image, digest})
	l := f.labels[image]
	f.mu.Unlock()
	if f.barrier != nil {
		f.barrier.Done()
		f.barrier.Wait()
	}
	if f.wait != nil {
		f.wait(ctx)
	}
	return l, nil
}

func (f *fakeLabels) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

const twoAppsCompose = `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }

services:
  api:
    image: ghcr.io/org/project-api
    labels: { cvctl.role: app, cvctl.expose: "3000" }
    deploy: { resources: { limits: { memory: 512m } } }
  web:
    image: ghcr.io/org/project-web
    labels: { cvctl.role: app, cvctl.expose: "3001" }
    deploy: { resources: { limits: { memory: 512m } } }
`

const twoAppsEnv = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_STRATEGY=recreate
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_DOMAIN_API=api.example.com
CVCTL_DOMAIN_WEB=web.example.com
CVCTL_DIGEST_API=sha256:a1
CVCTL_DIGEST_WEB=sha256:b1
`

var bothLinks = []notify.Link{
	{Service: "api", URL: "https://api.example.com"},
	{Service: "web", URL: "https://web.example.com"},
}

func twoAppsRig(t *testing.T) (*testRig, *fakeLabels) {
	t.Helper()
	rig := setupTestRig(t, "dev", "demo-dev")
	if err := os.WriteFile(filepath.Join(rig.infraDir, "compose.yml"), []byte(twoAppsCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.infraDir, "env.spec.yml"), []byte("vars: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rig.instDir, "cvctl.env"), []byte(twoAppsEnv), 0o644); err != nil {
		t.Fatal(err)
	}
	fl := &fakeLabels{labels: map[string]map[string]string{}}
	rig.deployer.Labels = fl
	rig.deployer.Allowed = []string{"ghcr.io/org/"}
	return rig, fl
}

func desiredOf(api, web string) state.Desired {
	return state.Desired{InfraCommit: "c1", Digests: map[string]string{"api": api, "web": web}}
}

func TestDeployInfoChangedAgainstCurrent(t *testing.T) {
	rig, fl := twoAppsRig(t)
	fl.labels["ghcr.io/org/project-web"] = map[string]string{
		"cvctl.repo": "https://github.com/acme/app", "cvctl.commit": "abc", "cvctl.pr": "7",
		"cvctl.title": "Fix it", "cvctl.author": "ana", "org.opencontainers.image.source": "ignored",
	}
	cur := desiredOf("sha256:a1", "sha256:b0")
	st := &state.State{Current: &cur}
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", st, desiredOf("sha256:a1", "sha256:b1"), "info")

	if !reflect.DeepEqual(info.Changed, []string{"web"}) {
		t.Fatalf("Changed = %v", info.Changed)
	}
	want := []notify.Build{{Service: "web", Repo: "https://github.com/acme/app", Commit: "abc", PR: "7", Title: "Fix it", Author: "ana"}}
	if !reflect.DeepEqual(info.Builds, want) {
		t.Fatalf("Builds = %+v", info.Builds)
	}
	if !reflect.DeepEqual(info.Links, bothLinks) {
		t.Fatalf("Links = %+v", info.Links)
	}
	if info.Logs != "" {
		t.Fatalf("Logs = %q for info level", info.Logs)
	}
	if fl.callCount() != 1 || fl.calls[0] != (labelCall{"ghcr.io/org/project-web", "sha256:b1"}) {
		t.Fatalf("calls = %+v", fl.calls)
	}
}

func TestDeployInfoNilCurrentChangesAllSorted(t *testing.T) {
	rig, fl := twoAppsRig(t)
	fl.labels["ghcr.io/org/project-api"] = map[string]string{"cvctl.commit": "c"}
	fl.labels["ghcr.io/org/project-web"] = map[string]string{"cvctl.commit": "d"}
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "error")

	if !reflect.DeepEqual(info.Changed, []string{"api", "web"}) {
		t.Fatalf("Changed = %v", info.Changed)
	}
	if len(info.Builds) != 2 || info.Builds[0].Service != "api" || info.Builds[1].Service != "web" {
		t.Fatalf("Builds = %+v", info.Builds)
	}
	if info.Logs != "sudo cvctl history demo-dev" {
		t.Fatalf("Logs = %q", info.Logs)
	}
	if !reflect.DeepEqual(info.Links, bothLinks) {
		t.Fatalf("Links = %+v", info.Links)
	}
}

func TestDeployInfoImageWithoutBuildLabelsHasNoBuild(t *testing.T) {
	rig, fl := twoAppsRig(t)
	fl.labels["ghcr.io/org/project-api"] = map[string]string{"org.opencontainers.image.source": "https://x"}
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if len(info.Builds) != 0 || len(info.Changed) != 2 {
		t.Fatalf("info = %+v", info)
	}
}

func TestDeployInfoDisallowedImageNotRead(t *testing.T) {
	rig, fl := twoAppsRig(t)
	rig.deployer.Allowed = []string{"registry.other.io/"}
	fl.labels["ghcr.io/org/project-api"] = map[string]string{"cvctl.commit": "c"}
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if fl.callCount() != 0 || len(info.Builds) != 0 || len(info.Changed) != 2 {
		t.Fatalf("calls=%d info=%+v", fl.callCount(), info)
	}
}

func TestDeployInfoNilAllowedSkipsCheck(t *testing.T) {
	rig, fl := twoAppsRig(t)
	rig.deployer.Allowed = nil
	rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if fl.callCount() != 2 {
		t.Fatalf("calls = %d", fl.callCount())
	}
}

func TestDeployInfoSlowReaderCutAtCeiling(t *testing.T) {
	rig, fl := twoAppsRig(t)
	old := labelTimeout
	labelTimeout = 50 * time.Millisecond
	t.Cleanup(func() { labelTimeout = old })
	fl.wait = func(ctx context.Context) { <-ctx.Done() }

	start := time.Now()
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("took %v", el)
	}
	if len(info.Builds) != 0 || len(info.Changed) != 2 {
		t.Fatalf("info = %+v", info)
	}
}

func TestDeployInfoIgnoresReaderThatNeverReturns(t *testing.T) {
	rig, fl := twoAppsRig(t)
	old := labelTimeout
	labelTimeout = 50 * time.Millisecond
	t.Cleanup(func() { labelTimeout = old })
	block := make(chan struct{})
	t.Cleanup(func() { close(block) })
	fl.wait = func(context.Context) { <-block }

	done := make(chan struct{})
	go func() {
		rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("deployInfo blocked on a reader that ignores its context")
	}
}

func TestDeployInfoReadsInParallel(t *testing.T) {
	rig, fl := twoAppsRig(t)
	fl.barrier = &sync.WaitGroup{}
	fl.barrier.Add(2)
	done := make(chan struct{})
	go func() {
		rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reads are not concurrent: barrier never released")
	}
}

func TestDeployInfoCancelledParentStillReads(t *testing.T) {
	rig, fl := twoAppsRig(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rig.deployer.deployInfo(ctx, "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if fl.callCount() != 2 {
		t.Fatalf("calls = %d", fl.callCount())
	}
}

func TestDeployInfoNoDigestsNoReads(t *testing.T) {
	rig, fl := twoAppsRig(t)
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, state.Desired{InfraCommit: "c1"}, "info")
	if fl.callCount() != 0 || len(info.Changed) != 0 || len(info.Builds) != 0 {
		t.Fatalf("calls=%d info=%+v", fl.callCount(), info)
	}
}

func TestDeployInfoLinksEveryAppWithADomain(t *testing.T) {
	rig, _ := twoAppsRig(t)
	cur := desiredOf("sha256:a1", "sha256:b0")
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{Current: &cur}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if !reflect.DeepEqual(info.Links, bothLinks) {
		t.Fatalf("changed web: Links = %+v", info.Links)
	}
	same := desiredOf("sha256:a1", "sha256:b1")
	info = rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{Current: &same}, same, "info")
	if !reflect.DeepEqual(info.Links, bothLinks) {
		t.Fatalf("nothing changed: Links = %+v", info.Links)
	}
}

func TestDeployInfoUnreadableContractKeepsChanged(t *testing.T) {
	rig, fl := twoAppsRig(t)
	if err := os.WriteFile(filepath.Join(rig.infraDir, "compose.yml"), []byte("services: [broken"), 0o644); err != nil {
		t.Fatal(err)
	}
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if !reflect.DeepEqual(info.Changed, []string{"api", "web"}) || len(info.Builds) != 0 || fl.callCount() != 0 {
		t.Fatalf("info=%+v calls=%d", info, fl.callCount())
	}
}

func TestDeployInfoMissingEnvFileNeverPanics(t *testing.T) {
	rig, _ := twoAppsRig(t)
	_ = os.Remove(filepath.Join(rig.instDir, "cvctl.env"))
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if info == nil || len(info.Changed) != 2 || len(info.Builds) != 0 {
		t.Fatalf("info = %+v", info)
	}
}

func TestDeployInfoNilLabelsReader(t *testing.T) {
	rig, _ := twoAppsRig(t)
	rig.deployer.Labels = nil
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if len(info.Builds) != 0 || len(info.Changed) != 2 || len(info.Links) != 2 {
		t.Fatalf("info = %+v", info)
	}
}

func TestDefaultLabelTimeout(t *testing.T) {
	if labelTimeout != 5*time.Second {
		t.Fatalf("labelTimeout = %s, want 5s", labelTimeout)
	}
}

func TestDeployOKNoticeCarriesChangedBuilds(t *testing.T) {
	rig, fl := twoAppsRig(t)
	fl.labels["ghcr.io/org/project-web"] = map[string]string{"cvctl.commit": "abc1234", "cvctl.pr": "7"}
	digest := func(c string) string { return "sha256:" + strings.Repeat(c, 64) }
	cur := desiredOf(digest("a"), digest("b"))
	cur.InfraCommit = strings.Repeat("0", 40)
	if err := rig.store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, Current: &cur}); err != nil {
		t.Fatal(err)
	}

	want := desiredOf(digest("a"), digest("c"))
	want.InfraCommit = strings.Repeat("1", 40)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("result = %+v", res)
	}

	m := lastMessage(t, rig.notifier)
	if !reflect.DeepEqual(m.Deploy.Changed, []string{"web"}) {
		t.Fatalf("Changed = %v", m.Deploy.Changed)
	}
	wantBuilds := []notify.Build{{Service: "web", Commit: "abc1234", PR: "7"}}
	if !reflect.DeepEqual(m.Deploy.Builds, wantBuilds) {
		t.Fatalf("Builds = %+v", m.Deploy.Builds)
	}
}

func TestPostDeployHookFailureReadsLabelsOnce(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	fl := &fakeLabels{labels: map[string]map[string]string{"ghcr.io/org/project-api": {"cvctl.pr": "7"}}}
	rig.deployer.Labels = fl
	rig.compose.failHook["api:node dist/warmup.js"] = struct {
		res sys.Result
		err error
	}{res: sys.Result{ExitCode: 1, Stderr: []byte("boom")}, err: errors.New("exit 1")}

	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{}); err != nil {
		t.Fatal(err)
	}

	if n := fl.callCount(); n != 1 {
		t.Fatalf("label reads = %d, want 1", n)
	}
	rig.notifier.mu.Lock()
	defer rig.notifier.mu.Unlock()
	if len(rig.notifier.messages) != 2 {
		t.Fatalf("messages = %+v", rig.notifier.messages)
	}
	warn, ok := rig.notifier.messages[0], rig.notifier.messages[1]
	if warn.Level != "warn" || warn.Deploy.Logs != "sudo cvctl history demo-dev" {
		t.Fatalf("warn notice = %+v, %+v", warn, warn.Deploy)
	}
	if ok.Level != "info" || ok.Deploy.Logs != "" {
		t.Fatalf("OK notice = %+v, %+v", ok, ok.Deploy)
	}
	same := *warn.Deploy
	same.Logs = ""
	if !reflect.DeepEqual(same, *ok.Deploy) {
		t.Fatalf("notices differ beyond Logs: %+v vs %+v", warn.Deploy, ok.Deploy)
	}
	if len(ok.Deploy.Builds) != 1 {
		t.Fatalf("Builds = %+v", ok.Deploy.Builds)
	}
}

func TestDeployInfoLinksIgnoreExposedDataServices(t *testing.T) {
	const compose = `services:
  api:
    image: ghcr.io/org/project-api
    labels: { cvctl.role: worker }
  assets:
    labels: { cvctl.kind: s3, cvctl.expose: "9000" }
`
	const env = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_INFRA_PATH=.
CVCTL_DOMAIN_ASSETS=assets.example.com
`
	rig, _ := twoAppsRig(t)
	want := state.Desired{InfraCommit: "c1", Digests: map[string]string{"api": "sha256:a1"}}

	write := func(compose, env string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(rig.infraDir, "compose.yml"), []byte(compose), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(rig.instDir, "cvctl.env"), []byte(env), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(compose, env)
	if p, dec := rig.deployer.loadForNotice("demo-dev"); p == nil || dec.Domains["assets"] == "" {
		t.Fatalf("fixture must give the data service a domain: %+v", dec)
	}
	if info := rig.deployer.deployInfo(context.Background(), "demo-dev", nil, want, "info"); len(info.Links) != 0 {
		t.Fatalf("only a data service has a domain: Links = %+v", info.Links)
	}

	// The data service sorts first, so a list of all domains would include it.
	write(strings.Replace(compose, "cvctl.role: worker", `cvctl.role: app, cvctl.expose: "3000"`, 1),
		env+"CVCTL_DOMAIN_API=zzz.example.com\n")
	same := state.State{Current: &want}
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &same, want, "info")
	if !reflect.DeepEqual(info.Links, []notify.Link{{Service: "api", URL: "https://zzz.example.com"}}) {
		t.Fatalf("Links = %+v", info.Links)
	}
}
