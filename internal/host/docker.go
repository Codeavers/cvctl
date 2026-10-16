package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

func (DockerDaemonStep) Check(_ context.Context, e *Env) (Plan, error) {
	data, err := e.FS.ReadFile(dockerDaemonConfigPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Plan{Changes: []string{"configure /etc/docker/daemon.json"}}, nil
		}
		return Plan{}, err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return Plan{Changes: []string{"configure /etc/docker/daemon.json"}}, nil
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
		return Plan{}, nil
	}

	return Plan{Changes: []string{"configure /etc/docker/daemon.json"}}, nil
}

func (DockerDaemonStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
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

	_ = e.FS.MkdirAll("/etc/docker", 0o755, sys.Owner{User: "root", Group: "root"})
	if err := e.FS.WriteFile(dockerDaemonConfigPath, out, 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
		return "", err
	}

	res, err := e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"ps", "-q"}})
	if err != nil {
		return "", err
	}
	if res.ExitCode != 0 {
		return "", fmt.Errorf("docker ps failed (exit %d): %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}

	hasContainers := strings.TrimSpace(string(res.Stdout)) != ""
	if !hasContainers {
		if err := runCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"restart", "docker"}}); err != nil {
			return "", err
		}
		return "", nil
	}

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
			if err := runCmd(ctx, e, sys.Cmd{Name: "systemctl", Args: []string{"restart", "docker"}}); err != nil {
				return "", err
			}
			return "", nil
		}
	}

	return dockerRestartPendingMsg, nil
}
