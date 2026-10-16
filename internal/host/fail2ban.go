package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	fail2banJailPath    = "/etc/fail2ban/jail.d/cvctl.conf"
	fail2banJailContent = "[sshd]\nenabled = true\nbackend = systemd\nmaxretry = 5\nfindtime = 10m\nbantime = 1h\n"
)

var rootOwner = sys.Owner{User: "root", Group: "root"}

type Fail2banStep struct{}

func (Fail2banStep) Name() string { return "fail2ban" }

func (Fail2banStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var changes []string

	fileChanged, err := fileNeedsWrite(e.FS, fail2banJailPath, []byte(fail2banJailContent), 0o644, rootOwner)
	if err != nil {
		return Plan{}, fail2banFSErr(fail2banJailPath, err)
	}
	if fileChanged {
		changes = append(changes, "configure "+fail2banJailPath)
	}

	enabledRes, err := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-enabled", "fail2ban"}})
	if err != nil {
		return Plan{}, err
	}
	activeRes, err := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-active", "fail2ban"}})
	if err != nil {
		return Plan{}, err
	}

	enabledOut := strings.TrimSpace(string(enabledRes.Stdout))
	enabled := enabledRes.ExitCode == 0 && (enabledOut == "" || enabledOut == "enabled")

	activeOut := strings.TrimSpace(string(activeRes.Stdout))
	active := activeRes.ExitCode == 0 && (activeOut == "" || activeOut == "active")

	if !enabled || !active {
		changes = append(changes, "enable fail2ban")
	}
	// Reload is only needed when the service was already active before; otherwise enable --now starts it with the new jail.
	if fileChanged && active {
		changes = append(changes, "reload fail2ban")
	}

	return Plan{Changes: changes}, nil
}

func (Fail2banStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	changeSet := make(map[string]bool, len(p.Changes))
	for _, c := range p.Changes {
		changeSet[c] = true
	}

	if changeSet["configure "+fail2banJailPath] {
		if err := e.FS.MkdirAll(path.Dir(fail2banJailPath), 0o755, rootOwner); err != nil {
			return "", fail2banFSErr(fail2banJailPath, err)
		}
		if err := e.FS.WriteFile(fail2banJailPath, []byte(fail2banJailContent), 0o644, rootOwner); err != nil {
			return "", fail2banFSErr(fail2banJailPath, err)
		}
	}

	if changeSet["enable fail2ban"] {
		if err := runFail2banCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"enable", "--now", "fail2ban"}}); err != nil {
			return "", err
		}
	}

	if changeSet["reload fail2ban"] {
		if err := runFail2banCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"try-reload-or-restart", "fail2ban"}}); err != nil {
			return "", err
		}
	}

	return "", nil
}

func fileNeedsWrite(fsys sys.FS, p string, want []byte, mode os.FileMode, owner sys.Owner) (bool, error) {
	data, err := fsys.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	fi, err := fsys.Stat(p)
	if err != nil {
		return false, err
	}
	if fi.IsDir || fi.Mode != mode || fi.User != owner.User || fi.Group != owner.Group || string(data) != string(want) {
		return true, nil
	}
	return false, nil
}

func runFail2banCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(string(res.Stderr))
		msg := fmt.Sprintf("%s failed (exit %d)", c.String(), res.ExitCode)
		if stderr != "" {
			msg += ": " + stderr
		}
		return &output.Error{
			Code:    "E_FAIL2BAN_FAILED",
			Message: msg,
			Hint:    fmt.Sprintf("check `%s` and `systemctl status fail2ban`", c.String()),
		}
	}
	return nil
}

func fail2banFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_FAIL2BAN_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
