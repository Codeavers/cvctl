package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const demoDecisions = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_STRATEGY=recreate
CVCTL_DOMAIN_API=api.example.com
CVCTL_DOMAIN_MEDIA=media.example.com
CVCTL_DIGEST_API=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
CVCTL_DIGEST_WORKER=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CVCTL_SVC_MAIN_DB_MODE=local
CVCTL_SVC_MAIN_DB_VERSION=16
CVCTL_SVC_MAIN_DB_USER=demo
CVCTL_SVC_MAIN_DB_PASSWORD=dbpass
CVCTL_SVC_MAIN_DB_DB=demo
CVCTL_SVC_CACHE_MODE=local
CVCTL_SVC_CACHE_PASSWORD=redispass
CVCTL_SVC_MEDIA_MODE=local
CVCTL_SVC_MEDIA_USER=media
CVCTL_SVC_MEDIA_PASSWORD=mediapass
CVCTL_SVC_MEDIA_DB=uploads
CVCTL_SVC_MEDIA_APP_USER=media-app
CVCTL_SVC_MEDIA_APP_PASSWORD=mediaapppass
`

func copyDemo(t *testing.T, decisions string) string {
	t.Helper()
	inst := t.TempDir()
	infra := filepath.Join(inst, "infra")
	if err := os.MkdirAll(infra, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"compose.yml", "env.spec.yml"} {
		b, err := os.ReadFile(filepath.Join("..", "..", "examples", "demo", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(infra, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(inst, "cvctl.env"), []byte(decisions), 0o640); err != nil {
		t.Fatal(err)
	}
	return inst
}

type renderData struct {
	OK   bool `json:"ok"`
	Data struct {
		Instance  string   `json:"instance"`
		Missing   []string `json:"missing"`
		Generated []string `json:"generated"`
		Files     []string `json:"files"`
		Hint      string   `json:"hint"`
	} `json:"data"`
	Issues []struct {
		Code string `json:"code"`
	} `json:"issues"`
}

func TestRenderDemo(t *testing.T) {
	inst := copyDemo(t, demoDecisions)
	code, out, errs := run(t, "render", "--instance-dir", inst, "--json")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errs)
	}
	var r renderData
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"compose.cvctl.yml", "traefik.yml", "app.env"} {
		st, err := os.Stat(filepath.Join(inst, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o640 {
			t.Errorf("%s mode %v", f, st.Mode().Perm())
		}
	}
	env, _ := os.ReadFile(filepath.Join(inst, "app.env"))
	if !strings.Contains(string(env), "DATABASE_URL=postgres://demo:dbpass@main-db:5432/demo") {
		t.Fatalf("app.env:\n%s", env)
	}
	if len(r.Data.Missing) != 1 || r.Data.Missing[0] != "SMTP_PASSWORD" {
		t.Fatalf("missing %v", r.Data.Missing)
	}
	if r.Data.Hint != "cvctl set-env demo-dev SMTP_PASSWORD" || r.Data.Instance != "demo-dev" {
		t.Fatalf("data %+v", r.Data)
	}
	if len(r.Data.Generated) != 1 || r.Data.Generated[0] != "JWT_SECRET" || len(r.Data.Files) != 3 {
		t.Fatalf("data %+v", r.Data)
	}
}

func TestRenderIdempotent(t *testing.T) {
	inst := copyDemo(t, demoDecisions)
	read := func() map[string]string {
		m := map[string]string{}
		for _, f := range []string{"compose.cvctl.yml", "traefik.yml", "app.env"} {
			b, err := os.ReadFile(filepath.Join(inst, f))
			if err != nil {
				t.Fatal(err)
			}
			m[f] = string(b)
		}
		return m
	}
	if code, out, _ := run(t, "render", "--instance-dir", inst, "--json"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	first := read()
	code, out, _ := run(t, "render", "--instance-dir", inst, "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
	var r renderData
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Data.Generated) != 0 || !strings.Contains(out, `"generated":[]`) {
		t.Fatalf("second run generated: %s", out)
	}
	second := read()
	for f, v := range first {
		if second[f] != v {
			t.Errorf("%s differs between runs", f)
		}
	}
}

func TestRenderIncompleteDecisions(t *testing.T) {
	inst := copyDemo(t, strings.ReplaceAll(demoDecisions, "CVCTL_SVC_MAIN_DB_MODE=local\n", ""))
	code, out, _ := run(t, "render", "--instance-dir", inst, "--json")
	if code != 1 || !strings.Contains(out, "E_DECISION_MISSING") {
		t.Fatalf("code %d: %s", code, out)
	}
	if _, err := os.Stat(filepath.Join(inst, "app.env")); err == nil {
		t.Fatal("nothing must be written on failure")
	}
}

func TestRenderUsage(t *testing.T) {
	if code, _, _ := run(t, "render"); code != 2 {
		t.Fatalf("missing --instance-dir: code %d", code)
	}
	if code, _, _ := run(t, "render", "--instance-dir", t.TempDir(), "--edge", "bogus"); code != 2 {
		t.Fatalf("bad --edge: code %d", code)
	}
}

func TestRenderKeepsExistingAppEnv(t *testing.T) {
	inst := copyDemo(t, demoDecisions)
	if err := os.WriteFile(filepath.Join(inst, "app.env"), []byte("SMTP_PASSWORD='p$ss \"x\"'\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "render", "--instance-dir", inst, "--json")
	if code != 0 || !strings.Contains(out, `"missing":[]`) {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestLintDemo(t *testing.T) {
	if code, out, _ := run(t, "lint", "../../examples/demo"); code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestRenderRelativeInstanceDirGivesAbsoluteEnvFile(t *testing.T) {
	inst := copyDemo(t, demoDecisions)
	wd, _ := os.Getwd()
	if err := os.Chdir(filepath.Dir(inst)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(wd) })
	if code, out, errs := run(t, "render", "--instance-dir", filepath.Base(inst)); code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errs)
	}
	b, err := os.ReadFile(filepath.Join(inst, "compose.cvctl.yml"))
	if err != nil {
		t.Fatal(err)
	}
	// t.TempDir may traverse a symlink (macOS /var); compare on the base only.
	for _, line := range strings.Split(string(b), "\n") {
		if l := strings.TrimSpace(line); strings.HasSuffix(l, "app.env") && strings.HasPrefix(l, "- ") {
			if !filepath.IsAbs(strings.TrimPrefix(l, "- ")) {
				t.Fatalf("env_file not absolute: %s", l)
			}
		}
	}
	if !strings.Contains(string(b), "/app.env") {
		t.Fatalf("no env_file in:\n%s", b)
	}
}

func TestRenderFailureHintIsNotRerunLint(t *testing.T) {
	inst := copyDemo(t, strings.ReplaceAll(demoDecisions, "CVCTL_SVC_MAIN_DB_MODE=local\n", ""))
	_, _, errs := run(t, "render", "--instance-dir", inst)
	if strings.Contains(errs, "re-run lint") || !strings.Contains(errs, "cvctl.env") {
		t.Fatalf("hint: %s", errs)
	}
}

func TestCommandsHaveExamples(t *testing.T) {
	for _, c := range NewRoot("x").Commands() {
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		if c.Example == "" {
			t.Errorf("%s has no Example", c.Name())
		}
	}
}

func TestDemoWorkerRole(t *testing.T) {
	b, err := os.ReadFile("../../examples/demo/compose.yml")
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), "  worker:")
	j := strings.Index(string(b), "  main-db:")
	if !strings.Contains(string(b)[i:j], "cvctl.role: worker") {
		t.Fatal("demo worker must use cvctl.role: worker")
	}
}

func TestRenderAppEnvMalformedHidesSecrets(t *testing.T) {
	inst := copyDemo(t, demoDecisions)
	appEnvPath := filepath.Join(inst, "app.env")
	if err := os.WriteFile(appEnvPath, []byte("SECRET=\"topsecretvalue\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "render", "--instance-dir", inst, "--json")
	if code == 0 {
		t.Fatalf("expected render to fail, got exit 0; out=%s", out)
	}
	if strings.Contains(out, "topsecretvalue") {
		t.Errorf("stdout JSON leaked secret: %s", out)
	}
	if strings.Contains(errs, "topsecretvalue") {
		t.Errorf("stderr leaked secret: %s", errs)
	}
	var errResp struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(out), &errResp); err != nil {
		t.Fatalf("failed to parse json output: %v (out: %s)", err, out)
	}
	if errResp.Error.Code != "E_APPENV_READ" {
		t.Errorf("expected code E_APPENV_READ, got %s", errResp.Error.Code)
	}
	if strings.Contains(errResp.Error.Message, "topsecretvalue") {
		t.Errorf("error in json leaked secret: %s", errResp.Error.Message)
	}
	if strings.Contains(errResp.Error.Hint, "topsecretvalue") {
		t.Errorf("hint in json leaked secret: %s", errResp.Error.Hint)
	}
	expectedMsg := fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", appEnvPath)
	if errResp.Error.Message != expectedMsg {
		t.Errorf("expected error %q, got %q", expectedMsg, errResp.Error.Message)
	}
}
