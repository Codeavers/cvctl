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
	Host    string
	Project string
	Level   string // info | warn | error
	Title   string
	Text    string
	// Status is the result shown on the Status line; empty falls back to Title.
	Status string
	// Error is the redacted cause shown on the Error line of a deploy notice;
	// empty falls back to Summary(Text).
	Error string
	// Deploy is nil for notices that are not about a deploy.
	Deploy *Deploy
}

// Build is the provenance of one image, read from its cvctl.* labels.
type Build struct{ Service, Repo, Commit, PR, Title, Author string }

// Link is the public URL of an app service.
type Link struct{ Service, URL string }

// Deploy carries what a deploy notice adds to the common fields.
type Deploy struct {
	Changed []string // services whose image changed; empty means unchanged
	Builds  []Build  // only images that carry labels
	Links   []Link   // one button each, in this order
	Logs    string
	Next    string // command to run next, shown at any level
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
		name := m.Title
		if name == "" {
			name = m.Status
		}
		fmt.Fprintf(l.w, "notify: %q not sent: %v\n", name, err)
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
