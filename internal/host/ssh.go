package host

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	sshDropInPath    = "/etc/ssh/sshd_config.d/00-cvctl.conf"
	sshDropInContent = "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nPubkeyAuthentication yes\n"
	sshPendingMsg    = "ssh hardened; confirm from a NEW session within 5 minutes: sudo cvctl init confirm-ssh"
	sshConfirmPrompt = "Open a NEW session to this server now, log in and run sudo -v before answering yes. If you answer no or do nothing for 5 minutes, the change is reverted."
	sshRevertNotice  = " (the change will be reverted automatically within 5 minutes)"
)

type SSHStep struct{}

func (SSHStep) Name() string { return "ssh" }

func (SSHStep) Check(ctx context.Context, e *Env) (Plan, error) {
	if e.Cfg == nil || !e.Cfg.SSHManage {
		return Plan{Skip: "SSH_MANAGE=false"}, nil
	}

	if e.Cfg.AdminUser == "root" {
		return Plan{}, &output.Error{
			Code:    "E_SSH_ADMIN_ROOT",
			Message: "admin user cannot be root when SSH is managed",
			Hint:    "set CVCTL_ADMIN_USER to a non-root sudo user",
		}
	}

	idRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "id",
		Args: []string{"-nG", e.Cfg.AdminUser},
	})
	if err != nil {
		return Plan{}, err
	}
	if idRes.ExitCode != 0 {
		return Plan{}, &output.Error{
			Code:    "E_SSH_ADMIN_NO_SUDO",
			Message: fmt.Sprintf("admin user %s is not in group sudo or admin", e.Cfg.AdminUser),
			Hint:    fmt.Sprintf("usermod -aG sudo %s", e.Cfg.AdminUser),
		}
	}
	groups := strings.Fields(string(idRes.Stdout))
	if !slices.Contains(groups, "sudo") && !slices.Contains(groups, "admin") {
		return Plan{}, &output.Error{
			Code:    "E_SSH_ADMIN_NO_SUDO",
			Message: fmt.Sprintf("admin user %s is not in group sudo or admin", e.Cfg.AdminUser),
			Hint:    fmt.Sprintf("usermod -aG sudo %s", e.Cfg.AdminUser),
		}
	}

	activeRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "systemctl",
		Args: []string{"is-active", "cvctl-ssh-revert.timer"},
	})
	if err != nil {
		return Plan{}, err
	}
	if strings.TrimSpace(string(activeRes.Stdout)) == "active" {
		return Plan{Pending: sshPendingMsg}, nil
	}

	var changes []string

	data, err := e.FS.ReadFile(sshDropInPath)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		changes = append(changes, "configure "+sshDropInPath)
	case err != nil:
		return Plan{}, err
	default:
		fi, err := e.FS.Stat(sshDropInPath)
		if err != nil {
			return Plan{}, err
		}
		if strings.TrimSpace(string(data)) != strings.TrimSpace(sshDropInContent) ||
			fi.Mode != 0o644 || fi.User != "root" || fi.Group != "root" {
			changes = append(changes, "configure "+sshDropInPath)
		}
	}

	connSpec := fmt.Sprintf("user=%s,host=localhost,addr=127.0.0.1", e.Cfg.AdminUser)
	res, err := e.Run.Run(ctx, sys.Cmd{
		Name: "sshd",
		Args: []string{"-T", "-C", connSpec},
	})
	if err != nil {
		return Plan{}, err
	}
	eff := parseSSHDEffective(res.Stdout)
	if res.ExitCode != 0 || eff["passwordauthentication"] != "no" || eff["permitrootlogin"] != "no" {
		changes = append(changes, "enforce effective sshd config")
	}

	return Plan{Changes: changes}, nil
}

