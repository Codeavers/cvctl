package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
)

type telegramNotifier struct {
	token    string
	chatID   string
	threadID string
	baseURL  string
	host     string
	client   *http.Client
}

type telegramPayload struct {
	ChatID                string `json:"chat_id"`
	MessageThreadID       *int   `json:"message_thread_id,omitempty"`
	Text                  string `json:"text"`
	DisableWebPagePreview bool   `json:"disable_web_page_preview"`
}

type telegramResponse struct {
	OK          bool   `json:"ok"`
	ErrorCode   int    `json:"error_code,omitempty"`
	Description string `json:"description,omitempty"`
}

func newTelegramNotifier(token, chatID, threadID, baseURL string) *telegramNotifier {
	base := baseURL
	if base == "" {
		base = DefaultBaseURL
	}
	if base == "" {
		base = "https://api.telegram.org"
	}
	h, _ := os.Hostname()
	return &telegramNotifier{
		token:    token,
		chatID:   chatID,
		threadID: threadID,
		baseURL:  strings.TrimRight(base, "/"),
		host:     h,
		client:   &http.Client{Timeout: 15 * time.Second},
	}
}

// NewTelegram creates a Telegram Notifier posting to {baseURL}/bot{token}/sendMessage.
// baseURL defaults to https://api.telegram.org if empty.
func NewTelegram(token, chatID, threadID, baseURL string) Notifier {
	return newTelegramNotifier(token, chatID, threadID, baseURL)
}

func (t *telegramNotifier) Send(ctx context.Context, m Message) error {
	var threadIDInt *int
	if t.threadID != "" {
		parsed, err := strconv.Atoi(t.threadID)
		if err != nil {
			return &output.Error{
				Code:    "E_NOTIFY_SEND",
				Message: fmt.Sprintf("invalid telegram thread id %q: %v", t.threadID, err),
				Hint:    "thread id must be an integer",
			}
		}
		threadIDInt = &parsed
	}

	h := m.Host
	if h == "" {
		h = t.host
	}
	if h == "" {
		h, _ = os.Hostname()
	}
	if h == "" {
		h = "unknown"
	}

	level := strings.ToUpper(m.Level)
	if level == "" {
		level = "INFO"
	}

	header := fmt.Sprintf("[%s] %s %s", h, level, m.Title)
	if m.Title == "" {
		header = fmt.Sprintf("[%s] %s", h, level)
	}

	text := header
	if m.Text != "" {
		text = header + "\n" + m.Text
	}

	payload := telegramPayload{
		ChatID:                t.chatID,
		MessageThreadID:       threadIDInt,
		Text:                  text,
		DisableWebPagePreview: true,
	}

	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: fmt.Sprintf("failed to encode telegram payload: %v", err),
			Hint:    "check message content",
		}
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.baseURL, t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: fmt.Sprintf("failed to create telegram request: %v", err),
			Hint:    "check telegram configuration",
		}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		var ue *url.Error
		var msg string
		if errors.As(err, &ue) {
			redactedURL := redactURL(ue.URL, t.baseURL)
			msg = fmt.Sprintf("%s %s: %v", ue.Op, redactedURL, ue.Err)
		} else {
			msg = err.Error()
		}
		if t.token != "" {
			msg = strings.ReplaceAll(msg, t.token, "<redacted>")
		}
		return &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: msg,
			Hint:    "check network connectivity and telegram base URL",
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)
	var tgResp telegramResponse
	_ = json.Unmarshal(respBody, &tgResp)

	if !tgResp.OK || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		desc := tgResp.Description
		if desc == "" {
			desc = fmt.Sprintf("HTTP status %d", resp.StatusCode)
		}
		if t.token != "" {
			desc = strings.ReplaceAll(desc, t.token, "<redacted>")
		}
		return &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: fmt.Sprintf("telegram sendMessage failed: %s", desc),
			Hint:    "verify telegram bot token and chat id",
		}
	}

	return nil
}

func redactURL(rawURL, baseURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		b, berr := url.Parse(baseURL)
		if berr == nil && b.Host != "" {
			return fmt.Sprintf("%s://%s/bot<redacted>/sendMessage", b.Scheme, b.Host)
		}
		return "https://api.telegram.org/bot<redacted>/sendMessage"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return fmt.Sprintf("%s://%s/bot<redacted>/sendMessage", scheme, u.Host)
}
