package host_test

import (
	"context"
	"testing"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const sandbox = `NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/srv/deploy /var/lib/cvctl /etc/traefik/dynamic
PrivateTmp=yes
SupplementaryGroups=docker
Environment=DOCKER_CONFIG=/var/lib/cvctl/docker
`

const goldenBackupService = `[Unit]
Description=cvctl daily backup
After=network.target

[Service]
Type=oneshot
User=deployer
Group=deploy
ExecStart=/usr/local/bin/cvctl backup run
TimeoutStartSec=6h
` + sandbox

const goldenVerifyService = `[Unit]
Description=cvctl weekly backup verification
After=network.target

[Service]
Type=oneshot
User=deployer
Group=deploy
ExecStart=/usr/local/bin/cvctl backup verify
TimeoutStartSec=4h
` + sandbox

const goldenBackupTimer = `[Unit]
Description=cvctl daily backup timer

[Timer]
OnCalendar=*-*-* 03:30:00
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
`

const goldenVerifyTimer = `[Unit]
Description=cvctl weekly backup verification timer

[Timer]
OnCalendar=Sun 04:30
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
`

func TestBackupTimersGoldenUnits(t *testing.T) {
	got := host.BackupTimersStep{}.UnitContents()
	want := map[string]string{
		"cvctl-backup.service":        goldenBackupService,
		"cvctl-backup.timer":          goldenBackupTimer,
		"cvctl-backup-verify.service": goldenVerifyService,
		"cvctl-backup-verify.timer":   goldenVerifyTimer,
	}
	for name, w := range want {
		if got[name] != w {
			t.Errorf("%s differs:\ngot:\n%s\nwant:\n%s", name, got[name], w)
		}
	}
}

func TestBackupTimersApplyThenUnchanged(t *testing.T) {
	fsys := systest.NewFS()
	run := &systest.Runner{}
	run.On("systemctl daemon-reload", sys.Result{})
	run.On("systemctl enable --now cvctl-backup.timer", sys.Result{})
	run.On("systemctl enable --now cvctl-backup-verify.timer", sys.Result{})
	run.On("systemctl is-enabled cvctl-backup.timer", sys.Result{ExitCode: 1})
	run.On("systemctl is-enabled cvctl-backup-verify.timer", sys.Result{ExitCode: 1})
	e := &host.Env{Run: run, FS: fsys}
	step := host.BackupTimersStep{}
	ctx := context.Background()

	plan, err := step.Check(ctx, e)
	if err != nil {
		t.Fatal(err)
	}
	if len(fsys.Files()) != 0 {
		t.Fatal("Check must not write")
	}
	if _, err := step.Apply(ctx, e, plan); err != nil {
		t.Fatal(err)
	}
	for name, want := range step.UnitContents() {
		data, err := fsys.ReadFile("/etc/systemd/system/" + name)
		if err != nil || string(data) != want {
			t.Fatalf("%s not written correctly: %v", name, err)
		}
		fi, _ := fsys.Stat("/etc/systemd/system/" + name)
		if fi.Mode != 0o644 || fi.User != "root" || fi.Group != "root" {
			t.Fatalf("%s bad perms: %#o %s:%s", name, fi.Mode, fi.User, fi.Group)
		}
	}
	enables := 0
	for _, c := range run.Calls {
		if c.String() == "systemctl enable --now cvctl-backup.timer" || c.String() == "systemctl enable --now cvctl-backup-verify.timer" {
			enables++
		}
	}
	if enables != 2 {
		t.Fatalf("expected 2 enable calls, got %d", enables)
	}

	run2 := &systest.Runner{}
	run2.On("systemctl is-enabled cvctl-backup.timer", sys.Result{Stdout: []byte("enabled\n")})
	run2.On("systemctl is-enabled cvctl-backup-verify.timer", sys.Result{Stdout: []byte("enabled\n")})
	plan, err = step.Check(ctx, &host.Env{Run: run2, FS: fsys})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Changes) != 0 || len(plan.Actions) != 0 {
		t.Fatalf("expected no changes, got %v", plan.Changes)
	}
}
