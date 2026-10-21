package host

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func newAccountsTestEnv(adminUser string) (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg := &hostcfg.Config{AdminUser: adminUser}
	return &Env{Run: r, FS: fs, Cfg: cfg, EUID: 0}, r, fs
}

func seedDesiredAccountsState(r *systest.Runner, fs *systest.FS, adminUser string) {
	r.On("getent group deploy", sys.Result{Stdout: []byte("deploy:x:998:\n"), ExitCode: 0})
	r.On("getent passwd deployer", sys.Result{Stdout: []byte("deployer:x:999:998::/srv/deploy:/bin/bash\n"), ExitCode: 0})
	r.On("id -nG deployer", sys.Result{Stdout: []byte("deployer docker deploy\n"), ExitCode: 0})
	if adminUser != "" {
		r.On("getent passwd "+adminUser, sys.Result{Stdout: []byte(adminUser + ":x:1000:1000::/home/" + adminUser + ":/bin/bash\n"), ExitCode: 0})
		r.On("id -nG "+adminUser, sys.Result{Stdout: []byte(adminUser + " sudo deploy\n"), ExitCode: 0})
	}

	deployerOwner := sys.Owner{User: "deployer", Group: "deploy"}
	rootDeploy := sys.Owner{User: "root", Group: "deploy"}

	_ = fs.MkdirAll("/srv/deploy", 0o2750, deployerOwner)
	_ = fs.MkdirAll("/etc/cvctl", 0o750, rootDeploy)
	_ = fs.MkdirAll("/etc/cvctl/secrets", 0o750, rootDeploy)
	_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)
	_ = fs.MkdirAll("/var/lib/cvctl", 0o750, deployerOwner)

	_ = fs.WriteFile("/etc/cvctl/host.env", []byte("CVCTL_HOST_NAME=box\n"), 0o640, rootDeploy)
	_ = fs.WriteFile("/etc/cvctl/secrets/telegram_token", []byte("secret-tok"), 0o640, rootDeploy)
}

func TestAccountsCreatesAll(t *testing.T) {
	env, runner, fs := newAccountsTestEnv("ops")

	rootRoot := sys.Owner{User: "root", Group: "root"}
	_ = fs.MkdirAll("/etc/cvctl", 0o700, rootRoot)
	_ = fs.MkdirAll("/etc/cvctl/secrets", 0o750, rootRoot)
	_ = fs.WriteFile("/etc/cvctl/host.env", []byte("CVCTL_HOST_NAME=box\n"), 0o640, rootRoot)
	_ = fs.WriteFile("/etc/cvctl/secrets/telegram_token", []byte("secret-tok"), 0o640, rootRoot)

	runner.On("getent group deploy", sys.Result{ExitCode: 2})
	runner.On("getent passwd deployer", sys.Result{ExitCode: 2})
	runner.On("id -nG deployer", sys.Result{ExitCode: 1, Stderr: []byte("id: 'deployer': no such user\n")})
	runner.On("getent passwd ops", sys.Result{Stdout: []byte("ops:x:1000:1000::/home/ops:/bin/bash\n"), ExitCode: 0})
	runner.On("id -nG ops", sys.Result{Stdout: []byte("ops sudo\n"), ExitCode: 0})

	runner.On("groupadd deploy", sys.Result{ExitCode: 0})
	runner.On("useradd --system --home-dir /srv/deploy --no-create-home --shell /bin/bash deployer", sys.Result{ExitCode: 0})
	runner.On("usermod -aG docker,deploy deployer", sys.Result{ExitCode: 0})
	runner.On("usermod -aG deploy ops", sys.Result{ExitCode: 0})

	step := AccountsStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes on a clean fake host, got none")
	}

	if _, err := fs.Stat("/srv/deploy"); err == nil {
		t.Fatal("Check must not create /srv/deploy")
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending, got %q", pending)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"groupadd deploy",
		"useradd --system --home-dir /srv/deploy --no-create-home --shell /bin/bash deployer",
		"usermod -aG docker,deploy deployer",
		"usermod -aG deploy ops",
	}
	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("Apply calls mismatch:\ngot:  %v\nwant: %v", got, wantCmds)
	}
	for i, want := range wantCmds {
		if applyCalls[i].String() != want {
			t.Fatalf("apply call %d: got %q, want %q", i, applyCalls[i].String(), want)
		}
	}

	files := fs.Files()
	wantEntries := map[string]struct {
		mode  os.FileMode
		user  string
		group string
		isDir bool
	}{
		"/srv/deploy":                       {0o2750, "deployer", "deploy", true},
		"/etc/cvctl":                        {0o750, "root", "deploy", true},
		"/etc/cvctl/secrets":                {0o750, "root", "deploy", true},
		"/etc/traefik/dynamic":              {0o2775, "deployer", "deploy", true},
		"/var/lib/cvctl":                    {0o750, "deployer", "deploy", true},
		"/etc/cvctl/host.env":               {0o640, "root", "deploy", false},
		"/etc/cvctl/secrets/telegram_token": {0o640, "root", "deploy", false},
	}

	for p, want := range wantEntries {
		got, ok := files[p]
		if !ok {
			t.Fatalf("expected %s to exist in FS", p)
		}
		if got.Mode != want.mode || got.User != want.user || got.Group != want.group || got.IsDir != want.isDir {
			t.Errorf("%s: got mode=%04o owner=%s:%s isDir=%v, want mode=%04o owner=%s:%s isDir=%v",
				p, got.Mode, got.User, got.Group, got.IsDir, want.mode, want.user, want.group, want.isDir)
		}
	}

	hostEnvBytes, err := fs.ReadFile("/etc/cvctl/host.env")
	if err != nil || string(hostEnvBytes) != "CVCTL_HOST_NAME=box\n" {
		t.Fatalf("host.env content changed: %q, err=%v", string(hostEnvBytes), err)
	}
	tokBytes, err := fs.ReadFile("/etc/cvctl/secrets/telegram_token")
	if err != nil || string(tokBytes) != "secret-tok" {
		t.Fatalf("telegram_token content changed: %q, err=%v", string(tokBytes), err)
	}
	if _, ok := files["/etc/cvctl/secrets/s3_secret_key"]; ok {
		t.Fatal("unset secret s3_secret_key should not have been created")
	}
}

