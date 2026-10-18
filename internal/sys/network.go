package sys

import (
	"context"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
)

func dockerCmd(args ...string) Cmd {
	return Cmd{Name: "docker", Args: args, Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}}
}

// EnsureNetwork creates the docker network when missing. A concurrent creator
// winning the race counts as success. An existing network whose internal flag
// differs is an error: recreating it would cut the containers attached to it.
func EnsureNetwork(ctx context.Context, run Runner, name string, internal bool) error {
	res, err := run.Run(ctx, dockerCmd("network", "inspect", "-f", "{{.Internal}}", name))
	if err == nil && res.ExitCode == 0 {
		got := strings.TrimSpace(string(res.Stdout))
		if got == "" || got == strconv.FormatBool(internal) {
			return nil
		}
		return &output.Error{
			Code:    "E_NETWORK",
			Message: "network " + name + " has internal=" + got + ", want " + strconv.FormatBool(internal),
			Hint:    "the storage mode changed: cvctl pause the instance, docker network rm " + name + ", then cvctl deploy",
		}
	}
	args := []string{"network", "create", "--label", "cvctl.managed=true"}
	if internal {
		args = append(args, "--internal")
	}
	cmd := dockerCmd(append(args, name)...)
	res, err = run.Run(ctx, cmd)
	if err == nil && res.ExitCode == 0 {
		return nil
	}
	if err == nil && strings.Contains(string(res.Stderr), "already exists") {
		return nil
	}
	return CmdErr("E_NETWORK", cmd, res, err, "check that docker is running")
}

// ConnectNetwork attaches a container to a network with the given aliases.
// Already being connected counts as success.
func ConnectNetwork(ctx context.Context, run Runner, network, container string, aliases ...string) error {
	args := []string{"network", "connect"}
	for _, a := range aliases {
		args = append(args, "--alias", a)
	}
	cmd := dockerCmd(append(args, network, container)...)
	res, err := run.Run(ctx, cmd)
	if err == nil && res.ExitCode == 0 {
		return nil
	}
	if err == nil && strings.Contains(string(res.Stderr), "already exists in network") {
		return nil
	}
	return CmdErr("E_NETWORK", cmd, res, err, "check that the container is running")
}
