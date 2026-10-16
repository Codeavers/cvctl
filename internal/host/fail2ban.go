package host

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const fail2banJailPath = "/etc/fail2ban/jail.d/cvctl.conf"

func fail2banJailContent(portStr string) string {
	return fmt.Sprintf("[sshd]\nenabled = true\nport = %s\nbackend = systemd\nmaxretry = 5\nfindtime = 10m\nbantime = 1h\n", portStr)
}

func resolveFail2banPorts(ctx context.Context, e *Env) string {
	res, err := e.Run.Run(ctx, sys.Cmd{
		Name: "sshd",
		Args: []string{"-T", "-C", "user=root,host=localhost,addr=127.0.0.1"},
	})
	if err == nil && res.ExitCode == 0 {
		ports := parseSSHPorts(res.Stdout)
		if len(ports) > 0 {
			return strings.Join(ports, ",")
		}
	}
	return "ssh"
}

type Fail2banStep struct{}

func (Fail2banStep) Name() string { return "fail2ban" }

func (Fail2banStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var changes []string
	var actions []Action

	portStr := resolveFail2banPorts(ctx, e)
	content := fail2banJailContent(portStr)

	fileChanged, err := fileNeedsWrite(e.FS, fail2banJailPath, []byte(content), 0o644, rootOwner)
	if err != nil {
		return Plan{}, fail2banFSErr(fail2banJailPath, err)
	}
	if fileChanged {
		changes = append(changes, fmt.Sprintf("configure %s (port=%s)", fail2banJailPath, portStr))
		actions = append(actions, Action{Kind: "configure", Target: fail2banJailPath, Arg: portStr})
	}

	enabledRes, errEnabled := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-enabled", "fail2ban"}})
	activeRes, errActive := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-active", "fail2ban"}})

	enabledOut := ""
	if errEnabled == nil {
		enabledOut = strings.TrimSpace(string(enabledRes.Stdout))
	}
	enabled := errEnabled == nil && enabledRes.ExitCode == 0 && (enabledOut == "" || enabledOut == "enabled")

	activeOut := ""
	if errActive == nil {
		activeOut = strings.TrimSpace(string(activeRes.Stdout))
	}
	active := errActive == nil && activeRes.ExitCode == 0 && (activeOut == "" || activeOut == "active")

	if !enabled || !active {
		changes = append(changes, "enable fail2ban")
		actions = append(actions, Action{Kind: "enable", Target: "fail2ban"})
	}
	// Reload is only needed when the service was already active before; otherwise enable --now starts it with the new jail.
	if fileChanged && active {
		changes = append(changes, "reload fail2ban")
		actions = append(actions, Action{Kind: "reload", Target: "fail2ban"})
	}

	return Plan{Changes: changes, Actions: actions}, nil
}

func (Fail2banStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	for _, a := range p.Actions {
		switch {
		case a.Kind == "configure" && a.Target == fail2banJailPath:
			jailPort := a.Arg
			if jailPort == "" {
				jailPort = "ssh"
			}
			content := fail2banJailContent(jailPort)
			if err := e.FS.MkdirAll(path.Dir(fail2banJailPath), 0o755, rootOwner); err != nil {
				return "", fail2banFSErr(fail2banJailPath, err)
			}
			if err := e.FS.WriteFile(fail2banJailPath, []byte(content), 0o644, rootOwner); err != nil {
				return "", fail2banFSErr(fail2banJailPath, err)
			}
		case a.Kind == "enable" && a.Target == "fail2ban":
			if err := runFail2banCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"enable", "--now", "fail2ban"}}); err != nil {
				return "", err
			}
		case a.Kind == "reload" && a.Target == "fail2ban":
			if err := runFail2banCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"try-reload-or-restart", "fail2ban"}}); err != nil {
				return "", err
			}
		}
	}

	return "", nil
}

func runFail2banCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return sys.CmdErr("E_FAIL2BAN_FAILED", c, res, nil, fmt.Sprintf("check `%s` and `systemctl status fail2ban`", c.String()))
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