func (SSHStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	adminUser := ""
	if e.Cfg != nil {
		adminUser = e.Cfg.AdminUser
	}

	home, gid, err := resolveAdminPasswd(ctx, e, adminUser)
	if err != nil {
		return "", err
	}

	if homeInfo, err := e.FS.Stat(home); err == nil {
		if homeInfo.Mode&0o022 != 0 {
			return "", &output.Error{
				Code:    "E_SSH_UNSAFE_HOME",
				Message: fmt.Sprintf("home directory %s is group- or world-writable (%04o)", home, homeInfo.Mode),
				Hint:    fmt.Sprintf("chmod go-w %s", home),
			}
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	sshDir := path.Join(home, ".ssh")
	akPath := path.Join(sshDir, "authorized_keys")
	akData, err := e.FS.ReadFile(akPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}

	if hasValidAuthorizedKey(akData) {
		group, err := resolveGroupName(ctx, e, gid)
		if err != nil {
			return "", err
		}
		adminOwner := sys.Owner{User: adminUser, Group: group}
		if err := e.FS.MkdirAll(sshDir, 0o700, adminOwner); err != nil {
			return "", err
		}
		akInfo, err := e.FS.Stat(akPath)
		if err != nil {
			return "", err
		}
		if akInfo.Mode != 0o600 || akInfo.User != adminUser || akInfo.Group != group {
			if err := e.FS.WriteFile(akPath, akData, 0o600, adminOwner); err != nil {
				return "", err
			}
		}
	} else {
		if !e.Interactive || e.Prompt == nil {
			return "", noKeysErr(adminUser)
		}
		ans, err := e.Prompt.Ask(hostcfg.Question{
			Key:      "admin_ssh_pubkey",
			Prompt:   fmt.Sprintf("SSH public key for %s", adminUser),
			Validate: validateSSHPubKey,
		}, "")
		if err != nil {
			return "", err
		}
		if err := validateSSHPubKey(ans); err != nil {
			return "", noKeysErr(adminUser)
		}
		group, err := resolveGroupName(ctx, e, gid)
		if err != nil {
			return "", err
		}
		adminOwner := sys.Owner{User: adminUser, Group: group}
		if err := e.FS.MkdirAll(sshDir, 0o700, adminOwner); err != nil {
			return "", err
		}
		updated := appendAuthorizedKey(akData, ans)
		if err := e.FS.WriteFile(akPath, updated, 0o600, adminOwner); err != nil {
			return "", err
		}
	}

	rootOwner := sys.Owner{User: "root", Group: "root"}
	if err := e.FS.MkdirAll(path.Dir(sshDropInPath), 0o755, rootOwner); err != nil {
		return "", err
	}
	if err := e.FS.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, rootOwner); err != nil {
		return "", err
	}

	res, err := e.Run.Run(ctx, sys.Cmd{Name: "sshd", Args: []string{"-t"}})
	if err != nil {
		return "", wrapRemoveErr(err, e.FS.Remove(sshDropInPath))
	}
	if res.ExitCode != 0 {
		rmErr := e.FS.Remove(sshDropInPath)
		out := strings.TrimSpace(string(res.Stderr))
		if out == "" {
			out = strings.TrimSpace(string(res.Stdout))
		}
		msg := fmt.Sprintf("sshd -t failed (exit %d)", res.ExitCode)
		if out != "" {
			msg += ": " + out
		}
		return "", wrapRemoveErr(&output.Error{
			Code:    "E_SSH_CONFIG",
			Message: msg,
			Hint:    "sshd -t output is in the message; fix sshd_config and re-run",
		}, rmErr)
	}

	connSpec := fmt.Sprintf("user=%s,host=localhost,addr=127.0.0.1", adminUser)
	effRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "sshd",
		Args: []string{"-T", "-C", connSpec},
	})
	if err != nil {
		return "", wrapRemoveErr(err, e.FS.Remove(sshDropInPath))
	}
	eff := parseSSHDEffective(effRes.Stdout)
	if effRes.ExitCode != 0 || eff["passwordauthentication"] != "no" || eff["permitrootlogin"] != "no" {
		rmErr := e.FS.Remove(sshDropInPath)
		return "", wrapRemoveErr(&output.Error{
			Code:    "E_SSH_CONFIG",
			Message: "drop-in written but not effective (is sshd_config.d included?)",
			Hint:    "ensure /etc/ssh/sshd_config includes /etc/ssh/sshd_config.d/*.conf before any Match blocks",
		}, rmErr)
	}

	// Clear any leftover revert unit before arming so systemd-run is idempotent across retries.
	if _, err := e.Run.Run(ctx, sys.Cmd{
		Name: "systemctl",
		Args: []string{"stop", "cvctl-ssh-revert.timer", "cvctl-ssh-revert.service"},
	}); err != nil {
		return "", wrapRemoveErr(err, e.FS.Remove(sshDropInPath))
	}
	if _, err := e.Run.Run(ctx, sys.Cmd{
		Name: "systemctl",
		Args: []string{"reset-failed", "cvctl-ssh-revert.timer", "cvctl-ssh-revert.service"},
	}); err != nil {
		return "", wrapRemoveErr(err, e.FS.Remove(sshDropInPath))
	}

	// Arm the auto-revert timer before reloading sshd so a broken reload or dropped session still reverts.
	if err := runSSHCmd(ctx, e, sys.Cmd{
		Name: "systemd-run",
		Args: []string{
			"--unit", "cvctl-ssh-revert",
			"--on-active=300",
			"/bin/sh", "-c",
			"rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh",
		},
	}); err != nil {
		return "", wrapRemoveErr(err, e.FS.Remove(sshDropInPath))
	}

	if err := runSSHCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"try-reload-or-restart", "ssh"}}); err != nil {
		return "", withRevertNotice(err)
	}

	if e.Interactive && e.Prompt != nil {
		ans, err := e.Prompt.Ask(hostcfg.Question{
			Key:     "confirm_ssh",
			Prompt:  sshConfirmPrompt,
			Options: []string{"yes", "no"},
		}, "no")
		if err != nil {
			return "", withRevertNotice(err)
		}
		if ans == "yes" {
			if err := ConfirmSSH(ctx, e); err != nil {
				var oe *output.Error
				if errors.As(err, &oe) && oe.Code == "E_SSH_NOT_CONFIRMED" {
					return "", err
				}
				return "", withRevertNotice(err)
			}
			return "", nil
		}
		if err := e.FS.Remove(sshDropInPath); err != nil {
			return "", withRevertNotice(err)
		}
		if err := runSSHCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"try-reload-or-restart", "ssh"}}); err != nil {
			return "", withRevertNotice(err)
		}
		if err := stopRevertTimer(ctx, e); err != nil {
			return "", withRevertNotice(err)
		}
		return "", &output.Error{
			Code:    "E_SSH_NOT_CONFIRMED",
			Message: "SSH hardening was not confirmed and has been reverted",
			Hint:    "check your key and run cvctl init again",
		}
	}

	return sshPendingMsg, nil
}

