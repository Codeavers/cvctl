package host

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestNotifierStepMarker(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	oldBaseURL := notify.DefaultBaseURL
	notify.DefaultBaseURL = srv.URL
	t.Cleanup(func() { notify.DefaultBaseURL = oldBaseURL })

	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("HOST_NAME", "myhost"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("NOTIFY", "telegram"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("TELEGRAM_CHAT_ID", "12345"); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("TELEGRAM_THREAD_ID", "6789"); err != nil {
		t.Fatal(err)
	}
	sec.Set("telegram_token", "secret-token-1")

	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}
	step := NotifierStep{}

	// First run: marker does not exist -> plan has changes, apply sends notification and writes marker.
	p, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check failed: %v", err)
	}
	if len(p.Changes) == 0 {
		t.Fatalf("expected changes on first run, got 0")
	}

	pending, err := step.Apply(context.Background(), env, p)
	if err != nil {
		t.Fatalf("Apply failed: %v", err)
	}
	if pending != "" {
		t.Fatalf("unexpected pending: %s", pending)
	}
	if callCount != 1 {
		t.Fatalf("expected 1 call to telegram, got %d", callCount)
	}

	markerData, err := fs.ReadFile("/var/lib/cvctl/notify.ok")
	if err != nil {
		t.Fatalf("marker file not written: %v", err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("secret-token-1|12345|6789")))
	if strings.TrimSpace(string(markerData)) != wantHash {
		t.Fatalf("marker hash = %q, want %q", string(markerData), wantHash)
	}
	fi, err := fs.Stat("/var/lib/cvctl/notify.ok")
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode != 0o640 || fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("marker stat = mode %o owner %s:%s, want 0640 deployer:deploy", fi.Mode, fi.User, fi.Group)
	}

	// Second run: unchanged.
	p2, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("second Check failed: %v", err)
	}
	if len(p2.Changes) != 0 {
		t.Fatalf("expected 0 changes on second run, got %v", p2.Changes)
	}
	if callCount != 1 {
		t.Fatalf("telegram called again on unchanged step, callCount=%d", callCount)
	}
}

func TestNotifierStepTokenChanged(t *testing.T) {
	callCount := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCount++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	oldBaseURL := notify.DefaultBaseURL
	notify.DefaultBaseURL = srv.URL
	t.Cleanup(func() { notify.DefaultBaseURL = oldBaseURL })

	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg.Set("HOST_NAME", "myhost")
	_ = cfg.Set("NOTIFY", "telegram")
	_ = cfg.Set("TELEGRAM_CHAT_ID", "12345")
	sec.Set("telegram_token", "token-initial")

	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}
	step := NotifierStep{}

	p, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Apply(context.Background(), env, p); err != nil {
		t.Fatal(err)
	}
	if callCount != 1 {
		t.Fatalf("callCount = %d, want 1", callCount)
	}

	// Now token changes:
	sec.Set("telegram_token", "token-updated")

	p2, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Changes) == 0 {
		t.Fatalf("expected changes after token changed, got 0")
	}

	if _, err := step.Apply(context.Background(), env, p2); err != nil {
		t.Fatalf("Apply failed on updated token: %v", err)
	}
	if callCount != 2 {
		t.Fatalf("expected second telegram call after token changed, got callCount=%d", callCount)
	}

	markerData, err := fs.ReadFile("/var/lib/cvctl/notify.ok")
	if err != nil {
		t.Fatal(err)
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte("token-updated|12345|")))
	if strings.TrimSpace(string(markerData)) != wantHash {
		t.Fatalf("marker hash = %q, want %q", string(markerData), wantHash)
	}
}

func TestNotifierStepSkipWhenNone(t *testing.T) {
	fs := systest.NewFS()
	cfg, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	_ = cfg.Set("NOTIFY", "none")
	env := &Env{FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}
	step := NotifierStep{}

	p, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if p.Skip == "" {
		t.Fatalf("expected step to be skipped when NOTIFY=none")
	}
}
