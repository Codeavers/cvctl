package host

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"

	"github.com/codeavers/cvctl/internal/sys"
)

const (
	legacyEdgeNetwork   = "cvctl-edge"
	legacyBackupNetwork = "cvctl-backup"
	siloContainer       = "cvctl-storage-s3-1"
)

var dockerEnv = []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}

func dockerOut(ctx context.Context, e *Env, args ...string) (string, bool) {
	res, err := e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: args, Env: dockerEnv})
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	return string(res.Stdout), true
}

// managedNetworks lists the cvctl-managed instance networks ending in suffix.
// The docker name filter is a substring match, so the suffix is rechecked.
func managedNetworks(ctx context.Context, e *Env, suffix string) []string {
	out, ok := dockerOut(ctx, e, "network", "ls", "--filter", "label=cvctl.managed=true",
		"--filter", "name="+suffix, "--format", "{{.Name}}")
	if !ok {
		return nil
	}
	var nets []string
	for _, n := range strings.Fields(out) {
		if strings.HasSuffix(n, suffix) {
			nets = append(nets, n)
		}
	}
	slices.Sort(nets)
	return nets
}

// connectedNetworks returns the networks a container is attached to; a missing
// container yields none.
func connectedNetworks(ctx context.Context, e *Env, container string) map[string]bool {
	out, _ := dockerOut(ctx, e, "inspect", "-f",
		"{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}", container)
	set := map[string]bool{}
	for _, n := range strings.Fields(out) {
		set[n] = true
	}
	return set
}

// dockerMissing is true before the docker step has installed the binary.
func dockerMissing(err error) bool {
	return err != nil && (errors.Is(err, exec.ErrNotFound) || strings.Contains(err.Error(), "executable file not found"))
}

// networkUsers inspects a network. A missing network is (false, nil); any
// other docker failure (daemon down, permissions) is an error so it is never
// mistaken for "absent".
func networkUsers(ctx context.Context, e *Env, name string) (bool, []string, error) {
	cmd := sys.Cmd{Name: "docker", Env: dockerEnv, Args: []string{"network", "inspect", "-f",
		"{{range $k, $v := .Containers}}{{$v.Name}} {{end}}", name}}
	res, err := e.Run.Run(ctx, cmd)
	if dockerMissing(err) {
		return false, nil, nil
	}
	if err != nil {
		return false, nil, sys.CmdErr("E_NETWORK", cmd, res, err, "check that docker is running")
	}
	if res.ExitCode != 0 {
		if strings.Contains(string(res.Stderr), "No such network") {
			return false, nil, nil
		}
		return false, nil, sys.CmdErr("E_NETWORK", cmd, res, nil, "check that docker is running")
	}
	return true, strings.Fields(string(res.Stdout)), nil
}

// planLegacyRemoval returns the action to retire a pre-instance shared network
// when only own still uses it. A network held by other containers yields a
// pending warning and no action, so repeated runs stay quiet on changes.
func planLegacyRemoval(ctx context.Context, e *Env, name, own string) (*Action, string, error) {
	exists, users, err := networkUsers(ctx, e, name)
	if err != nil || !exists {
		return nil, "", err
	}
	var others []string
	for _, u := range users {
		if u != own {
			others = append(others, u)
		}
	}
	if len(others) > 0 {
		return nil, fmt.Sprintf("legacy network %s still used by %s; remove it once they are gone", name, strings.Join(others, ", ")), nil
	}
	return &Action{Kind: "network-remove", Target: name}, "", nil
}

// retireLegacyNetwork detaches own from the legacy network and removes it
// when nothing else uses it; otherwise it leaves it and returns a warning.
func retireLegacyNetwork(ctx context.Context, e *Env, name, own string) string {
	exists, _, err := networkUsers(ctx, e, name)
	if err != nil || !exists {
		return ""
	}
	if connectedNetworks(ctx, e, own)[name] {
		_, _ = dockerOut(ctx, e, "network", "disconnect", name, own)
	}
	_, users, _ := networkUsers(ctx, e, name)
	if len(users) > 0 {
		return fmt.Sprintf("legacy network %s still used by %s; remove it once they are gone", name, strings.Join(users, ", "))
	}
	if _, ok := dockerOut(ctx, e, "network", "rm", name); !ok {
		return fmt.Sprintf("legacy network %s could not be removed; run `docker network rm %s`", name, name)
	}
	return ""
}
