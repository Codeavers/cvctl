package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func newFail2banTestEnv() (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	return &Env{Run: r, FS: fs, EUID: 0}, r, fs
}

func TestFail2banWritesJailAndEnables(t *testing.T) {
	env, runner, fs := newFail2banTestEnv()
	runner.On("systemctl is-enabled fail2ban", sys.Result{Stdout: []byte("disabled\n"), ExitCode: 1})
	runner.On("systemctl is-active fail2ban", sys.Result{Stdout: []byte("inactive\n"), ExitCode: 3})
	runner.On("systemctl enable --now fail2ban", sys.Result{ExitCode: 0})

	step := Fail2banStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when jail file is missing and fail2ban is disabled, got none")
	}
	if _, err := fs.Stat(fail2banJailPath); err == nil {
		t.Fatal("Check must not write jail file")
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending, got %q", pending)
	}

	data, err := fs.ReadFile(fail2banJailPath)
	if err != nil {
		t.Fatalf("expected %s to be written: %v", fail2banJailPath, err)
	}
	want := "[sshd]\nenabled = true\nbackend = systemd\nmaxretry = 5\nfindtime = 10m\nbantime = 1h\n"
	if string(data) != want {
		t.Fatalf("jail content mismatch:\ngot:\n%q\nwant:\n%q", string(data), want)
	}

	fi, err := fs.Stat(fail2banJailPath)
	if err != nil {
		t.Fatalf("stat %s: %v", fail2banJailPath, err)
	}
	if fi.Mode != 0o644 || fi.User != "root" || fi.Group != "root" {
		t.Fatalf("jail file meta: got mode=%04o owner=%s:%s, want 0644 root:root", fi.Mode, fi.User, fi.Group)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	if len(applyCalls) != 1 || applyCalls[0].String() != "systemctl enable --now fail2ban" {
		t.Fatalf("expected only [systemctl enable --now fail2ban] in Apply, got %v", applyCalls)
	}
}

func TestFail2banUnchanged(t *testing.T) {
	env, runner, fs := newFail2banTestEnv()
	_ = fs.WriteFile(fail2banJailPath, []byte(fail2banJailContent), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("systemctl is-enabled fail2ban", sys.Result{Stdout: []byte("enabled\n"), ExitCode: 0})
	runner.On("systemctl is-active fail2ban", sys.Result{Stdout: []byte("active\n"), ExitCode: 0})

	step := Fail2banStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when jail matches and service is enabled+active, got %v", plan.Changes)
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("unexpected Apply result: pending=%q err=%v", pending, err)
	}
	if len(runner.Calls) != callsBeforeApply {
		t.Fatalf("expected no Apply calls when unchanged, got %v", runner.Calls[callsBeforeApply:])
	}
}

func TestFail2banReloadsWhenJailChangedAndAlreadyActive(t *testing.T) {
	env, runner, fs := newFail2banTestEnv()
	_ = fs.WriteFile(fail2banJailPath, []byte("[sshd]\nenabled = false\n"), 0o644, sys.Owner{User: "root", Group: "root"})
	runner.On("systemctl is-enabled fail2ban", sys.Result{Stdout: []byte("enabled\n"), ExitCode: 0})
	runner.On("systemctl is-active fail2ban", sys.Result{Stdout: []byte("active\n"), ExitCode: 0})
	runner.On("systemctl try-reload-or-restart fail2ban", sys.Result{ExitCode: 0})

	step := Fail2banStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when jail differs")
	}

	callsBeforeApply := len(runner.Calls)
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	if len(applyCalls) != 1 || applyCalls[0].String() != "systemctl try-reload-or-restart fail2ban" {
		t.Fatalf("expected [systemctl try-reload-or-restart fail2ban], got %v", applyCalls)
	}
	if slices.ContainsFunc(applyCalls, func(c sys.Cmd) bool { return c.String() == "systemctl enable --now fail2ban" }) {
		t.Fatal("did not expect systemctl enable --now fail2ban when already enabled and active")
	}
}

func TestFail2banCommandFailure(t *testing.T) {
	env, runner, _ := newFail2banTestEnv()
	runner.On("systemctl is-enabled fail2ban", sys.Result{Stdout: []byte("disabled\n"), ExitCode: 1})
	runner.On("systemctl is-active fail2ban", sys.Result{Stdout: []byte("inactive\n"), ExitCode: 3})
	runner.On("systemctl enable --now fail2ban", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("Failed to enable unit: Unit file fail2ban.service does not exist.\n"),
	})

	step := Fail2banStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_FAIL2BAN_FAILED error, got nil")
	}
	if got := code(t, err); got != "E_FAIL2BAN_FAILED" {
		t.Fatalf("got code %q, want E_FAIL2BAN_FAILED", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if !strings.Contains(oe.Message, "Unit file fail2ban.service does not exist") {
		t.Fatalf("expected stderr in error message, got %q", oe.Message)
	}
	if !strings.Contains(oe.Hint, "systemctl enable --now fail2ban") {
		t.Fatalf("expected command in hint, got %q", oe.Hint)
	}
}
