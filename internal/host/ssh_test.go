package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/hostcfg/hostcfgtest"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func newSSHTestEnv(adminUser string, manage bool, interactive bool) (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		AdminUser: adminUser,
		SSHManage: manage,
	}
	return &Env{
		Run:         r,
		FS:          fs,
		Cfg:         cfg,
		Interactive: interactive,
		EUID:        0,
	}, r, fs
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
	if keyLine != "" {
		owner := sys.Owner{User: adminUser, Group: group}
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
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI test@host")
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl reload ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl reload ssh", sys.Result{ExitCode: 0})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	wantPending := "ssh hardened; confirm from a NEW session within 5 minutes: sudo cvctl init confirm-ssh"
	if pending != wantPending {
		t.Fatalf("got pending %q, want %q", pending, wantPending)
	}

	revertIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.Name == "systemd-run"
	})
	reloadIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "systemctl reload ssh"
	})
	if revertIdx == -1 {
		t.Fatalf("systemd-run was not called; calls=%v", runner.Calls)
	}
	if reloadIdx == -1 {
		t.Fatalf("systemctl reload ssh was not called; calls=%v", runner.Calls)
	}
	if revertIdx >= reloadIdx {
		t.Fatalf("expected systemd-run (index %d) before systemctl reload ssh (index %d)", revertIdx, reloadIdx)
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
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl reload ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl reload ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{ExitCode: 0})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}

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
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAI test@host")
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})
	runner.On("sshd -t", sys.Result{ExitCode: 0})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl reload ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl reload ssh", sys.Result{ExitCode: 0})
	runner.On("systemctl stop cvctl-ssh-revert.timer cvctl-ssh-revert.service", sys.Result{ExitCode: 5, Stderr: []byte("Unit cvctl-ssh-revert.timer not loaded.\n")})

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
		if c.String() == "systemctl reload ssh" {
			reloadCount++
		}
	}
	if reloadCount < 2 {
		t.Fatalf("expected systemctl reload ssh to be called for apply and revert, got %d calls", reloadCount)
	}
	stopped := slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool {
		return strings.HasPrefix(c.String(), "systemctl stop cvctl-ssh-revert.timer")
	})
	if !stopped {
		t.Fatal("expected timer stop on declined confirmation")
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
	env, runner, fs := newSSHTestEnv("andres", true, true)
	env.Prompt = hostcfgtest.NewPrompter(t, map[string]string{
		"admin_ssh_pubkey": "not-a-valid-key-prefix AAAA",
	})
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", "# comment only")
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication yes\npermitrootlogin yes\n"),
		ExitCode: 0,
	})

	step := SSHStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_SSH_NO_KEYS error for invalid pubkey prefix, got nil")
	}
	if got := code(t, err); got != "E_SSH_NO_KEYS" {
		t.Fatalf("got error code %q, want E_SSH_NO_KEYS", got)
	}
}