func TestAccountsUnchanged(t *testing.T) {
	env, runner, fs := newAccountsTestEnv("ops")
	seedDesiredAccountsState(runner, fs, "ops")

	step := AccountsStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes, got %v", plan.Changes)
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("unexpected Apply result: pending=%q err=%v", pending, err)
	}
	if len(runner.Calls) != callsBeforeApply {
		t.Fatalf("expected no commands in Apply when unchanged, got %v", runner.Calls[callsBeforeApply:])
	}
}

func TestAccountsFixesModes(t *testing.T) {
	env, runner, fs := newAccountsTestEnv("ops")
	seedDesiredAccountsState(runner, fs, "ops")

	// Drift /srv/deploy to 2775; sshd StrictModes rejects deployer key if group-writable.
	_ = fs.MkdirAll("/srv/deploy", 0o2775, sys.Owner{User: "deployer", Group: "deploy"})

	step := AccountsStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when /srv/deploy has mode 2775, got none")
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("unexpected Apply result: pending=%q err=%v", pending, err)
	}
	if len(runner.Calls) != callsBeforeApply {
		t.Fatalf("expected no command calls when only directory mode drifted, got %v", runner.Calls[callsBeforeApply:])
	}

	fi, err := fs.Stat("/srv/deploy")
	if err != nil {
		t.Fatalf("stat /srv/deploy failed: %v", err)
	}
	if fi.Mode != 0o2750 {
		t.Fatalf("/srv/deploy mode: got %04o, want 2750", fi.Mode)
	}
	if fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("/srv/deploy owner: got %s:%s, want deployer:deploy", fi.User, fi.Group)
	}
}

func TestAccountsAdminUserSkippedWhenMissing(t *testing.T) {
	env, runner, fs := newAccountsTestEnv("ghost")
	seedDesiredAccountsState(runner, fs, "")
	runner.On("getent passwd ghost", sys.Result{ExitCode: 2})

	step := AccountsStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when admin user does not exist, got %v", plan.Changes)
	}
}

func TestAccountsCommandFailure(t *testing.T) {
	env, runner, _ := newAccountsTestEnv("")
	runner.On("getent group deploy", sys.Result{ExitCode: 2})
	runner.On("getent passwd deployer", sys.Result{ExitCode: 2})
	runner.On("id -nG deployer", sys.Result{ExitCode: 1})
	runner.On("groupadd deploy", sys.Result{
		ExitCode: 10,
		Stderr:   []byte("groupadd: cannot lock /etc/group; try again later.\n"),
	})

	step := AccountsStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected error when groupadd fails, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %v", err)
	}
	if oe.Code != "E_ACCOUNTS_FAILED" {
		t.Errorf("got code %q, want E_ACCOUNTS_FAILED", oe.Code)
	}
	if !strings.Contains(oe.Message, "cannot lock /etc/group") {
		t.Errorf("expected stderr in message, got %q", oe.Message)
	}
	if !strings.Contains(oe.Hint, "groupadd deploy") {
		t.Errorf("expected hint to name command 'groupadd deploy', got %q", oe.Hint)
	}
}

func TestAccountsRegisteredInDefaultSteps(t *testing.T) {
	steps := DefaultSteps()
	if len(steps) < 4 {
		t.Fatalf("expected at least 4 steps in DefaultSteps, got %d", len(steps))
	}
	if steps[2].Name() != "docker-daemon" {
		t.Fatalf("step 2: got %q, want docker-daemon", steps[2].Name())
	}
	if steps[3].Name() != "accounts" {
		t.Fatalf("step 3: got %q, want accounts", steps[3].Name())
	}
}

func TestAccountsIdempotentAfterHostcfgSave(t *testing.T) {
	env, runner, fs := newAccountsTestEnv("ops")
	seedDesiredAccountsState(runner, fs, "ops")

	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatalf("unexpected Load error: %v", err)
	}
	if err := hostcfg.Save(fs, "/etc/cvctl", cfg, sec); err != nil {
		t.Fatalf("unexpected Save error: %v", err)
	}

	step := AccountsStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes after hostcfg.Save on desired state, got: %v", plan.Changes)
	}
}
