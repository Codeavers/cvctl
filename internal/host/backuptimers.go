package host

import (
	"context"
	"fmt"
	"path"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const systemdDir = "/etc/systemd/system"

type timerUnit struct {
	name     string // unit base name, without suffix
	desc     string
	command  string
	calendar string
	// timeout stops a hung wal-g or restic run; oneshot units have none by default.
	timeout string
}

var backupTimerUnits = []timerUnit{
	{name: "cvctl-backup", desc: "cvctl daily backup", command: "backup run", calendar: "*-*-* 03:30:00", timeout: "6h"},
	{name: "cvctl-backup-verify", desc: "cvctl weekly backup verification", command: "backup verify", calendar: "Sun 04:30", timeout: "4h"},
}

func (u timerUnit) serviceContent() string {
	return fmt.Sprintf(`[Unit]
Description=%s
After=network.target

[Service]
Type=oneshot
User=deployer
Group=deploy
ExecStart=%s %s
TimeoutStartSec=%s
%s`, u.desc, agentBinaryPath, u.command, u.timeout, agentSandboxDirectives)
}

func (u timerUnit) timerContent() string {
	return fmt.Sprintf(`[Unit]
Description=%s timer

[Timer]
OnCalendar=%s
Persistent=true
RandomizedDelaySec=15m

[Install]
WantedBy=timers.target
`, u.desc, u.calendar)
}

// BackupTimersStep installs systemd timers for the daily backup and weekly verification.
type BackupTimersStep struct{}

func (BackupTimersStep) Name() string { return "backup-timers" }

// UnitContents returns the generated unit files keyed by file name.
func (BackupTimersStep) UnitContents() map[string]string {
	m := map[string]string{}
	for _, u := range backupTimerUnits {
		m[u.name+".service"] = u.serviceContent()
		m[u.name+".timer"] = u.timerContent()
	}
	return m
}

func (BackupTimersStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var plan Plan
	written := false
	for _, u := range backupTimerUnits {
		for _, f := range []struct{ suffix, content string }{{".service", u.serviceContent()}, {".timer", u.timerContent()}} {
			content := f.content
			p := path.Join(systemdDir, u.name+f.suffix)
			changed, err := fileNeedsWrite(e.FS, p, []byte(content), 0o644, rootOwner)
			if err != nil {
				return Plan{}, backupTimersFSErr(p, err)
			}
			if changed {
				written = true
				plan.Changes = append(plan.Changes, fmt.Sprintf("configure %s", p))
				plan.Actions = append(plan.Actions, Action{Kind: "configure", Target: p})
			}
		}
	}
	// Written units are always followed by enable: it is idempotent and the
	// timers may be new.
	for _, u := range backupTimerUnits {
		timer := u.name + ".timer"
		res, err := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-enabled", timer}})
		out := strings.TrimSpace(string(res.Stdout))
		enabled := err == nil && res.ExitCode == 0 && (out == "" || out == "enabled")
		if !enabled || written {
			plan.Changes = append(plan.Changes, "enable "+timer)
			plan.Actions = append(plan.Actions, Action{Kind: "enable", Target: timer})
		}
	}
	return plan, nil
}

func (BackupTimersStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	var enable []string
	for _, a := range p.Actions {
		switch a.Kind {
		case "configure":
			if err := e.FS.MkdirAll(path.Dir(a.Target), 0o755, rootOwner); err != nil {
				return "", backupTimersFSErr(a.Target, err)
			}
			content, ok := backupUnitFor(a.Target)
			if !ok {
				return "", backupTimersFSErr(a.Target, fmt.Errorf("unknown unit"))
			}
			if err := e.FS.WriteFile(a.Target, []byte(content), 0o644, rootOwner); err != nil {
				return "", backupTimersFSErr(a.Target, err)
			}
		case "enable":
			enable = append(enable, a.Target)
		}
	}
	if len(p.Actions) == 0 {
		return "", nil
	}
	reload := sys.Cmd{Name: "systemctl", Args: []string{"daemon-reload"}}
	res, err := e.Run.Run(ctx, reload)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr("E_BACKUP_TIMERS_FAILED", reload, res, err, "check systemctl status")
	}
	for _, t := range enable {
		cmd := sys.Cmd{Name: "systemctl", Args: []string{"enable", "--now", t}}
		res, err := e.Run.Run(ctx, cmd)
		if err != nil || res.ExitCode != 0 {
			return "", sys.CmdErr("E_BACKUP_TIMERS_FAILED", cmd, res, err, "check systemctl status "+t)
		}
	}
	return "", nil
}

func backupUnitFor(target string) (string, bool) {
	content, ok := BackupTimersStep{}.UnitContents()[path.Base(target)]
	return content, ok
}

func backupTimersFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_BACKUP_TIMERS_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
