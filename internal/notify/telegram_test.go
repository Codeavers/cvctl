package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
)

func TestTelegramPayloadWithThread(t *testing.T) {
	var gotBody map[string]any
	var gotPath string
	var gotContentType string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotContentType = r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	n := notify.NewTelegram("test-bot-token", "-1001234567890", "42", srv.URL)
	msg := notify.Message{
		Host:  "web-01",
		Level: "info",
		Title: "Deployment",
		Text:  "deployed successfully",
	}

	if err := n.Send(context.Background(), msg); err != nil {
		t.Fatalf("unexpected Send error: %v", err)
	}

	if gotPath != "/bottest-bot-token/sendMessage" {
		t.Fatalf("got path %q, want %q", gotPath, "/bottest-bot-token/sendMessage")
	}
	if !strings.Contains(gotContentType, "application/json") {
		t.Fatalf("got content-type %q, want application/json", gotContentType)
	}

	if chatID, ok := gotBody["chat_id"].(string); !ok || chatID != "-1001234567890" {
		t.Fatalf("got chat_id %v, want string %q", gotBody["chat_id"], "-1001234567890")
	}

	if threadID, ok := gotBody["message_thread_id"].(float64); !ok || int(threadID) != 42 {
		t.Fatalf("got message_thread_id %v, want 42", gotBody["message_thread_id"])
	}

	if preview, ok := gotBody["disable_web_page_preview"].(bool); !ok || !preview {
		t.Fatalf("got disable_web_page_preview %v, want true", gotBody["disable_web_page_preview"])
	}

	wantText := "[web-01] INFO Deployment\ndeployed successfully"
	if text, ok := gotBody["text"].(string); !ok || text != wantText {
		t.Fatalf("got text %q, want %q", gotBody["text"], wantText)
	}
}

func TestTelegramNoThreadOmitted(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	n := notify.NewTelegram("test-bot-token", "987654321", "", srv.URL)
	msg := notify.Message{
		Host:  "db-01",
		Level: "warn",
		Title: "Disk",
		Text:  "disk 85%",
	}

	if err := n.Send(context.Background(), msg); err != nil {
		t.Fatalf("unexpected Send error: %v", err)
	}

	if _, exists := gotBody["message_thread_id"]; exists {
		t.Fatalf("message_thread_id should be omitted when thread is empty, got %v", gotBody["message_thread_id"])
	}
}

func TestTelegramErrorNoToken(t *testing.T) {
	secretToken := "secret-bot-token-xyz-123"

	// Case 1: Telegram responds with 401 ok:false
	srv401 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ok":          false,
			"error_code":  401,
			"description": "Unauthorized",
		})
	}))
	defer srv401.Close()

	n1 := notify.NewTelegram(secretToken, "123", "", srv401.URL)
	err1 := n1.Send(context.Background(), notify.Message{Level: "info", Title: "t", Text: "m"})
	if err1 == nil {
		t.Fatalf("expected error from 401 response, got nil")
	}
	var oe1 *output.Error
	if !errors.As(err1, &oe1) {
		t.Fatalf("expected *output.Error, got %T: %v", err1, err1)
	}
	if oe1.Code != "E_NOTIFY_SEND" {
		t.Fatalf("got code %q, want E_NOTIFY_SEND", oe1.Code)
	}
	if strings.Contains(oe1.Message, secretToken) {
		t.Fatalf("error message contains token: %s", oe1.Message)
	}
	if strings.Contains(oe1.Hint, secretToken) {
		t.Fatalf("error hint contains token: %s", oe1.Hint)
	}
	if !strings.Contains(oe1.Message, "Unauthorized") {
		t.Fatalf("error message %q should contain Telegram description 'Unauthorized'", oe1.Message)
	}

	// Case 2: Network error (closed server)
	srvClosed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvClosed.Close()

	n2 := notify.NewTelegram(secretToken, "123", "", srvClosed.URL)
	err2 := n2.Send(context.Background(), notify.Message{Level: "info", Title: "t", Text: "m"})
	if err2 == nil {
		t.Fatalf("expected network error, got nil")
	}
	var oe2 *output.Error
	if !errors.As(err2, &oe2) {
		t.Fatalf("expected *output.Error, got %T: %v", err2, err2)
	}
	if oe2.Code != "E_NOTIFY_SEND" {
		t.Fatalf("got code %q, want E_NOTIFY_SEND", oe2.Code)
	}
	if strings.Contains(oe2.Message, secretToken) {
		t.Fatalf("network error message contains token: %s", oe2.Message)
	}
	if strings.Contains(oe2.Hint, secretToken) {
		t.Fatalf("network error hint contains token: %s", oe2.Hint)
	}
	if !strings.Contains(oe2.Message, "<redacted>") {
		t.Fatalf("network error message %q should have redacted the URL/token", oe2.Message)
	}
}

func TestTelegramInvalidThread(t *testing.T) {
	n := notify.NewTelegram("tok", "123", "not-a-number", "http://127.0.0.1:9")
	err := n.Send(context.Background(), notify.Message{Level: "info", Title: "t", Text: "m"})
	if err == nil {
		t.Fatalf("expected error for non-numeric thread ID, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_NOTIFY_SEND" {
		t.Fatalf("got %v, want code E_NOTIFY_SEND", err)
	}
}
