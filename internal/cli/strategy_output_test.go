package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/state"
)

func setStrategyEnv(t *testing.T, env *OpsEnv, inst, strategy string) {
	t.Helper()
	p := filepath.Join(env.deployRoot(), inst, "cvctl.env")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := strings.Replace(string(b), "CVCTL_STRATEGY=recreate", "CVCTL_STRATEGY="+strategy, 1)
	if err := os.WriteFile(p, []byte(s), 0o640); err != nil {
		t.Fatal(err)
	}
}

func TestStatusShowsStrategy(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	setStrategyEnv(t, env, "myapp-dev", "blue-green")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	_ = env.Store.Save(&state.State{Instance: "myapp-dev", Status: state.StatusOK, UpdatedAt: time.Now()})

	code, out, errb := run(t, "status")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "STRATEGY") || !strings.Contains(out, "blue-green") {
		t.Errorf("human status lacks strategy: %s", out)
	}

	code, out, errb = run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var resp struct {
		Data statusReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Instances) != 1 || resp.Data.Instances[0].Strategy != "blue-green" {
		t.Errorf("json status = %+v", resp.Data.Instances)
	}
}

func TestProjectListShowsStrategy(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	setStrategyEnv(t, env, "myapp-dev", "blue-green")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	code, out, errb := run(t, "project", "list")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "STRATEGY") || !strings.Contains(out, "blue-green") {
		t.Errorf("human list lacks strategy: %s", out)
	}

	code, out, errb = run(t, "project", "list", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var resp struct {
		Data projectListReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Data.Instances) != 1 || resp.Data.Instances[0].Strategy != "blue-green" {
		t.Errorf("json list = %+v", resp.Data.Instances)
	}
}

func TestHistoryShowsBlueGreen(t *testing.T) {
	_, env := setupOpsFixture(t, "myapp-dev")
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }
	_ = env.Store.Append("myapp-dev", state.Event{
		ID: "ev-1", At: time.Now(), Kind: "deploy", Result: "OK", Actor: "cli", Detail: "deployed", BlueGreen: []string{"api"},
	})

	code, out, errb := run(t, "history", "myapp-dev")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	if !strings.Contains(out, "(blue-green: api)") {
		t.Errorf("history lacks blue-green marker: %s", out)
	}
}
