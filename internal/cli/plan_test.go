package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/kinds"
)

const planCompose = `
services:
  web:
    image: ghcr.io/codeavers/web:develop@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    environment:
      API_TOKEN: s3cr3t-value
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
  media:
    labels:
      cvctl.kind: s3
      cvctl.env: MEDIA
x-cvctl:
  health:
    service: web
    http: /healthz
`

// setupPlanRig installs planCompose over the fixture contract and puts a
// recognisable secret in both the compose environment and cvctl.env.
func setupPlanRig(t *testing.T, inst string) (string, *OpsEnv) {
	t.Helper()
	tmp, env := setupOpsFixture(t, inst)
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	infra := filepath.Join(tmp, inst, "infra")
	if err := os.WriteFile(filepath.Join(infra, "compose.yml"), []byte(planCompose), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(infra, "env.spec.yml"), []byte("vars: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := instance.Decisions{
		Instance: inst, Env: "dev", InfraBranch: "develop", TrackTag: "develop", InfraPath: ".",
		Strategy: "recreate",
		Domains:  map[string]string{"web": "app.example.com"},
		Digests:  map[string]string{"web": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
		Services: map[string]instance.ServiceDecision{
			"media": {Mode: "local", Creds: kinds.Credentials{User: "s3cr3t-value", Password: "s3cr3t-value", DB: "media"}},
		},
		Backup: instance.BackupDecision{AccessKey: "s3cr3t-value", SecretKey: "s3cr3t-value"},
	}
	if err := os.WriteFile(filepath.Join(tmp, inst, "cvctl.env"), instance.FormatEnv(d.Entries()), 0o640); err != nil {
		t.Fatal(err)
	}
	return tmp, env
}

func TestPlanDescribesInstance(t *testing.T) {
	setupPlanRig(t, "myapp-dev")
	code, out, errb := run(t, "plan", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var resp struct {
		Data planReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("json: %v (%s)", err, out)
	}
	if resp.Data.Schema != 1 {
		t.Errorf("schema = %d", resp.Data.Schema)
	}
	in, ok := resp.Data.Instances["myapp-dev"]
	if !ok || in.Error != "" {
		t.Fatalf("instance = %+v", in)
	}
	if in.Env != "dev" || in.Strategy != "recreate" {
		t.Errorf("env=%q strategy=%q", in.Env, in.Strategy)
	}
	web := in.Apps["web"]
	if web.Image != "ghcr.io/codeavers/web:develop" || web.Expose != 3000 || web.Domain != "app.example.com" {
		t.Errorf("web = %+v", web)
	}
	media := in.Data["media"]
	if media.Kind != "s3" || media.Mode != "local" || media.Backup != "none" {
		t.Errorf("media = %+v", media)
	}
}

func TestPlanHasNoSecrets(t *testing.T) {
	setupPlanRig(t, "myapp-dev")
	for _, args := range [][]string{{"plan", "--json"}, {"plan"}} {
		code, out, errb := run(t, args...)
		if code != 0 {
			t.Fatalf("code %d: out=%s err=%s", code, out, errb)
		}
		if strings.Contains(out, "s3cr3t-value") || strings.Contains(errb, "s3cr3t-value") {
			t.Fatalf("plan %v leaks a secret: %s", args, out)
		}
		if !strings.Contains(out, "myapp-dev") {
			t.Fatalf("plan %v lacks the instance: %s", args, out)
		}
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	setupPlanRig(t, "myapp-dev")
	_, a, _ := run(t, "plan", "--json")
	_, b, _ := run(t, "plan", "--json")
	if a == "" || a != b {
		t.Fatalf("two runs differ:\n%s\n%s", a, b)
	}
}

func TestPlanUnreadableInstance(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	if err := os.MkdirAll(filepath.Join(tmp, "broken-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "broken-dev", "cvctl.env"), []byte("CVCTL_INSTANCE=broken-dev\nCVCTL_ENV=dev\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, out, errb := run(t, "plan", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var resp struct {
		Data planReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Data.Instances["broken-dev"].Error == "" {
		t.Errorf("broken-dev should carry an error: %s", out)
	}
	if g := resp.Data.Instances["myapp-dev"]; g.Error != "" || g.Apps["web"].Expose != 3000 {
		t.Errorf("myapp-dev incomplete: %+v", g)
	}
}
