package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/upgrade"
	"github.com/codeavers/cvctl/kinds"
)

const planCompose = `
services:
  web:
    image: ghcr.io/codeavers/web:develop@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
    environment:
      API_TOKEN: s3cr3t-value
    deploy:
      resources:
        limits:
          memory: 256M
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
			"media": {Mode: "local", Creds: kinds.Credentials{
				User: "mediauser", Password: "s3cr3t-value", DB: "media",
				AppUser: "mediaapp", AppPassword: "s3cr3t-value",
			}},
		},
		Backup: instance.BackupDecision{AccessKey: "s3cr3t-value", SecretKey: "s3cr3t-value"},
	}
	if err := os.WriteFile(filepath.Join(tmp, inst, "cvctl.env"), instance.FormatEnv(d.Entries()), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, inst, "app.env"), []byte("API_TOKEN=s3cr3t-value\n"), 0o640); err != nil {
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

// planDoc runs plan --json and returns the document that upgrade compares.
func planDoc(t *testing.T) (json.RawMessage, planReport) {
	t.Helper()
	code, out, errb := run(t, "plan", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var resp struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("json: %v (%s)", err, out)
	}
	var rep planReport
	if err := json.Unmarshal(resp.Data, &rep); err != nil {
		t.Fatal(err)
	}
	return resp.Data, rep
}

// editEnv rewrites cvctl.env without the lines starting with drop, then appends add.
func editEnv(t *testing.T, tmp, inst, drop, add string) {
	t.Helper()
	path := filepath.Join(tmp, inst, "cvctl.env")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kept []string
	for _, l := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(l, drop) {
			kept = append(kept, l)
		}
	}
	body := strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n" + add
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
}

func writeInfra(t *testing.T, tmp, inst, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(tmp, inst, "infra", name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertChangesIssues(t *testing.T, before, after json.RawMessage) {
	t.Helper()
	changes, err := upgrade.Diff(before, after)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range changes {
		if c.Path == "instances.myapp-dev.issues" {
			return
		}
	}
	t.Fatalf("issues not reported as a difference: %+v", changes)
}

func TestPlanValidInstanceOmitsIssues(t *testing.T) {
	setupPlanRig(t, "myapp-dev")
	raw, rep := planDoc(t)
	if strings.Contains(string(raw), `"issues"`) {
		t.Fatalf("issues must be omitted when empty: %s (%v)", raw, rep.Instances["myapp-dev"].Issues)
	}
}

// Digests reach a deploy from the desired state, so hosts have none in cvctl.env.
func TestPlanDoesNotRequireDigestsInCvctlEnv(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	editEnv(t, tmp, "myapp-dev", "CVCTL_DIGEST_", "")
	if _, rep := planDoc(t); len(rep.Instances["myapp-dev"].Issues) != 0 {
		t.Fatalf("issues = %v", rep.Instances["myapp-dev"].Issues)
	}
}

func TestPlanMissingDecisionIsADifference(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	before, _ := planDoc(t)
	editEnv(t, tmp, "myapp-dev", "CVCTL_DOMAIN_WEB=", "")
	after, rep := planDoc(t)
	in := rep.Instances["myapp-dev"]
	if in.Error != "" || !slices.Equal(in.Issues, []string{"E_DECISION_MISSING"}) {
		t.Fatalf("instance = %+v", in)
	}
	assertChangesIssues(t, before, after)
}

func TestPlanRejectedContractIsADifference(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	before, _ := planDoc(t)
	writeInfra(t, tmp, "myapp-dev", "compose.yml", strings.Replace(planCompose, "s3cr3t-value", "${UNDECLARED}", 1))
	after, rep := planDoc(t)
	in := rep.Instances["myapp-dev"]
	if in.Error != "" || !slices.Equal(in.Issues, []string{"E_CONTRACT_ENV_UNDECLARED"}) {
		t.Fatalf("instance = %+v", in)
	}
	assertChangesIssues(t, before, after)
}

// A second finding with a code already listed is a new requirement all the
// same, so repeats must survive or the two versions would compare equal.
func TestPlanIssuesAreSortedAndKeepRepeats(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	compose := strings.Replace(planCompose, "API_TOKEN: s3cr3t-value", "API_TOKEN: ${UNDECLARED_A}\n      OTHER: ${UNDECLARED_B}", 1)
	writeInfra(t, tmp, "myapp-dev", "compose.yml", compose)
	editEnv(t, tmp, "myapp-dev", "CVCTL_DOMAIN_WEB=", "")
	_, rep := planDoc(t)
	want := []string{"E_CONTRACT_ENV_UNDECLARED", "E_CONTRACT_ENV_UNDECLARED", "E_DECISION_MISSING"}
	if got := rep.Instances["myapp-dev"].Issues; !slices.Equal(got, want) {
		t.Fatalf("issues = %v", got)
	}
}

func TestPlanBadEnvSpecIsAnIssue(t *testing.T) {
	for name, setup := range map[string]func(t *testing.T, tmp string){
		"invalid": func(t *testing.T, tmp string) {
			writeInfra(t, tmp, "myapp-dev", "env.spec.yml", "nope: s3cr3t-value\n")
		},
		"missing": func(t *testing.T, tmp string) {
			if err := os.Remove(filepath.Join(tmp, "myapp-dev", "infra", "env.spec.yml")); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			tmp, _ := setupPlanRig(t, "myapp-dev")
			setup(t, tmp)
			raw, rep := planDoc(t)
			in := rep.Instances["myapp-dev"]
			if in.Error != "" || !slices.Equal(in.Issues, []string{"E_ENVSPEC_INVALID"}) {
				t.Fatalf("instance = %+v", in)
			}
			if strings.Contains(string(raw), "s3cr3t-value") {
				t.Fatalf("leak: %s", raw)
			}
		})
	}
}

// The message of this issue quotes the value; only the code may reach the plan.
func TestPlanIssuesCarryNoValues(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	editEnv(t, tmp, "myapp-dev", "CVCTL_DIGEST_WEB=", "CVCTL_DIGEST_WEB=s3cr3t-value\n")
	for _, args := range [][]string{{"plan", "--json"}, {"plan"}} {
		code, out, errb := run(t, args...)
		if code != 0 {
			t.Fatalf("code %d: out=%s err=%s", code, out, errb)
		}
		if !strings.Contains(out, "E_DECISION_INVALID") {
			t.Fatalf("plan %v lacks the issue: %s", args, out)
		}
		if strings.Contains(out, "s3cr3t-value") || strings.Contains(errb, "s3cr3t-value") {
			t.Fatalf("plan %v leaks a value: %s", args, out)
		}
	}
}

func planWeb(t *testing.T) (string, planApp) {
	t.Helper()
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
	return out, resp.Data.Instances["myapp-dev"].Apps["web"]
}

func TestPlanJSONUnchangedWithoutAliases(t *testing.T) {
	setupPlanRig(t, "myapp-dev")
	out, _ := planWeb(t)
	if strings.Contains(out, `"aliases"`) || strings.Contains(out, `"redirects"`) {
		t.Fatalf("plan gained alias keys:\n%s", out)
	}
}

func TestPlanListsAliasesAndRedirects(t *testing.T) {
	tmp, _ := setupPlanRig(t, "myapp-dev")
	path := filepath.Join(tmp, "myapp-dev", "cvctl.env")
	raw, _ := os.ReadFile(path)
	raw = append(raw, "CVCTL_ALIASES_WEB=b.example.com,a.example.com\nCVCTL_REDIRECT_WEB=www.example.com=app.example.com\n"...)
	if err := os.WriteFile(path, raw, 0o640); err != nil {
		t.Fatal(err)
	}
	_, web := planWeb(t)
	if !slices.Equal(web.Aliases, []string{"a.example.com", "b.example.com"}) || !slices.Equal(web.Redirects, []string{"www.example.com=app.example.com"}) {
		t.Fatalf("web = %+v", web)
	}
}
