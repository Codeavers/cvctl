package deploy

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/state"
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

const twoAppsCompose = `services:
  api:
    image: ghcr.io/org/project-api
    labels: { cvctl.role: app, cvctl.expose: "3000" }
  web:
    image: ghcr.io/org/project-web
    labels: { cvctl.role: app, cvctl.expose: "3001" }
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
	if info.AppURL != "https://web.example.com" {
		t.Fatalf("AppURL = %q", info.AppURL)
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
	if info.AppURL != "https://api.example.com" {
		t.Fatalf("AppURL = %q", info.AppURL)
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

func TestDeployInfoAppURLPrefersChangedService(t *testing.T) {
	rig, _ := twoAppsRig(t)
	cur := desiredOf("sha256:a1", "sha256:b0")
	info := rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{Current: &cur}, desiredOf("sha256:a1", "sha256:b1"), "info")
	if info.AppURL != "https://web.example.com" {
		t.Fatalf("changed web: AppURL = %q", info.AppURL)
	}
	same := desiredOf("sha256:a1", "sha256:b1")
	info = rig.deployer.deployInfo(context.Background(), "demo-dev", &state.State{Current: &same}, same, "info")
	if info.AppURL != "https://api.example.com" {
		t.Fatalf("nothing changed: AppURL = %q", info.AppURL)
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
	if len(info.Builds) != 0 || len(info.Changed) != 2 || !strings.HasPrefix(info.AppURL, "https://") {
		t.Fatalf("info = %+v", info)
	}
}
