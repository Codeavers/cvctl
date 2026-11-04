// Package notify provides outgoing notification delivery (e.g. Telegram alerts)
// for host management and deployment events.
package notify

import (
	"context"
	"fmt"
	"io"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
)

// DefaultBaseURL is the default base URL for Telegram API requests.
// Tests may override this to point to an httptest.Server.
var DefaultBaseURL = "https://api.telegram.org"

// Message is an alert or notification dispatched to configured notifiers.
type Message struct {
	Host  string
	Level string // info | warn | error
	Title string
	Text  string
}

// Notifier sends formatted notifications to external endpoints.
type Notifier interface {
	Send(ctx context.Context, m Message) error
}

// NoopNotifier discards all messages without error.
type NoopNotifier struct{}

func (NoopNotifier) Send(_ context.Context, _ Message) error {
	return nil
}

// FromHost returns a configured Notifier based on host configuration and secrets.
// When notifications are disabled (NOTIFY=none or unset), it returns a no-op Notifier.
func FromHost(cfg *hostcfg.Config, secrets *hostcfg.Secrets) (Notifier, error) {
	if cfg == nil || cfg.Notify == "" || cfg.Notify == "none" {
		return NoopNotifier{}, nil
	}
	if cfg.Notify != "telegram" {
		return nil, &output.Error{
			Code:    "E_NOTIFY_CONFIG",
			Message: fmt.Sprintf("unsupported notification provider %q", cfg.Notify),
			Hint:    "set CVCTL_NOTIFY=telegram or none in /etc/cvctl/host.env",
		}
	}

	var token string
	if secrets != nil {
		token, _ = secrets.Get("telegram_token")
	}
	if token == "" {
		return nil, &output.Error{
			Code:    "E_NOTIFY_CONFIG",
			Message: "telegram_token is not set",
			Hint:    "set telegram_token in /etc/cvctl/secrets/telegram_token or run cvctl init",
		}
	}
	if cfg.TelegramChatID == "" {
		return nil, &output.Error{
			Code:    "E_NOTIFY_CONFIG",
			Message: "CVCTL_TELEGRAM_CHAT_ID is not set",
			Hint:    "set CVCTL_TELEGRAM_CHAT_ID in /etc/cvctl/host.env or run cvctl init",
		}
	}

	tg := newTelegramNotifier(token, cfg.TelegramChatID, cfg.TelegramThread, DefaultBaseURL)
	if cfg.HostName != "" {
		tg.host = cfg.HostName
	}
	return tg, nil
}

type loggedNotifier struct {
	n Notifier
	w io.Writer
}

// Logged reports failed sends to w. Callers discard Send errors so an alert
// never fails the operation behind it; without this the failure leaves no trace.
func Logged(n Notifier, w io.Writer) Notifier {
	if _, ok := n.(NoopNotifier); ok || w == nil {
		return n
	}
	return loggedNotifier{n: n, w: w}
}

func (l loggedNotifier) Send(ctx context.Context, m Message) error {
	err := l.n.Send(ctx, m)
	if err != nil {
		fmt.Fprintf(l.w, "notify: %q not sent: %v\n", m.Title, err)
	}
	return err
}

// FromHostLogged is FromHost for callers that must keep running without
// notifications: a configuration error is reported to w and yields a no-op.
func FromHostLogged(cfg *hostcfg.Config, secrets *hostcfg.Secrets, w io.Writer) Notifier {
	n, err := FromHost(cfg, secrets)
	if err != nil || n == nil {
		if err != nil && w != nil {
			fmt.Fprintf(w, "notify: notifications disabled: %v\n", err)
		}
		return NoopNotifier{}
	}
	return Logged(n, w)
}
