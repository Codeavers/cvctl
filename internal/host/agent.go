package host

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// agentSandboxDirectives is shared with the backup units so they run with the
// same privileges and filesystem view as the agent.
const agentSandboxDirectives = `NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=/srv/deploy /var/lib/cvctl /etc/traefik/dynamic
PrivateTmp=yes
SupplementaryGroups=docker
Environment=DOCKER_CONFIG=/var/lib/cvctl/docker
`

const (
	agentBinaryPath  = "/usr/local/bin/cvctl"
	agentServicePath = "/etc/systemd/system/cvctl-agent.service"
	agentUnitContent = `[Unit]
Description=cvctl deploy agent
After=network.target

[Service]
User=deployer
Group=deploy
ExecStart=/usr/local/bin/cvctl agent
Restart=always
RestartSec=10
` + agentSandboxDirectives + `TimeoutStopSec=420

[Install]
WantedBy=multi-user.target
`
)

// AgentStep ensures the cvctl binary is installed to /usr/local/bin/cvctl and the systemd unit is configured and running.
type AgentStep struct{}

func (AgentStep) Name() string { return "agent" }

func (AgentStep) UnitContent() string { return agentUnitContent }

func (AgentStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var changes []string
	var actions []Action

	exePath, err := os.Executable()
	if err != nil {
		return Plan{}, agentFSErr(agentBinaryPath, err)
	}

	exeBytes, err := e.FS.ReadFile(exePath)
	if err != nil {
		exeBytes, err = os.ReadFile(exePath)
		if err != nil {
			return Plan{}, agentFSErr(exePath, err)
		}
	}

	dstBytes, dstErr := e.FS.ReadFile(agentBinaryPath)
	binaryChanged := false
	if dstErr != nil {
		binaryChanged = true
	} else {
		hExe := sha256.Sum256(exeBytes)
		hDst := sha256.Sum256(dstBytes)
		if hExe != hDst {
			binaryChanged = true
		} else {
			fi, sErr := e.FS.Stat(agentBinaryPath)
			if sErr != nil || fi.Mode != 0o755 || fi.User != "root" || fi.Group != "root" {
				binaryChanged = true
			}
		}
	}

	if binaryChanged {
		changes = append(changes, fmt.Sprintf("install cvctl binary to %s", agentBinaryPath))
		actions = append(actions, Action{Kind: "install_binary", Target: agentBinaryPath})
	}

	unitChanged, err := fileNeedsWrite(e.FS, agentServicePath, []byte(agentUnitContent), 0o644, rootOwner)
	if err != nil {
		return Plan{}, agentFSErr(agentServicePath, err)
	}
	if unitChanged {
		changes = append(changes, fmt.Sprintf("configure %s", agentServicePath))
		actions = append(actions, Action{Kind: "configure", Target: agentServicePath})
	}

	dockerDir := "/var/lib/cvctl/docker"
	dockerDirOwner := sys.Owner{User: "deployer", Group: "deploy"}
	fi, err := e.FS.Stat(dockerDir)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && (!fi.IsDir || fi.Mode != 0o750 || fi.User != dockerDirOwner.User || fi.Group != dockerDirOwner.Group)) {
		changes = append(changes, "directory "+dockerDir)
		actions = append(actions, Action{Kind: "mkdir", Target: dockerDir})
	} else if err != nil {
		return Plan{}, agentFSErr(dockerDir, err)
	}

	enabledRes, errEnabled := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-enabled", "cvctl-agent"}})
	activeRes, errActive := e.Run.Run(ctx, sys.Cmd{Name: "systemctl", Args: []string{"is-active", "cvctl-agent"}})

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
		changes = append(changes, "enable cvctl-agent")
		actions = append(actions, Action{Kind: "enable", Target: "cvctl-agent"})
	} else if binaryChanged || unitChanged {
		changes = append(changes, "restart cvctl-agent")
		actions = append(actions, Action{Kind: "restart", Target: "cvctl-agent"})
	}

	return Plan{Changes: changes, Actions: actions}, nil
}

func (AgentStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	hasEnable := false
	hasRestart := false

	for _, a := range p.Actions {
		switch {
		case a.Kind == "mkdir":
			if err := e.FS.MkdirAll(a.Target, 0o750, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
				return "", agentFSErr(a.Target, err)
			}
		case a.Kind == "install_binary":
			exePath, err := os.Executable()
			if err != nil {
				return "", agentFSErr(agentBinaryPath, err)
			}
			exeBytes, err := e.FS.ReadFile(exePath)
			if err != nil {
				exeBytes, err = os.ReadFile(exePath)
				if err != nil {
					return "", agentFSErr(exePath, err)
				}
			}
			if err := e.FS.MkdirAll(path.Dir(agentBinaryPath), 0o755, rootOwner); err != nil {
				return "", agentFSErr(agentBinaryPath, err)
			}
			if err := e.FS.WriteFile(agentBinaryPath, exeBytes, 0o755, rootOwner); err != nil {
				return "", agentFSErr(agentBinaryPath, err)
			}
		case a.Kind == "configure":
			if err := e.FS.MkdirAll(path.Dir(agentServicePath), 0o755, rootOwner); err != nil {
				return "", agentFSErr(agentServicePath, err)
			}
			if err := e.FS.WriteFile(agentServicePath, []byte(agentUnitContent), 0o644, rootOwner); err != nil {
				return "", agentFSErr(agentServicePath, err)
			}
		case a.Kind == "enable":
			hasEnable = true
		case a.Kind == "restart":
			hasRestart = true
		}
	}

	reloadCmd := sys.Cmd{Name: "systemctl", Args: []string{"daemon-reload"}}
	res, err := e.Run.Run(ctx, reloadCmd)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr("E_AGENT_FAILED", reloadCmd, res, err, "check systemctl status cvctl-agent")
	}

	if hasEnable {
		cmd := sys.Cmd{Name: "systemctl", Args: []string{"enable", "--now", "cvctl-agent"}}
		res, err := e.Run.Run(ctx, cmd)
		if err != nil || res.ExitCode != 0 {
			return "", sys.CmdErr("E_AGENT_FAILED", cmd, res, err, "check systemctl status cvctl-agent")
		}
	} else if hasRestart {
		cmd := sys.Cmd{Name: "systemctl", Args: []string{"try-restart", "cvctl-agent"}}
		res, err := e.Run.Run(ctx, cmd)
		if err != nil || res.ExitCode != 0 {
			return "", sys.CmdErr("E_AGENT_FAILED", cmd, res, err, "check systemctl status cvctl-agent")
		}
	}

	return "", nil
}

func agentFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_AGENT_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
