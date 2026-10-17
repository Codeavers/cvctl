package sys

import (
	"context"
	"strings"
)

// ContainerState reports whether a container of a compose service exists and
// whether any is running. It asks docker by compose labels, so it works when
// the instance's compose files or env are not on disk yet.
func ContainerState(ctx context.Context, run Runner, project, service string) (exists, running bool, err error) {
	cmd := Cmd{
		Name: "docker",
		Args: []string{
			"ps", "-a",
			"--filter", "label=com.docker.compose.project=" + project,
			"--filter", "label=com.docker.compose.service=" + service,
			"--format", "{{.State}}",
		},
		Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	}
	res, runErr := run.Run(ctx, cmd)
	if runErr != nil || res.ExitCode != 0 {
		return false, false, CmdErr("E_CONTAINER_PROBE", cmd, res, runErr, "check that docker is running")
	}
	for _, line := range strings.Fields(string(res.Stdout)) {
		exists = true
		if line == "running" {
			running = true
		}
	}
	return exists, running, nil
}
