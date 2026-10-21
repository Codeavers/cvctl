package host

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const testEd25519Key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIA== test@host"

func newSSHTestEnv(adminUser string, manage bool, interactive bool) (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	seedOS(fs, ubuntu2404)
	r.On("systemctl cat ssh.service", sys.Result{ExitCode: 0})
	cfg := &hostcfg.Config{
		AdminUser: adminUser,
		SSHManage: manage,
	}
	r.On("id -nG "+adminUser, sys.Result{
		Stdout:   []byte(adminUser + " sudo\n"),
		ExitCode: 0,
	})
	return &Env{
		Run:         r,
		FS:          fs,
		Cfg:         cfg,
		Interactive: interactive,
		EUID:        0,
	}, r, fs
}

func TestSSHAdminRootRefused(t *testing.T) {
	env, _, _ := newSSHTestEnv("root", true, false)
	step := SSHStep{}
	_, err := step.Check(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when admin user is root, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_SSH_ADMIN_ROOT" {
		t.Errorf("got code %q, want E_SSH_ADMIN_ROOT", oe.Code)
	}
	if oe.Hint != "set CVCTL_ADMIN_USER to a non-root sudo user" {
		t.Errorf("got hint %q", oe.Hint)
	}
}

func TestSSHAdminNoSudoRefused(t *testing.T) {
	env, runner, _ := newSSHTestEnv("andres", true, false)
	runner.On("id -nG andres", sys.Result{
		Stdout:   []byte("andres users\n"),
		ExitCode: 0,
	})
	step := SSHStep{}
	_, err := step.Check(context.Background(), env)
	if err == nil {
		t.Fatal("expected error when admin user is not in sudo or admin, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Code != "E_SSH_ADMIN_NO_SUDO" {
		t.Errorf("got code %q, want E_SSH_ADMIN_NO_SUDO", oe.Code)
	}
	if oe.Hint != "usermod -aG sudo andres" {
		t.Errorf("got hint %q, want usermod -aG sudo andres", oe.Hint)
	}
}

func TestSSHAdminWithAdminGroupAccepted(t *testing.T) {
	env, runner, _ := newSSHTestEnv("andres", true, false)
	runner.On("id -nG andres", sys.Result{
		Stdout:   []byte("andres admin\n"),
		ExitCode: 0,
	})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}
}

func TestSSHConfirmPromptWording(t *testing.T) {
	if !strings.Contains(sshConfirmPrompt, "sudo -v") {
		t.Errorf("expected sshConfirmPrompt to mention sudo -v, got %q", sshConfirmPrompt)
	}
	if !strings.Contains(sshConfirmPrompt, "NEW session") {
		t.Errorf("expected sshConfirmPrompt to mention NEW session, got %q", sshConfirmPrompt)
	}
}

func seedAdminWithKey(r *systest.Runner, fs *systest.FS, adminUser, home, gid, group, keyLine string) {
	r.On("getent passwd "+adminUser, sys.Result{
		Stdout:   []byte(adminUser + ":x:1000:" + gid + "::" + home + ":/bin/bash\n"),
		ExitCode: 0,
	})
	r.On("getent group "+gid, sys.Result{
		Stdout:   []byte(group + ":x:" + gid + ":\n"),
		ExitCode: 0,
	})
	owner := sys.Owner{User: adminUser, Group: group}
	_ = fs.MkdirAll(home, 0o755, owner)
	if keyLine != "" {
		_ = fs.MkdirAll(home+"/.ssh", 0o700, owner)
		_ = fs.WriteFile(home+"/.ssh/authorized_keys", []byte(keyLine+"\n"), 0o600, owner)
	}
}

