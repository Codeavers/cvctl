package host

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type writeErrFS struct {
	*systest.FS
	writeErr error
}

func (w *writeErrFS) WriteFile(p string, data []byte, mode os.FileMode, o sys.Owner) error {
	return w.writeErr
}

func newUnattendedTestEnv() (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	seedOS(fs, ubuntu2404)
	return &Env{Run: r, FS: fs, EUID: 0}, r, fs
}

func TestUnattendedFiles(t *testing.T) {
	env, runner, fs := newUnattendedTestEnv()
	existing50 := "Unattended-Upgrade::Allowed-Origins {\n\t\"${distro_id}:${distro_codename}-security\";\n};\n"
	_ = fs.WriteFile("/etc/apt/apt.conf.d/50unattended-upgrades", []byte(existing50), 0o644, sys.Owner{User: "root", Group: "root"})

	step := UnattendedStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when unattended-upgrades config files are missing, got none")
	}
	if _, err := fs.Stat(autoUpgradesPath); err == nil {
		t.Fatal("Check must not write 20auto-upgrades")
	}
	if _, err := fs.Stat(cvctlUnattendedPath); err == nil {
		t.Fatal("Check must not write 52cvctl-unattended")
	}

	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending, got %q", pending)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no service/command calls for unattended-upgrades, got %v", runner.Calls)
	}

	wantAuto := "APT::Periodic::Update-Package-Lists \"1\";\nAPT::Periodic::Unattended-Upgrade \"1\";\n"
	gotAuto, err := fs.ReadFile(autoUpgradesPath)
	if err != nil {
		t.Fatalf("read %s: %v", autoUpgradesPath, err)
	}
	if string(gotAuto) != wantAuto {
		t.Fatalf("20auto-upgrades mismatch:\ngot:\n%q\nwant:\n%q", string(gotAuto), wantAuto)
	}
	fiAuto, err := fs.Stat(autoUpgradesPath)
	if err != nil {
		t.Fatalf("stat %s: %v", autoUpgradesPath, err)
	}
	if fiAuto.Mode != 0o644 || fiAuto.User != "root" || fiAuto.Group != "root" {
		t.Fatalf("20auto-upgrades meta: got mode=%04o owner=%s:%s, want 0644 root:root", fiAuto.Mode, fiAuto.User, fiAuto.Group)
	}

	wantCvctl := "Unattended-Upgrade::Automatic-Reboot \"false\";\n"
	gotCvctl, err := fs.ReadFile(cvctlUnattendedPath)
	if err != nil {
		t.Fatalf("read %s: %v", cvctlUnattendedPath, err)
	}
	if string(gotCvctl) != wantCvctl {
		t.Fatalf("52cvctl-unattended mismatch:\ngot:\n%q\nwant:\n%q", string(gotCvctl), wantCvctl)
	}
	fiCvctl, err := fs.Stat(cvctlUnattendedPath)
	if err != nil {
		t.Fatalf("stat %s: %v", cvctlUnattendedPath, err)
	}
	if fiCvctl.Mode != 0o644 || fiCvctl.User != "root" || fiCvctl.Group != "root" {
		t.Fatalf("52cvctl-unattended meta: got mode=%04o owner=%s:%s, want 0644 root:root", fiCvctl.Mode, fiCvctl.User, fiCvctl.Group)
	}

	got50, err := fs.ReadFile("/etc/apt/apt.conf.d/50unattended-upgrades")
	if err != nil || string(got50) != existing50 {
		t.Fatalf("50unattended-upgrades must not be touched; got %q err=%v", string(got50), err)
	}

	plan2, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected second Check error: %v", err)
	}
	if len(plan2.Changes) != 0 {
		t.Fatalf("expected no changes on second Check, got %v", plan2.Changes)
	}
}

func TestUnattendedWriteError(t *testing.T) {
	env, _, fs := newUnattendedTestEnv()
	env.FS = &writeErrFS{FS: fs, writeErr: errors.New("disk full")}

	step := UnattendedStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_UNATTENDED_FAILED error, got nil")
	}
	if got := code(t, err); got != "E_UNATTENDED_FAILED" {
		t.Fatalf("got code %q, want E_UNATTENDED_FAILED", got)
	}
}

func TestTask7StepsRegisteredInDefaultSteps(t *testing.T) {
	steps := DefaultSteps()
	if len(steps) < 8 {
		t.Fatalf("expected at least 8 steps in DefaultSteps, got %d", len(steps))
	}
	wantNames := []string{
		"preflight",
		"packages",
		"docker-daemon",
		"accounts",
		"ssh",
		"fail2ban",
		"firewall",
		"unattended-upgrades",
	}
	for i, want := range wantNames {
		if steps[i].Name() != want {
			t.Fatalf("step %d: got %q, want %q", i, steps[i].Name(), want)
		}
	}
}
