package cli

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ggcrreg "github.com/google/go-containerregistry/pkg/registry"
)

func newTestRegistry(t *testing.T) (string, *int) {
	t.Helper()
	pushes := 0
	h := ggcrreg.New(ggcrreg.Logger(log.New(io.Discard, "", 0)))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			pushes++
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), &pushes
}

func TestPublishCLI(t *testing.T) {
	host, pushes := newTestRegistry(t)
	t.Setenv("CVCTL_INSECURE_REGISTRIES", host)
	t.Setenv("GITHUB_TOKEN", "sekret-token")
	t.Setenv("GITHUB_ACTOR", "bot")
	ref := host + "/acme/app-infra:develop"

	code, out, _ := run(t, "publish", "../contract/testdata/ports", ref, "--json")
	if code != 1 || *pushes != 0 {
		t.Fatalf("lint failure must not push: code %d pushes %d: %s", code, *pushes, out)
	}
	if !strings.Contains(out, "E_CONTRACT_PORTS_PUBLISHED") {
		t.Fatalf("issues lost: %s", out)
	}

	dir := t.TempDir()
	for _, f := range []string{"compose.yml", "env.spec.yml"} {
		b, err := os.ReadFile(filepath.Join("../contract/testdata/valid", f))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, f), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "deploy"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deploy", "prod.env"), []byte("A=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "publish", dir, ref, "--json")
	if code != 0 {
		t.Fatalf("code %d: %s %s", code, out, errs)
	}
	var env struct {
		Data struct {
			Ref    string `json:"ref"`
			Digest string `json:"digest"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env.Data.Ref != ref || !strings.HasPrefix(env.Data.Digest, "sha256:") {
		t.Fatalf("got %s", out)
	}
	if strings.Contains(out+errs, "sekret-token") {
		t.Fatal("token leaked")
	}
}

func TestPublishCLIBadRefAndArgs(t *testing.T) {
	code, out, _ := run(t, "publish", "../contract/testdata/valid", "NOT A REF", "--json")
	if code != 1 || !strings.Contains(out, "E_PUBLISH") {
		t.Fatalf("code %d: %s", code, out)
	}
	if code, _, _ := run(t, "publish", "x"); code != 2 {
		t.Fatalf("code %d", code)
	}
}
