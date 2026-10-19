package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/state"
)

func TestProjectListJSON(t *testing.T) {
	tmp, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	if err := env.Store.Save(&state.State{Instance: "myapp-dev", Status: state.StatusPaused, PausedReason: "paused by operator"}); err != nil {
		t.Fatal(err)
	}
	// A broken instance is listed with its error instead of failing the command.
	if err := os.MkdirAll(filepath.Join(tmp, "broken-dev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "broken-dev", "cvctl.env"), []byte("CVCTL_INSTANCE=broken-dev\nCVCTL_ENV=dev\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	code, out, errb := run(t, "project", "list", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if strings.Contains(out, "dbpassword") {
		t.Fatalf("list leaks a credential: %s", out)
	}
	var resp struct {
		Data projectListReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("json: %v (%s)", err, out)
	}
	got := map[string]projectListItem{}
	for _, it := range resp.Data.Instances {
		got[it.Instance] = it
	}
	ok := got["myapp-dev"]
	if ok.Env != "dev" || ok.Track != "develop" || !ok.Paused || ok.Error != "" {
		t.Errorf("myapp-dev = %+v", ok)
	}
	if !strings.HasPrefix(ok.Source, "git") || !strings.HasSuffix(ok.Source, "develop") {
		t.Errorf("source = %q", ok.Source)
	}
	if ok.Domains["web"] != "app.example.com" || ok.Data["db"] != "local" {
		t.Errorf("domains=%v data=%v", ok.Domains, ok.Data)
	}
	if b := got["broken-dev"]; b.Error == "" {
		t.Errorf("broken-dev should carry an error: %+v", b)
	}
}

func TestProjectListHuman(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	code, out, errb := run(t, "project", "list")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	for _, want := range []string{"INSTANCE", "myapp-dev", "web=app.example.com", "db=local"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
}
