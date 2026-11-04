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

	"github.com/codeavers/cvctl/internal/hostcfg"
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

	if _, exists := gotBody["disable_web_page_preview"]; exists {
		t.Fatalf("deprecated disable_web_page_preview still sent")
	}

	wantText := "<b>Server:</b> web-01\n<b>Status:</b> Deployment\n\ndeployed successfully"
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

func TestTelegramErrorTokenWithSpace(t *testing.T) {
	tokenWithSpace := "12 34:AB"

	// Request construction error: NewRequestWithContext fails on invalid URL scheme
	nReq := notify.NewTelegram(tokenWithSpace, "123", "", "://invalid-url")
	errReq := nReq.Send(context.Background(), notify.Message{Level: "info", Title: "t", Text: "m"})
	if errReq == nil {
		t.Fatalf("expected error for invalid request construction, got nil")
	}
	var oeReq *output.Error
	if !errors.As(errReq, &oeReq) {
		t.Fatalf("expected *output.Error, got %T: %v", errReq, errReq)
	}
	if strings.Contains(oeReq.Message, "12 34:AB") || strings.Contains(oeReq.Message, "12 34") {
		t.Fatalf("request error message contains token: %s", oeReq.Message)
	}
	if strings.Contains(oeReq.Hint, "12 34:AB") || strings.Contains(oeReq.Hint, "12 34") {
		t.Fatalf("request error hint contains token: %s", oeReq.Hint)
	}

	// Network error: closed server ensures no external traffic and tests client.Do sanitization
	srvClosed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srvClosed.Close()

	nNet := notify.NewTelegram(tokenWithSpace, "123", "", srvClosed.URL)
	errNet := nNet.Send(context.Background(), notify.Message{Level: "info", Title: "t", Text: "m"})
	if errNet == nil {
		t.Fatalf("expected error for token with space, got nil")
	}
	var oeNet *output.Error
	if !errors.As(errNet, &oeNet) {
		t.Fatalf("expected *output.Error, got %T: %v", errNet, errNet)
	}
	if strings.Contains(oeNet.Message, "12 34:AB") || strings.Contains(oeNet.Message, "12 34") {
		t.Fatalf("network error message contains token with space: %s", oeNet.Message)
	}
	if strings.Contains(oeNet.Hint, "12 34:AB") || strings.Contains(oeNet.Hint, "12 34") {
		t.Fatalf("network error hint contains token with space: %s", oeNet.Hint)
	}
}

func TestTelegramMessageTruncation(t *testing.T) {
	var gotBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{}})
	}))
	defer srv.Close()

	n := notify.NewTelegram("test-token", "12345", "", srv.URL)
	msg := notify.Message{Host: "srv-01", Level: "info", Title: "LongMsg", Text: strings.Repeat("世", 5000)}
	if err := n.Send(context.Background(), msg); err != nil {
		t.Fatalf("unexpected Send error: %v", err)
	}

	sentText, ok := gotBody["text"].(string)
	if !ok {
		t.Fatalf("text not found in payload or not a string: %v", gotBody["text"])
	}
	if len([]rune(sentText)) > 4096 {
		t.Fatalf("sent text has %d runes, want <= 4096", len([]rune(sentText)))
	}
	if !strings.HasSuffix(sentText, "…") {
		t.Fatalf("sent text is not clipped")
	}
}

// tgServer records every POST body and answers with the given statuses in
// order; the last status repeats.
func tgServer(t *testing.T, statuses ...int) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		bodies = append(bodies, b)
		st := statuses[len(statuses)-1]
		if len(bodies) <= len(statuses) {
			st = statuses[len(bodies)-1]
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(st)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": st == 200, "description": "Bad Request: can't parse entities"})
	}))
	t.Cleanup(srv.Close)
	return srv, &bodies
}

func deployMsg() notify.Message {
	return notify.Message{
		Host: "web-01", Project: "app", Level: "info", Title: "Deployed",
		Deploy: &notify.Deploy{
			Changed: []string{"web"},
			Builds:  []notify.Build{{Service: "web", Repo: "https://github.com/o/r", Commit: "abcdef1234", PR: "7", Title: "Fix"}},
			Links:   []notify.Link{{Service: "api", URL: "https://api.example.com"}, {Service: "web", URL: "https://app.example.com"}},
		},
	}
}

func TestSendHTML(t *testing.T) {
	srv, bodies := tgServer(t, 200)
	if err := notify.NewTelegram("tok", "1", "", srv.URL).Send(context.Background(), deployMsg()); err != nil {
		t.Fatal(err)
	}
	b := (*bodies)[0]
	if b["parse_mode"] != "HTML" {
		t.Fatalf("parse_mode = %v", b["parse_mode"])
	}
	lp, _ := b["link_preview_options"].(map[string]any)
	if lp["is_disabled"] != true {
		t.Fatalf("link_preview_options = %v", b["link_preview_options"])
	}
	rm, _ := b["reply_markup"].(map[string]any)
	rows, _ := rm["inline_keyboard"].([]any)
	if len(rows) != 4 {
		t.Fatalf("inline_keyboard = %v", rm)
	}
	var urls []string
	for _, r := range rows {
		row := r.([]any)
		if len(row) != 1 {
			t.Fatalf("row = %v, want one button", row)
		}
		urls = append(urls, row[0].(map[string]any)["url"].(string))
	}
	want := []string{"https://github.com/o/r/pull/7", "https://github.com/o/r/commit/abcdef1234", "https://api.example.com", "https://app.example.com"}
	if strings.Join(urls, " ") != strings.Join(want, " ") {
		t.Fatalf("urls = %v, want %v", urls, want)
	}
}