func TestSSHNoKeysNonInteractive(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/custom/home/andres", "1000", "andres", "")
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when drop-in is missing, got none")
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_SSH_NO_KEYS error, got nil")
	}
	if got := code(t, err); got != "E_SSH_NO_KEYS" {
		t.Fatalf("got error code %q, want E_SSH_NO_KEYS", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Hint != "add your public key to ~andres/.ssh/authorized_keys or run cvctl init interactively" {
		t.Fatalf("unexpected hint: %q", oe.Hint)
	}

	if _, err := fs.Stat("/etc/ssh/sshd_config.d/00-cvctl.conf"); err == nil {
		t.Fatal("drop-in /etc/ssh/sshd_config.d/00-cvctl.conf must not be written when keys are missing")
	}
}

func TestSSHOrderRevertBeforeReload(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	wantPending := "ssh hardened; confirm from a NEW session within 5 minutes: sudo cvctl init confirm-ssh"
	if pending != wantPending {
		t.Fatalf("got pending %q, want %q", pending, wantPending)
	}

	stopIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service"
	})
	resetIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "systemctl reset-failed cvctl-ssh-revert.timer cvctl-ssh-revert.service"
	})
	revertIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.Name == "systemd-run"
	})
	reloadIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "systemctl try-reload-or-restart ssh"
	})
	if stopIdx == -1 || resetIdx == -1 {
		t.Fatalf("expected stop and reset-failed before systemd-run; calls=%v", runner.Calls)
	}
	if revertIdx == -1 {
		t.Fatalf("systemd-run was not called; calls=%v", runner.Calls)
	}
	if reloadIdx == -1 {
		t.Fatalf("systemctl try-reload-or-restart ssh was not called; calls=%v", runner.Calls)
	}
	if stopIdx >= revertIdx || resetIdx >= revertIdx {
		t.Fatalf("expected stop (%d) and reset-failed (%d) before systemd-run (%d)", stopIdx, resetIdx, revertIdx)
	}
	if revertIdx >= reloadIdx {
		t.Fatalf("expected systemd-run (index %d) before systemctl try-reload-or-restart ssh (index %d)", revertIdx, reloadIdx)
	}

	data, err := fs.ReadFile("/etc/ssh/sshd_config.d/00-cvctl.conf")
	if err != nil {
		t.Fatalf("expected drop-in to exist: %v", err)
	}
	wantContent := "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nPubkeyAuthentication yes\n"
	if string(data) != wantContent {
		t.Fatalf("drop-in content mismatch:\ngot:\n%s\nwant:\n%s", string(data), wantContent)
	}
	fi, err := fs.Stat("/etc/ssh/sshd_config.d/00-cvctl.conf")
	if err != nil {
		t.Fatalf("stat drop-in: %v", err)
	}
	if fi.Mode != 0o644 || fi.User != "root" || fi.Group != "root" {
		t.Fatalf("drop-in meta: got mode=%04o owner=%s:%s, want 0644 root:root", fi.Mode, fi.User, fi.Group)
	}
}

func TestSSHSystemdRunFailureRollsBackWithoutReload(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("Failed to start transient timer unit: Unit cvctl-ssh-revert.timer already exists.\n"),
	})

	step := SSHStep{}
	_, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	})
	if err == nil {
		t.Fatal("expected E_SSH_FAILED when systemd-run fails, got nil")
	}
	if got := code(t, err); got != "E_SSH_FAILED" {
		t.Fatalf("got error code %q, want E_SSH_FAILED", got)
	}
	if _, statErr := fs.Stat(sshDropInPath); statErr == nil {
		t.Fatal("expected drop-in to be removed when systemd-run fails")
	}
	reloaded := slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.Name == "systemctl" && (slices.Contains(c.Args, "reload") || slices.Contains(c.Args, "try-reload-or-restart"))
	})
	if reloaded {
		t.Fatalf("expected no ssh reload call when systemd-run fails; calls=%v", runner.Calls)
	}
}

func TestSSHInvalidConfigRollsBack(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", "ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTY= test@host")
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("/etc/ssh/sshd_config line 42: Bad configuration option\n"),
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_SSH_CONFIG error when sshd -t fails, got nil")
	}
	if got := code(t, err); got != "E_SSH_CONFIG" {
		t.Fatalf("got error code %q, want E_SSH_CONFIG", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if !strings.Contains(oe.Message, "Bad configuration option") {
		t.Fatalf("expected sshd -t stderr in message, got %q", oe.Message)
	}
	if oe.Hint != "sshd -t output is in the message; fix sshd_config and re-run" {
		t.Fatalf("unexpected hint: %q", oe.Hint)
	}

	if _, err := fs.Stat("/etc/ssh/sshd_config.d/00-cvctl.conf"); err == nil {
		t.Fatal("expected drop-in to be removed after sshd -t failure")
	}
}

func TestSSHInvalidConfigStdoutOnly(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{
		ExitCode: 1,
		Stdout:   []byte("/etc/ssh/sshd_config line 99: Bad configuration option on stdout\n"),
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_SSH_CONFIG error when sshd -t fails, got nil")
	}
	if got := code(t, err); got != "E_SSH_CONFIG" {
		t.Fatalf("got error code %q, want E_SSH_CONFIG", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if !strings.Contains(oe.Message, "Bad configuration option on stdout") {
		t.Fatalf("expected sshd -t stdout in message, got %q", oe.Message)
	}
	if oe.Hint != "sshd -t output is in the message; fix sshd_config and re-run" {
		t.Fatalf("unexpected hint: %q", oe.Hint)
	}

	if _, err := fs.Stat("/etc/ssh/sshd_config.d/00-cvctl.conf"); err == nil {
		t.Fatal("expected drop-in to be removed after sshd -t failure")
	}
}

type removeErrFS struct {
	*systest.FS
	removeErr error
}

func (r *removeErrFS) Remove(p string) error {
	_ = r.FS.Remove(p)
	return r.removeErr
}

func TestSSHInvalidConfigRollsBackRemoveError(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	env.FS = &removeErrFS{FS: fs, removeErr: errors.New("read-only filesystem")}
	runner.On("sshd -t", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("bad config\n"),
	})

	step := SSHStep{}
	_, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	})
	if err == nil {
		t.Fatal("expected E_SSH_CONFIG error, got nil")
	}
	if got := code(t, err); got != "E_SSH_CONFIG" {
		t.Fatalf("got error code %q, want E_SSH_CONFIG", got)
	}
	if !strings.Contains(err.Error(), "read-only filesystem") {
		t.Fatalf("expected FS.Remove error in message, got %q", err.Error())
	}
}