// ConfirmSSH verifies the drop-in was not already reverted, stops the auto-revert
// timer and service, and verifies the timer is no longer active.
func ConfirmSSH(ctx context.Context, e *Env) error {
	if _, err := e.FS.Stat(sshDropInPath); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &output.Error{
				Code:    "E_SSH_NOT_CONFIRMED",
				Message: "the change was already reverted",
				Hint:    "run cvctl init again",
			}
		}
		return err
	}
	return stopRevertTimer(ctx, e)
}

func stopRevertTimer(ctx context.Context, e *Env) error {
	stopCmd := sys.Cmd{
		Name: "systemctl",
		Args: []string{"stop", "cvctl-ssh-revert.timer", "cvctl-ssh-revert.service"},
	}
	res, err := e.Run.Run(ctx, stopCmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 && !strings.Contains(strings.ToLower(string(res.Stderr)), "not loaded") {
		return sshCmdFailed(stopCmd, res)
	}

	activeCmd := sys.Cmd{
		Name: "systemctl",
		Args: []string{"is-active", "cvctl-ssh-revert.timer"},
	}
	activeRes, err := e.Run.Run(ctx, activeCmd)
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(activeRes.Stdout)) == "active" {
		return &output.Error{
			Code:    "E_SSH_FAILED",
			Message: "cvctl-ssh-revert.timer is still active after stop",
			Hint:    "check `systemctl status cvctl-ssh-revert.timer`",
		}
	}
	return nil
}

