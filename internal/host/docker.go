package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const dockerDaemonConfigPath = "/etc/docker/daemon.json"
const dockerRestartPendingMsg = "daemon.json updated; run: systemctl restart docker (running containers will restart)"

type DockerDaemonStep struct{}

func (DockerDaemonStep) Name() string { return "docker-daemon" }

func isDaemonConfigDesired(m map[string]any) bool {
	if m["log-driver"] != "local" {
		return false
	}
	if m["live-restore"] != true {
		return false
	}
	logOpts, ok := m["log-opts"].(map[string]any)
	if !ok || logOpts == nil {
		return false
	}
	if logOpts["max-size"] != "20m" || logOpts["max-file"] != "5" {
		return false
	}
	return true
}

// minDockerMajor is the oldest Docker Engine cvctl supports: Traefik relies
// on endpoint gw_priority (Engine 28) to publish its ports through the proxy
// network gateway that trustedIPs names.
const minDockerMajor = 28

// checkDockerVersion fails on an engine older than minDockerMajor. An engine
// that cannot be queried (not installed yet, daemon down) is left to the
// steps that need it.
func checkDockerVersion(ctx context.Context, e *Env) error {
	res, err := e.Run.Run(ctx, sys.Cmd{
		Name: "docker",
		Args: []string{"version", "--format", "{{.Server.Version}}"},
		Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	})
	if err != nil || res.ExitCode != 0 {
		return nil
	}
	v := strings.TrimSpace(string(res.Stdout))
	major, err := strconv.Atoi(strings.SplitN(v, ".", 2)[0])
	if err != nil || major >= minDockerMajor {
		return nil
	}
	return &output.Error{
		Code:    "E_DOCKER_TOO_OLD",
		Message: fmt.Sprintf("Docker Engine %s is older than %d", v, minDockerMajor),
		Hint:    fmt.Sprintf("upgrade Docker Engine to %d+", minDockerMajor),
	}
}

func (DockerDaemonStep) Check(ctx context.Context, e *Env) (Plan, error) {
	if err := checkDockerVersion(ctx, e); err != nil {
		return Plan{}, err
	}
	data, err := e.FS.ReadFile(dockerDaemonConfigPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Plan{
				Changes: []string{"configure /etc/docker/daemon.json"},
				Actions: []Action{{Kind: "configure", Target: dockerDaemonConfigPath}},
			}, nil
		}
		return Plan{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Plan{
			Changes: []string{"configure /etc/docker/daemon.json"},
			Actions: []Action{{Kind: "configure", Target: dockerDaemonConfigPath}},
		}, nil
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return Plan{}, &output.Error{
			Code:    "E_DOCKER_DAEMON_JSON",
			Message: "invalid /etc/docker/daemon.json: " + err.Error(),
			Hint:    "fix or remove /etc/docker/daemon.json",
		}
	}

	if isDaemonConfigDesired(m) {
		res, err := e.Run.Run(ctx, sys.Cmd{
			Name: "docker",
			Args: []string{"info", "--format", "{{.LoggingDriver}} {{.LiveRestoreEnabled}}"},
			Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
		})
		if err != nil || res.ExitCode != 0 || strings.TrimSpace(string(res.Stdout)) != "local true" {
			return Plan{Pending: dockerRestartPendingMsg}, nil
		}
		return Plan{}, nil
	}

	return Plan{
		Changes: []string{"configure /etc/docker/daemon.json"},
		Actions: []Action{{Kind: "configure", Target: dockerDaemonConfigPath}},
	}, nil
}

func (DockerDaemonStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Actions) == 0 {
		return "", nil
	}

	var m map[string]any
	data, err := e.FS.ReadFile(dockerDaemonConfigPath)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err == nil && len(strings.TrimSpace(string(data))) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return "", &output.Error{
				Code:    "E_DOCKER_DAEMON_JSON",
				Message: "invalid /etc/docker/daemon.json: " + err.Error(),
				Hint:    "fix or remove /etc/docker/daemon.json",
			}
		}
	}
	if m == nil {
		m = make(map[string]any)
	}

	m["log-driver"] = "local"
	m["live-restore"] = true

	logOpts, ok := m["log-opts"].(map[string]any)
	if !ok || logOpts == nil {
		logOpts = make(map[string]any)
	}
	logOpts["max-size"] = "20m"
	logOpts["max-file"] = "5"
	m["log-opts"] = logOpts

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return "", err
	}
	out = append(out, '\n')

	if err := e.FS.MkdirAll("/etc/docker", 0o755, sys.Owner{User: "root", Group: "root"}); err != nil {
		return "", err
	}
	if err := e.FS.WriteFile(dockerDaemonConfigPath, out, 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
		return "", err
	}

	dockerPsCmd := sys.Cmd{Name: "docker", Args: []string{"ps", "-q"}, Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}}
	res, err := e.Run.Run(ctx, dockerPsCmd)
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", sys.CmdErr("E_DOCKER_PS_FAILED", dockerPsCmd, res, nil, "systemctl status docker")
	}

	hasContainers := strings.TrimSpace(string(res.Stdout)) != ""
	// On an adopted host, or when containers are running, restarting Docker always requires confirmation.
	needsConfirm := hasContainers || e.Adopt
	if needsConfirm {
		if e.Interactive && e.Prompt != nil {
			ans, err := e.Prompt.Ask(hostcfg.Question{
				Key:     "confirm_docker_restart",
				Prompt:  "Restart Docker daemon now?",
				Help:    "Running containers will restart.",
				Options: []string{"yes", "no"},
			}, "no")
			if err != nil {
				return "", err
			}
			if ans == "yes" {
				if err := restartDocker(ctx, e); err != nil {
					return "", err
				}
				return "", nil
			}
		}
		return dockerRestartPendingMsg, nil
	}

	if err := restartDocker(ctx, e); err != nil {
		return "", err
	}
	return "", nil
}

func restartDocker(ctx context.Context, e *Env) error {
	systemctlCmd := sys.Cmd{Name: "systemctl", Args: []string{"restart", "docker"}}
	res, err := e.Run.Run(ctx, systemctlCmd)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return sys.CmdErr("E_DOCKER_RESTART_FAILED", systemctlCmd, res, nil, "check systemctl status docker")
	}
	return nil
}