func TestSSHDropInNotEffectiveRollsBack(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	step := SSHStep{}
	_, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	})
	if err == nil {
		t.Fatal("expected E_SSH_CONFIG when drop-in is not effective, got nil")
	}
	if got := code(t, err); got != "E_SSH_CONFIG" {
		t.Fatalf("got error code %q, want E_SSH_CONFIG", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	wantMsg := "drop-in written but not effective (is sshd_config.d included?)"
	if oe.Message != wantMsg {
		t.Fatalf("got message %q, want %q", oe.Message, wantMsg)
	}
	if _, statErr := fs.Stat(sshDropInPath); statErr == nil {
		t.Fatal("expected drop-in to be removed when not effective")
	}
	armed := slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.Name == "systemd-run"
	})
	if armed {
		t.Fatalf("expected timer not to be armed when drop-in is not effective; calls=%v", runner.Calls)
	}
}

func TestSSHEffectiveMismatch(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	wantContent := "PasswordAuthentication no\nKbdInteractiveAuthentication no\nPermitRootLogin no\nPubkeyAuthentication yes\n"
	_ = fs.WriteFile("/etc/ssh/sshd_config.d/00-cvctl.conf", []byte(wantContent), 0o644, sys.Owner{User: "root", Group: "root"})

	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when sshd -T reports passwordauthentication yes despite drop-in present")
	}
}

func TestSSHCheckPendingWhenRevertTimerActive(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	_ = fs.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("systemctl is-active cvctl-ssh-revert.timer", sys.Result{
		Stdout:   []byte("active\n"),
		ExitCode: 0,
	})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when revert timer is active, got %v", plan.Changes)
	}
	if plan.Pending != sshPendingMsg {
		t.Fatalf("got pending %q, want %q", plan.Pending, sshPendingMsg)
	}
}

func TestSSHConfirmInteractive(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, true)
	env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
		"admin_ssh_pubkey": "sk-ssh-ed25519@openssh.com AAAAGnNrLXNzaC1lZDI1NTE5QG9wZW5zc2guY29t user@host",
		"confirm_ssh":      "yes",
	})
	seedAdminWithKey(runner, fs, "andres", "/srv/admins/andres", "1001", "admins", "")
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{ExitCode: 0})
	runner.On("systemctl is-active cvctl-ssh-revert.timer", sys.Result{Stdout: []byte("inactive\n"), ExitCode: 3})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending after interactive confirmation, got %q", pending)
	}

	stopped := slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool {
		return strings.HasPrefix(c.String(), "systemctl stop cvctl-ssh-revert.timer")
	})
	if !stopped {
		t.Fatalf("expected systemctl stop cvctl-ssh-revert.timer to be called; calls=%v", runner.Calls)
	}

	sshDirInfo, err := fs.Stat("/srv/admins/andres/.ssh")
	if err != nil {
		t.Fatalf("expected ~/.ssh to be created: %v", err)
	}
	if sshDirInfo.Mode != 0o700 || sshDirInfo.User != "andres" || sshDirInfo.Group != "admins" {
		t.Fatalf("~/.ssh meta: got mode=%04o owner=%s:%s, want 0700 andres:admins", sshDirInfo.Mode, sshDirInfo.User, sshDirInfo.Group)
	}
	akInfo, err := fs.Stat("/srv/admins/andres/.ssh/authorized_keys")
	if err != nil {
		t.Fatalf("expected authorized_keys to be created: %v", err)
	}
	if akInfo.Mode != 0o600 || akInfo.User != "andres" || akInfo.Group != "admins" {
		t.Fatalf("authorized_keys meta: got mode=%04o owner=%s:%s, want 0600 andres:admins", akInfo.Mode, akInfo.User, akInfo.Group)
	}
}

