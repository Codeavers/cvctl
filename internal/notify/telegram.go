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

type linkPreviewOptions struct {
	IsDisabled bool `json:"is_disabled"`
}

type inlineButton struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

type replyMarkup struct {
	InlineKeyboard [][]inlineButton `json:"inline_keyboard"`
}

type telegramPayload struct {
	ChatID              string             `json:"chat_id"`
	MessageThreadID     *int               `json:"message_thread_id,omitempty"`
	Text                string             `json:"text"`
	ParseMode           string             `json:"parse_mode,omitempty"`
	ReplyMarkup         *replyMarkup       `json:"reply_markup,omitempty"`
	DisableNotification bool               `json:"disable_notification"`
	LinkPreviewOptions  linkPreviewOptions `json:"link_preview_options"`
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
	// A failure notice is most needed after a deploy timeout or shutdown, when
	// ctx is already dead; the client timeout bounds each POST instead.
	ctx = context.WithoutCancel(ctx)

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

	payload := telegramPayload{
		ChatID:              t.chatID,
		MessageThreadID:     threadIDInt,
		DisableNotification: m.Level == "" || m.Level == "info",
		LinkPreviewOptions:  linkPreviewOptions{IsDisabled: true},
	}
	var buttons []Button
	payload.Text, buttons = renderHTML(m, h)
	payload.ParseMode = "HTML"
	if len(buttons) > 0 {
		rm := &replyMarkup{}
		for _, b := range buttons {
			rm.InlineKeyboard = append(rm.InlineKeyboard, []inlineButton{{Text: b.Text, URL: b.URL}})
		}
		payload.ReplyMarkup = rm
	}

	status, err := t.post(ctx, payload)
	if status != http.StatusBadRequest {
		return err
	}

	// Telegram rejects markup it cannot parse (e.g. a button URL it dislikes)
	// with 400; the notice still has to arrive, so resend it as plain text.
	payload.Text = renderPlain(m, h)
	payload.ParseMode = ""
	payload.ReplyMarkup = nil
	_, err = t.post(ctx, payload)
	return err
}

// post sends one sendMessage request and returns the HTTP status (0 when no
// response arrived) with a sanitized error.
func (t *telegramNotifier) post(ctx context.Context, payload telegramPayload) (int, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return 0, &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: fmt.Sprintf("failed to encode telegram payload: %v", err),
			Hint:    "check message content",
		}
	}

	endpoint := fmt.Sprintf("%s/bot%s/sendMessage", t.baseURL, t.token)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(bodyBytes))
	if err != nil {
		return 0, &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: sanitizeError(err, t.baseURL, t.token),
			Hint:    "check telegram configuration",
		}
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return 0, &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: sanitizeError(err, t.baseURL, t.token),
			Hint:    "check network connectivity and telegram base URL",
		}
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
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
		return resp.StatusCode, &output.Error{
			Code:    "E_NOTIFY_SEND",
			Message: fmt.Sprintf("telegram sendMessage failed: %s", desc),
			Hint:    "verify telegram bot token and chat id",
		}
	}

	return resp.StatusCode, nil
}

func sanitizeError(err error, baseURL, token string) string {
	if err == nil {
		return ""
	}
	var ue *url.Error
	var msg string
	if errors.As(err, &ue) {
		redactedURL := redactURL(ue.URL, baseURL)
		msg = fmt.Sprintf("%s %s: %v", ue.Op, redactedURL, ue.Err)
	} else {
		msg = err.Error()
	}
	if token != "" {
		msg = strings.ReplaceAll(msg, token, "<redacted>")
	}
	return msg
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