func wrapRemoveErr(err error, rmErr error) error {
	if rmErr == nil {
		return err
	}
	var oe *output.Error
	if errors.As(err, &oe) {
		oe.Message = fmt.Sprintf("%s (remove %s: %v)", oe.Message, sshDropInPath, rmErr)
		return oe
	}
	return fmt.Errorf("%w (remove %s: %v)", err, sshDropInPath, rmErr)
}

func withRevertNotice(err error) error {
	if err == nil {
		return nil
	}
	var oe *output.Error
	if errors.As(err, &oe) {
		if !strings.HasSuffix(oe.Message, sshRevertNotice) {
			oe.Message += sshRevertNotice
		}
		return oe
	}
	return fmt.Errorf("%w%s", err, sshRevertNotice)
}

func parseSSHDEffective(stdout []byte) map[string]string {
	m := make(map[string]string)
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) >= 2 {
			m[strings.ToLower(fields[0])] = strings.ToLower(strings.Join(fields[1:], " "))
		}
	}
	return m
}

func hasValidAuthorizedKey(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		if validateSSHPubKey(line) == nil {
			return true
		}
	}
	return false
}

func validateSSHPubKey(line string) error {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return errors.New("empty or comment line")
	}
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return errors.New("key must have at least key type and base64-encoded key")
	}
	ktype := fields[0]
	if !strings.HasPrefix(ktype, "ssh-") && !strings.HasPrefix(ktype, "ecdsa-") && !strings.HasPrefix(ktype, "sk-") {
		return errors.New("key must start with ssh-, ecdsa-, or sk-")
	}
	decoded, err := base64.StdEncoding.DecodeString(fields[1])
	if err != nil || len(decoded) == 0 {
		return errors.New("second field must be valid base64")
	}
	return nil
}

func appendAuthorizedKey(existing []byte, pubkey string) []byte {
	s := string(existing)
	if len(s) > 0 && !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	s += strings.TrimSpace(pubkey) + "\n"
	return []byte(s)
}

func resolveAdminPasswd(ctx context.Context, e *Env, adminUser string) (home string, gid string, err error) {
	c := sys.Cmd{Name: "getent", Args: []string{"passwd", adminUser}}
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return "", "", err
	}
	if res.ExitCode != 0 {
		return "", "", sshCmdFailed(c, res)
	}
	firstLine := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")[0]
	fields := strings.Split(firstLine, ":")
	if len(fields) < 6 || fields[5] == "" {
		return "", "", &output.Error{
			Code:    "E_SSH_FAILED",
			Message: fmt.Sprintf("malformed getent passwd output for %s", adminUser),
			Hint:    fmt.Sprintf("check `getent passwd %s`", adminUser),
		}
	}
	return fields[5], fields[3], nil
}

func resolveGroupName(ctx context.Context, e *Env, gid string) (string, error) {
	c := sys.Cmd{Name: "getent", Args: []string{"group", gid}}
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", sshCmdFailed(c, res)
	}
	firstLine := strings.Split(strings.TrimSpace(string(res.Stdout)), "\n")[0]
	fields := strings.Split(firstLine, ":")
	if len(fields) == 0 || fields[0] == "" {
		return "", &output.Error{
			Code:    "E_SSH_FAILED",
			Message: fmt.Sprintf("malformed getent group output for %s", gid),
			Hint:    fmt.Sprintf("check `getent group %s`", gid),
		}
	}
	return fields[0], nil
}

func noKeysErr(adminUser string) error {
	return &output.Error{
		Code:    "E_SSH_NO_KEYS",
		Message: fmt.Sprintf("no SSH public keys found for %s", adminUser),
		Hint:    fmt.Sprintf("add your public key to ~%s/.ssh/authorized_keys or run cvctl init interactively", adminUser),
	}
}

func runSSHCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return sshCmdFailed(c, res)
	}
	return nil
}

func sshCmdFailed(c sys.Cmd, res sys.Result) error {
	stderr := strings.TrimSpace(string(res.Stderr))
	msg := fmt.Sprintf("%s failed (exit %d)", c.String(), res.ExitCode)
	if stderr != "" {
		msg += ": " + stderr
	}
	return &output.Error{
		Code:    "E_SSH_FAILED",
		Message: msg,
		Hint:    fmt.Sprintf("check `%s` output and sshd service status", c.String()),
	}
}
