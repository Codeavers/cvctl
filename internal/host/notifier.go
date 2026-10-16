package host

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"strings"

	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
)

const notifyMarkerPath = "/var/lib/cvctl/notify.ok"

var notifyFromHost = notify.FromHost

type NotifierStep struct{}

func (NotifierStep) Name() string { return "notifier" }

func (NotifierStep) Check(_ context.Context, e *Env) (Plan, error) {
	if e.Cfg == nil || e.Cfg.Notify == "" || e.Cfg.Notify == "none" {
		return Plan{Skip: "notifications disabled (CVCTL_NOTIFY=none)"}, nil
	}

	token := ""
	if e.Secrets != nil {
		token, _ = e.Secrets.Get("telegram_token")
	}
	chatID := e.Cfg.TelegramChatID
	threadID := e.Cfg.TelegramThread
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token+"|"+chatID+"|"+threadID)))

	data, err := e.FS.ReadFile(notifyMarkerPath)
	if err == nil {
		fi, serr := e.FS.Stat(notifyMarkerPath)
		if serr == nil && !fi.IsDir && fi.Mode == 0o640 && fi.User == "deployer" && fi.Group == "deploy" && strings.TrimSpace(string(data)) == wantHash {
			return Plan{}, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Plan{}, notifyFSErr(notifyMarkerPath, err)
	}

	return Plan{Changes: []string{"send test notification and write " + notifyMarkerPath}}, nil
}

func (NotifierStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	n, err := notifyFromHost(e.Cfg, e.Secrets)
	if err != nil {
		return "", err
	}

	hostName := ""
	if e.Cfg != nil {
		hostName = e.Cfg.HostName
	}

	msg := notify.Message{
		Host:  hostName,
		Level: "info",
		Title: "init",
		Text:  fmt.Sprintf("cvctl: notifier configured on %s", hostName),
	}
	if err := n.Send(ctx, msg); err != nil {
		return "", err
	}

	token := ""
	if e.Secrets != nil {
		token, _ = e.Secrets.Get("telegram_token")
	}
	chatID := ""
	threadID := ""
	if e.Cfg != nil {
		chatID = e.Cfg.TelegramChatID
		threadID = e.Cfg.TelegramThread
	}
	wantHash := fmt.Sprintf("%x", sha256.Sum256([]byte(token+"|"+chatID+"|"+threadID)))

	if err := e.FS.MkdirAll("/var/lib/cvctl", 0o750, deployerOwner); err != nil {
		return "", notifyFSErr("/var/lib/cvctl", err)
	}
	if err := e.FS.WriteFile(notifyMarkerPath, []byte(wantHash+"\n"), 0o640, deployerOwner); err != nil {
		return "", notifyFSErr(notifyMarkerPath, err)
	}

	return "", nil
}

func notifyFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_NOTIFY_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
