package cli

import (
	"bytes"
	"encoding/json"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Execute(NewRoot("1.2.3"), args, &out, &errb)
	return code, out.String(), errb.String()
}

func TestVersionJSON(t *testing.T) {
	code, out, _ := run(t, "version", "--json")
	if code != 0 {
		t.Fatalf("code %d", code)
	}
	var env struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if !env.OK || env.Data.Version != "1.2.3" {
		t.Fatalf("got %q", out)
	}
}

func TestUnknownCommandExit2(t *testing.T) {
	if code, _, _ := run(t, "nope"); code != 2 {
		t.Fatalf("code %d", code)
	}
}

func TestUnknownFlagExit2JSON(t *testing.T) {
	code, out, _ := run(t, "version", "--bogus", "--json")
	if code != 2 {
		t.Fatalf("code %d", code)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil || env["ok"] != false {
		t.Fatalf("out %q err %v", out, err)
	}
}