func TestSSHDeclinedReverts(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, true)
	env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
		"confirm_ssh": "no",
	})
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{ExitCode: 5, Stderr: []byte("Unit cvctl-ssh-revert.timer not loaded.\n")})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_SSH_NOT_CONFIRMED error when user declines, got nil")
	}
	if got := code(t, err); got != "E_SSH_NOT_CONFIRMED" {
		t.Fatalf("got error code %q, want E_SSH_NOT_CONFIRMED", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Hint != "check your key and run cvctl init again" {
		t.Fatalf("unexpected hint: %q", oe.Hint)
	}

	if _, err := fs.Stat("/etc/ssh/sshd_config.d/00-cvctl.conf"); err == nil {
		t.Fatal("expected drop-in to be removed when confirmation is declined")
	}

	var reloadCount int
	for _, c := range runner.Calls {
		if c.String() == "systemctl try-reload-or-restart ssh" {
			reloadCount++
		}
	}
	if reloadCount < 2 {
		t.Fatalf("expected systemctl try-reload-or-restart ssh to be called for apply and revert, got %d calls", reloadCount)
	}
	stopped := slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool {
		return strings.HasPrefix(c.String(), "systemctl stop cvctl-ssh-revert.timer")
	})
	if !stopped {
		t.Fatal("expected timer stop on declined confirmation")
	}
}

type writeCountFS struct {
	*systest.FS
	writes map[string]int
}

func (w *writeCountFS) WriteFile(p string, data []byte, mode os.FileMode, o sys.Owner) error {
	if w.writes == nil {
		w.writes = map[string]int{}
	}
	w.writes[p]++
	return w.FS.WriteFile(p, data, mode, o)
}

func TestSSHStrictModesFixesPermAndOwner(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1001", "admins", "")
	rootOwner := sys.Owner{User: "root", Group: "root"}
	_ = fs.MkdirAll("/home/andres/.ssh", 0o755, rootOwner)
	wantKeyContent := testEd25519Key + "\n"
	_ = fs.WriteFile("/home/andres/.ssh/authorized_keys", []byte(wantKeyContent), 0o644, rootOwner)

	wfs := &writeCountFS{FS: fs}
	env.FS = wfs

	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})

	step := SSHStep{}
	if _, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	}); err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	dirInfo, err := fs.Stat("/home/andres/.ssh")
	if err != nil {
		t.Fatalf("stat ~/.ssh: %v", err)
	}
	if dirInfo.Mode != 0o700 || dirInfo.User != "andres" || dirInfo.Group != "admins" {
		t.Fatalf("~/.ssh meta: got mode=%04o owner=%s:%s, want 0700 andres:admins", dirInfo.Mode, dirInfo.User, dirInfo.Group)
	}
	akInfo, err := fs.Stat("/home/andres/.ssh/authorized_keys")
	if err != nil {
		t.Fatalf("stat authorized_keys: %v", err)
	}
	if akInfo.Mode != 0o600 || akInfo.User != "andres" || akInfo.Group != "admins" {
		t.Fatalf("authorized_keys meta: got mode=%04o owner=%s:%s, want 0600 andres:admins", akInfo.Mode, akInfo.User, akInfo.Group)
	}
	gotData, err := fs.ReadFile("/home/andres/.ssh/authorized_keys")
	if err != nil || string(gotData) != wantKeyContent {
		t.Fatalf("authorized_keys content: got %q, want %q", string(gotData), wantKeyContent)
	}
	if wfs.writes["/home/andres/.ssh/authorized_keys"] != 1 {
		t.Fatalf("expected 1 write to authorized_keys to fix mode/owner, got %d", wfs.writes["/home/andres/.ssh/authorized_keys"])
	}

	// Second apply when mode/owner already match must not rewrite authorized_keys.
	if _, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	}); err != nil {
		t.Fatalf("unexpected second Apply error: %v", err)
	}
	if wfs.writes["/home/andres/.ssh/authorized_keys"] != 1 {
		t.Fatalf("expected authorized_keys not to be rewritten when mode/owner already match, got %d writes", wfs.writes["/home/andres/.ssh/authorized_keys"])
	}
}

