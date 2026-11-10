package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func writeHostEnv(t *testing.T, dir, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, "host.env"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

// CODEA-49: after cvctl init switches host.env to edge, the agent's next
// deploy must render the route for websecure with TLS, without a restart.
func TestHostReloaderAppliesEdgeModeToDeployer(t *testing.T) {
	tmp := t.TempDir()
	cfgDir := filepath.Join(tmp, "etc")
	traefikDir := filepath.Join(tmp, "traefik")
	for _, d := range []string{cfgDir, traefikDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeHostEnv(t, cfgDir, "CVCTL_EDGE_MODE=behind-proxy\nCVCTL_REGISTRIES=ghcr.io/codeavers\n")

	fsys := sys.NewOSFS()
	hcfg, secrets, err := hostcfg.Load(fsys, cfgDir)
	if err != nil {
		t.Fatal(err)
	}
	run := &systest.Runner{}
	dep := &deploy.Deployer{TraefikDir: traefikDir, FS: fsys, Host: hcfg, Secrets: secrets}
	deploy.Wire(dep, run)

	logins := 0
	oldLogin := runAgentRegistryLogin
	t.Cleanup(func() { runAgentRegistryLogin = oldLogin })
	runAgentRegistryLogin = func(context.Context, sys.Runner, *hostcfg.Config, *hostcfg.Secrets, notify.Notifier, io.Writer) {
		logins++
	}

	var errOut, logOut bytes.Buffer
	reload := hostReloader(context.Background(), fsys, cfgDir, run, dep, &errOut, &logOut)

	h, _, _, err := reload()
	if err != nil {
		t.Fatal(err)
	}
	if h != hcfg {
		t.Fatal("unchanged host.env must keep the same config")
	}

	writeHostEnv(t, cfgDir, "CVCTL_EDGE_MODE=edge\nCVCTL_ACME_EMAIL=ops@example.com\nCVCTL_REGISTRIES=ghcr.io/codeavers\n")
	h, _, n, err := reload()
	if err != nil {
		t.Fatal(err)
	}
	if h.EdgeMode != "edge" || dep.Host != h || dep.Notify != n {
		t.Fatalf("deployer not updated: host=%+v", dep.Host)
	}
	if logins != 0 {
		t.Fatalf("registry login redone without a registry change: %d", logins)
	}

	p := &contract.Project{Apps: []contract.AppService{{Name: "web", ExposePort: 8080}}}
	dec := &instance.Decisions{Instance: "cute-prod", Domains: map[string]string{"web": "app.example.com"}}
	if err := dep.WriteRoute(p, dec, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(traefikDir, "cute-prod.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "websecure") || !strings.Contains(string(b), "certResolver") {
		t.Fatalf("route rendered for the old edge mode:\n%s", b)
	}

	if err := os.MkdirAll(filepath.Join(cfgDir, "secrets"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfgDir, "secrets", "registry_token"), []byte("new-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := reload(); err != nil {
		t.Fatal(err)
	}
	if logins != 1 {
		t.Fatalf("expected a registry login after the token changed, got %d", logins)
	}
}

func TestHostReloaderKeepsDeployerOnError(t *testing.T) {
	tmp := t.TempDir()
	writeHostEnv(t, tmp, "CVCTL_EDGE_MODE=behind-proxy\n")
	fsys := sys.NewOSFS()
	hcfg, secrets, err := hostcfg.Load(fsys, tmp)
	if err != nil {
		t.Fatal(err)
	}
	dep := &deploy.Deployer{FS: fsys, Host: hcfg, Secrets: secrets}
	reload := hostReloader(context.Background(), fsys, tmp, &systest.Runner{}, dep, io.Discard, io.Discard)

	writeHostEnv(t, tmp, "CVCTL_EDGE_MODE=sideways\n")
	if _, _, _, err := reload(); err == nil {
		t.Fatal("expected an error for an invalid host.env")
	}
	if dep.Host != hcfg {
		t.Fatal("a failed reload must not touch the deployer")
	}
}
