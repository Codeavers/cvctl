package agent_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/agent"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

// An OCI instance has a plain infra/ directory (the extracted artifact), which
// the default loader reads exactly like a git checkout.
func TestAgentLoadsOCIInstance(t *testing.T) {
	root := t.TempDir()
	deployRoot, stateRoot := filepath.Join(root, "deploy"), filepath.Join(root, "state")
	infra := filepath.Join(deployRoot, "demo-dev", "infra")
	if err := os.MkdirAll(infra, 0o755); err != nil {
		t.Fatal(err)
	}
	compose := `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }
services:
  api:
    image: ghcr.io/org/api
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy: { resources: { limits: { memory: 512m } } }
`
	files := map[string]string{
		filepath.Join(infra, "compose.yml"):  compose,
		filepath.Join(infra, "env.spec.yml"): "vars: {}\n",
		filepath.Join(deployRoot, "demo-dev", "cvctl.env"): "CVCTL_INSTANCE=demo-dev\nCVCTL_ENV=dev\nCVCTL_STRATEGY=recreate\n" +
			"CVCTL_TRACK_TAG=develop\nCVCTL_INFRA_PATH=.\nCVCTL_INFRA_SOURCE=oci\nCVCTL_INFRA_REF=ghcr.io/org/demo-infra\n" +
			"CVCTL_DOMAIN_API=api.example.com\n",
	}
	for p, c := range files {
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store := state.NewStore(sys.NewOSFS(), stateRoot)
	if err := store.Save(&state.State{Instance: "demo-dev", Status: state.StatusOK, UpdatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}

	dg := "sha256:" + strings.Repeat("a", 64)
	var seen *instance.Decisions
	var deployed state.Desired
	dep := &fakeDeployer{onDeploy: func(_ context.Context, _ string, want state.Desired) (deploy.Result, error) {
		deployed = want
		return deploy.Result{Status: state.StatusOK}, nil
	}}
	var out bytes.Buffer
	err := agent.Run(context.Background(), agent.Config{
		Interval:   time.Minute,
		Store:      store,
		Deployer:   dep,
		DeployRoot: deployRoot,
		StateRoot:  stateRoot,
		Sources: func(d *instance.Decisions) deploy.Source {
			seen = d
			return &fakeSource{desired: state.Desired{InfraDigest: dg}}
		},
		Notify: &fakeNotifier{},
		Out:    &out,
		Once:   true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen == nil || !seen.IsOCI() || seen.InfraRef != "ghcr.io/org/demo-infra" {
		t.Fatalf("decisions = %+v\nlog: %s", seen, out.String())
	}
	if deployed.InfraDigest != dg {
		t.Fatalf("deployed %+v\nlog: %s", deployed, out.String())
	}
}