func TestSendNoButtonsOmitsReplyMarkup(t *testing.T) {
	srv, bodies := tgServer(t, 200)
	if err := notify.NewTelegram("tok", "1", "", srv.URL).Send(context.Background(), notify.Message{Host: "h", Title: "x"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := (*bodies)[0]["reply_markup"]; ok {
		t.Fatalf("reply_markup present without buttons")
	}
}

func TestSendInfoIsSilent(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  bool
	}{{"info", true}, {"", true}, {"warn", false}, {"error", false}} {
		srv, bodies := tgServer(t, 200)
		if err := notify.NewTelegram("tok", "1", "", srv.URL).Send(context.Background(), notify.Message{Host: "h", Level: tc.level, Title: "x"}); err != nil {
			t.Fatal(err)
		}
		if got, _ := (*bodies)[0]["disable_notification"].(bool); got != tc.want {
			t.Errorf("level %q: disable_notification = %v, want %v", tc.level, got, tc.want)
		}
	}
}

func TestSendFallsBackToPlainOn400(t *testing.T) {
	srv, bodies := tgServer(t, 400, 200)
	m := deployMsg()
	m.Level = "warn"
	if err := notify.NewTelegram("tok", "1", "5", srv.URL).Send(context.Background(), m); err != nil {
		t.Fatalf("fallback should succeed: %v", err)
	}
	if len(*bodies) != 2 {
		t.Fatalf("posts = %d, want 2", len(*bodies))
	}
	b := (*bodies)[1]
	for _, k := range []string{"parse_mode", "reply_markup"} {
		if _, ok := b[k]; ok {
			t.Errorf("fallback still has %s", k)
		}
	}
	if b["message_thread_id"] != float64(5) {
		t.Errorf("thread = %v", b["message_thread_id"])
	}
	if got, _ := b["disable_notification"].(bool); got {
		t.Errorf("warn fallback should notify")
	}
	if strings.Contains(b["text"].(string), "<b>") {
		t.Errorf("fallback text has HTML: %v", b["text"])
	}
}

func TestSendNoFallbackOn401(t *testing.T) {
	srv, bodies := tgServer(t, 401)
	if err := notify.NewTelegram("tok", "1", "", srv.URL).Send(context.Background(), deployMsg()); err == nil {
		t.Fatal("expected error")
	}
	if len(*bodies) != 1 {
		t.Fatalf("posts = %d, want 1", len(*bodies))
	}
}

func TestSendFallbackFailureRedactsToken(t *testing.T) {
	token := "123:secret-token"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "bad " + token})
	}))
	defer srv.Close()
	err := notify.NewTelegram(token, "1", "", srv.URL).Send(context.Background(), deployMsg())
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_NOTIFY_SEND" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(oe.Message, token) || strings.Contains(oe.Hint, token) || !strings.Contains(oe.Message, "<redacted>") {
		t.Fatalf("message = %q", oe.Message)
	}
}

func TestLoggedFallsBackToStatus(t *testing.T) {
	var buf strings.Builder
	n := notify.Logged(failingNotifier{err: errors.New("boom")}, &buf)
	_ = n.Send(context.Background(), notify.Message{Status: "Deploy failed"})
	if !strings.Contains(buf.String(), `"Deploy failed" not sent`) {
		t.Fatalf("log = %q", buf.String())
	}
}

type failingNotifier struct{ err error }

func (f failingNotifier) Send(context.Context, notify.Message) error { return f.err }

func TestLoggedReportsFailedSend(t *testing.T) {
	var buf strings.Builder
	sendErr := &output.Error{Code: "E_NOTIFY_SEND", Message: "telegram sendMessage failed: chat not found"}
	n := notify.Logged(failingNotifier{err: sendErr}, &buf)
	if err := n.Send(context.Background(), notify.Message{Title: "app-dev deploy OK"}); !errors.Is(err, sendErr) {
		t.Fatalf("error not passed through: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, `"app-dev deploy OK" not sent`) || !strings.Contains(got, "chat not found") {
		t.Fatalf("log = %q", got)
	}

	buf.Reset()
	if err := notify.Logged(failingNotifier{}, &buf).Send(context.Background(), notify.Message{}); err != nil || buf.Len() != 0 {
		t.Fatalf("successful send logged %q (err %v)", buf.String(), err)
	}
	if _, ok := notify.Logged(notify.NoopNotifier{}, &buf).(notify.NoopNotifier); !ok {
		t.Fatal("no-op notifier wrapped")
	}
}

func TestFromHostLoggedReportsConfigError(t *testing.T) {
	cfg := hostcfg.NewConfig()
	if err := cfg.Set("NOTIFY", "telegram"); err != nil {
		t.Fatal(err)
	}
	sec := hostcfg.NewSecrets()
	sec.Set("telegram_token", "123:secret-token")

	var buf strings.Builder
	n := notify.FromHostLogged(cfg, sec, &buf)
	if _, ok := n.(notify.NoopNotifier); !ok {
		t.Fatalf("got %T, want no-op", n)
	}
	if got := buf.String(); !strings.Contains(got, "CVCTL_TELEGRAM_CHAT_ID is not set") || strings.Contains(got, "secret-token") {
		t.Fatalf("log = %q", got)
	}

	buf.Reset()
	if err := cfg.Set("NOTIFY", "none"); err != nil {
		t.Fatal(err)
	}
	notify.FromHostLogged(cfg, sec, &buf)
	if buf.Len() != 0 {
		t.Fatalf("disabled notifications logged %q", buf.String())
	}
}

func TestSendSurvivesCancelledContext(t *testing.T) {
	posts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts++
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n := notify.NewTelegram("t", "1", "", srv.URL)
	if err := n.Send(ctx, notify.Message{Level: "error", Title: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if posts != 1 {
		t.Fatalf("posts = %d, want 1", posts)
	}
}