func TestSSHUnsafeHomeRejected(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	_ = fs.MkdirAll("/home/andres", 0o775, sys.Owner{User: "andres", Group: "andres"})

	step := SSHStep{}
	_, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	})
	if err == nil {
		t.Fatal("expected E_SSH_UNSAFE_HOME error when home is group-writable, got nil")
	}
	if got := code(t, err); got != "E_SSH_UNSAFE_HOME" {
		t.Fatalf("got error code %q, want E_SSH_UNSAFE_HOME", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Hint != "chmod go-w /home/andres" {
		t.Fatalf("got hint %q, want %q", oe.Hint, "chmod go-w /home/andres")
	}
	if _, statErr := fs.Stat(sshDropInPath); statErr == nil {
		t.Fatal("drop-in must not be written when home directory has unsafe permissions")
	}
}

func TestConfirmSSHAlreadyReverted(t *testing.T) {
	env, _, _ := newSSHTestEnv("andres", true, false)

	err := ConfirmSSH(context.Background(), env)
	if err == nil {
		t.Fatal("expected E_SSH_NOT_CONFIRMED when drop-in is missing, got nil")
	}
	if got := code(t, err); got != "E_SSH_NOT_CONFIRMED" {
		t.Fatalf("got error code %q, want E_SSH_NOT_CONFIRMED", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Message != "the change was already reverted" {
		t.Fatalf("got message %q, want %q", oe.Message, "the change was already reverted")
	}
	if oe.Hint != "run cvctl init again" {
		t.Fatalf("got hint %q, want %q", oe.Hint, "run cvctl init again")
	}
}

type callbackRunner struct {
	*systest.Runner
	onRun func(c sys.Cmd)
}

func (c *callbackRunner) Run(ctx context.Context, cmd sys.Cmd) (sys.Result, error) {
	if c.onRun != nil {
		c.onRun(cmd)
	}
	return c.Runner.Run(ctx, cmd)
}

func TestConfirmSSHRevertedWhileStoppingTimer(t *testing.T) {
	env, baseRunner, fs := newSSHTestEnv("andres", true, false)
	_ = fs.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, sys.Owner{User: "root", Group: "root"})
	baseRunner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{
		ExitCode: 0,
	})
	baseRunner.On("systemctl is-active cvctl-ssh-revert.timer", sys.Result{
		Stdout:   []byte("inactive\n"),
		ExitCode: 3,
	})
	runner := &callbackRunner{
		Runner: baseRunner,
		onRun: func(c sys.Cmd) {
			if strings.HasPrefix(c.String(), "systemctl stop cvctl-ssh-revert.timer") {
				_ = fs.Remove(sshDropInPath)
			}
		},
	}
	env.Run = runner

	err := ConfirmSSH(context.Background(), env)
	if err == nil {
		t.Fatal("expected E_SSH_NOT_CONFIRMED when drop-in is removed during stop, got nil")
	}
	if got := code(t, err); got != "E_SSH_NOT_CONFIRMED" {
		t.Fatalf("got error code %q, want E_SSH_NOT_CONFIRMED", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Message != "the change was already reverted" {
		t.Fatalf("got message %q, want %q", oe.Message, "the change was already reverted")
	}
}

func TestConfirmSSHStopFailure(t *testing.T) {
	t.Run("stop command fails with real error", func(t *testing.T) {
		env, runner, fs := newSSHTestEnv("andres", true, false)
		_ = fs.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, sys.Owner{User: "root", Group: "root"})
		runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{
			ExitCode: 1,
			Stderr:   []byte("Failed to stop cvctl-ssh-revert.timer: Access denied\n"),
		})

		err := ConfirmSSH(context.Background(), env)
		if err == nil {
			t.Fatal("expected E_SSH_FAILED when systemctl stop fails with access denied, got nil")
		}
		if got := code(t, err); got != "E_SSH_FAILED" {
			t.Fatalf("got error code %q, want E_SSH_FAILED", got)
		}
	})

	t.Run("timer still active after stop", func(t *testing.T) {
		env, runner, fs := newSSHTestEnv("andres", true, false)
		_ = fs.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, sys.Owner{User: "root", Group: "root"})
		runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{ExitCode: 0})
		runner.On("systemctl is-active cvctl-ssh-revert.timer", sys.Result{
			Stdout:   []byte("active\n"),
			ExitCode: 0,
		})

		err := ConfirmSSH(context.Background(), env)
		if err == nil {
			t.Fatal("expected E_SSH_FAILED when timer remains active after stop, got nil")
		}
		if got := code(t, err); got != "E_SSH_FAILED" {
			t.Fatalf("got error code %q, want E_SSH_FAILED", got)
		}
	})
}

