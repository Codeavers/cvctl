package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestNotifyTestCommand(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	oldBaseURL := notify.DefaultBaseURL
	notify.DefaultBaseURL = srv.URL
	t.Cleanup(func() { notify.DefaultBaseURL = oldBaseURL })

	// Case 1: Success with custom message
	fs := systest.NewFS()
	_ = fs.MkdirAll("/etc/cvctl/secrets", 0o750, sys.Owner{User: "root", Group: "deploy"})
	_ = fs.WriteFile("/etc/cvctl/host.env", []byte("CVCTL_NOTIFY=telegram\nCVCTL_TELEGRAM_CHAT_ID=12345\nCVCTL_HOST_NAME=mybox\n"), 0o640, sys.Owner{User: "root", Group: "deploy"})
	_ = fs.WriteFile("/etc/cvctl/secrets/telegram_token", []byte("secret-token\n"), 0o640, sys.Owner{User: "root", Group: "deploy"})

	oldNotifyEnv := newNotifyEnv
	newNotifyEnv = func() (sys.FS, string) { return fs, "/etc/cvctl" }
	t.Cleanup(func() { newNotifyEnv = oldNotifyEnv })

	var stdout, stderr bytes.Buffer
	root := NewRoot("test")
	code := Execute(root, []string{"notify", "test", "--message", "custom ping"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("expected exit code 0, got %d (stderr: %s)", code, stderr.String())
	}
	if strings.TrimSpace(stdout.String()) != "sent" {
		t.Fatalf("expected 'sent', got %q", stdout.String())
	}
	if text, ok := gotBody["text"].(string); !ok || !strings.Contains(text, "custom ping") {
		t.Fatalf("telegram payload text %v does not contain 'custom ping'", gotBody["text"])
	}

	// Case 2: Success with --json
	stdout.Reset()
	stderr.Reset()
	rootJSON := NewRoot("test")
	codeJSON := Execute(rootJSON, []string{"notify", "test", "--json"}, &stdout, &stderr)
	if codeJSON != 0 {
		t.Fatalf("expected exit code 0 with --json, got %d", codeJSON)
	}
	var jsonDoc map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &jsonDoc); err != nil {
		t.Fatalf("invalid json output: %v (got %q)", err, stdout.String())
	}
	if ok, _ := jsonDoc["ok"].(bool); !ok {
		t.Fatalf("expected ok=true, got %v", jsonDoc)
	}
	data, _ := jsonDoc["data"].(map[string]any)
	if sent, _ := data["sent"].(bool); !sent {
		t.Fatalf("expected data.sent=true, got %v", data)
	}

	// Case 3: NOTIFY=none -> E_NOTIFY_DISABLED
	fsDisabled := systest.NewFS()
	_ = fsDisabled.MkdirAll("/etc/cvctl", 0o750, sys.Owner{User: "root", Group: "deploy"})
	_ = fsDisabled.WriteFile("/etc/cvctl/host.env", []byte("CVCTL_NOTIFY=none\n"), 0o640, sys.Owner{User: "root", Group: "deploy"})

	newNotifyEnv = func() (sys.FS, string) { return fsDisabled, "/etc/cvctl" }

	stdout.Reset()
	stderr.Reset()
	rootDisabled := NewRoot("test")
	codeDisabled := Execute(rootDisabled, []string{"notify", "test", "--json"}, &stdout, &stderr)
	if codeDisabled == 0 {
		t.Fatalf("expected failure when NOTIFY=none, got 0")
	}
	var errDoc struct {
		OK    bool `json:"ok"`
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &errDoc); err != nil {
		t.Fatalf("cannot parse error json: %v (%s)", err, stdout.String())
	}
	if errDoc.Error.Code != "E_NOTIFY_DISABLED" {
		t.Fatalf("got error code %q, want E_NOTIFY_DISABLED", errDoc.Error.Code)
	}
	wantHint := "set CVCTL_NOTIFY=telegram in /etc/cvctl/host.env and run cvctl init"
	if errDoc.Error.Hint != wantHint {
		t.Fatalf("got hint %q, want %q", errDoc.Error.Hint, wantHint)
	}
}
