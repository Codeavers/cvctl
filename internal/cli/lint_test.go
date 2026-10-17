package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLintCLIJSON(t *testing.T) {
	code, out, _ := run(t, "lint", "../contract/testdata/ports", "--json")
	if code != 1 {
		t.Fatalf("code %d: %s", code, out)
	}
	var env struct {
		OK    bool `json:"ok"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		Issues []struct {
			Code string `json:"code"`
		} `json:"issues"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.OK || env.Error.Code != "E_LINT_FAILED" || len(env.Issues) == 0 || env.Issues[0].Code != "E_CONTRACT_PORTS_PUBLISHED" {
		t.Fatalf("got %s", out)
	}

	code, out, _ = run(t, "lint", "../contract/testdata/valid", "--json")
	if code != 0 {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestLintTooManyArgsExit2(t *testing.T) {
	if code, _, _ := run(t, "lint", "a", "b"); code != 2 {
		t.Fatalf("code %d", code)
	}
}

func TestLintMissingEnvSpecIsIssue(t *testing.T) {
	dir := t.TempDir()
	code, out, _ := run(t, "lint", dir, "--json")
	if code != 1 {
		t.Fatalf("code %d: %s", code, out)
	}
}

func TestLintMissingEnvSpecCode(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte("services: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ := run(t, "lint", dir, "--json")
	if code != 1 || !strings.Contains(out, "E_CONTRACT_NO_ENVSPEC") {
		t.Fatalf("code %d: %s", code, out)
	}
}