func TestConfirmSSHSuccess(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	_ = fs.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{
		ExitCode: 5,
		Stderr:   []byte("Unit cvctl-ssh-revert.timer not loaded.\n"),
	})
	runner.On("systemctl is-active cvctl-ssh-revert.timer", sys.Result{
		Stdout:   []byte("inactive\n"),
		ExitCode: 3,
	})

	if err := ConfirmSSH(context.Background(), env); err != nil {
		t.Fatalf("unexpected ConfirmSSH error: %v", err)
	}
}

func TestSSHErrorAfterTimerArmedIncludesRevertNotice(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl try-reload-or-restart ssh", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("Job for ssh.service failed\n"),
	})

	step := SSHStep{}
	_, err := step.Apply(context.Background(), env, Plan{
		Changes: []string{"configure " + sshDropInPath},
		Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
	})
	if err == nil {
		t.Fatal("expected E_SSH_FAILED when reload fails, got nil")
	}
	if got := code(t, err); got != "E_SSH_FAILED" {
		t.Fatalf("got error code %q, want E_SSH_FAILED", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	wantSuffix := " (the change will be reverted automatically within 5 minutes)"
	if !strings.HasSuffix(oe.Message, wantSuffix) {
		t.Fatalf("expected error message %q to end with %q", oe.Message, wantSuffix)
	}
}

func TestSSHSkipWhenNotManaged(t *testing.T) {
	env, runner, _ := newSSHTestEnv("andres", false, false)

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if plan.Skip == "" {
		t.Fatal("expected non-empty Skip when SSHManage is false")
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when skipped, got %v", plan.Changes)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no commands when skipped, got %v", runner.Calls)
	}
}

func TestSSHRegisteredInDefaultSteps(t *testing.T) {
	steps := DefaultSteps()
	if len(steps) < 5 {
		t.Fatalf("expected at least 5 steps in DefaultSteps, got %d", len(steps))
	}
	if steps[3].Name() != "accounts" {
		t.Fatalf("step 3: got %q, want accounts", steps[3].Name())
	}
	if steps[4].Name() != "ssh" {
		t.Fatalf("step 4: got %q, want ssh", steps[4].Name())
	}
}

func TestSSHUnchangedWhenEffectiveMatches(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	_ = fs.WriteFile(sshDropInPath, []byte(sshDropInContent), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\npermitrootlogin no\n"),
		ExitCode: 0,
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when drop-in and sshd -T match desired state, got %v", plan.Changes)
	}
}

func TestSSHInvalidInteractivePubKeyRejected(t *testing.T) {
	cases := []struct {
		name string
		key  string
	}{
		{name: "bad prefix", key: "not-a-valid-key-prefix AAAA"},
		{name: "missing second field", key: "ssh-ed25519"},
		{name: "invalid base64 second field", key: "ssh-ed25519 !!!not-base64!!! user@host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, runner, fs := newSSHTestEnv("andres", true, true)
			env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
				"admin_ssh_pubkey": tc.key,
			})
			seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", "# comment only")

			step := SSHStep{}
			_, err := step.Apply(context.Background(), env, Plan{
				Changes: []string{"configure " + sshDropInPath},
				Actions: []Action{{Kind: "configure", Target: sshDropInPath}},
			})
			if err == nil {
				t.Fatalf("expected E_SSH_NO_KEYS error for %s (%q), got nil", tc.name, tc.key)
			}
			if got := code(t, err); got != "E_SSH_NO_KEYS" {
				t.Fatalf("got error code %q, want E_SSH_NO_KEYS", got)
			}
		})
	}
}

func TestSSHCheckWithSSHDMissingReturnsChangesNoError(t *testing.T) {
	env, runner, _ := newSSHTestEnv("andres", true, false)
	runner.OnErrorPrefix("sshd", errors.New("exec: \"sshd\": executable file not found in $PATH"))

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if !slices.Contains(plan.Changes, "enforce effective sshd config") {
		t.Fatalf("expected 'enforce effective sshd config' in changes, got %v", plan.Changes)
	}
}
