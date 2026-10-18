package host

import (
	"context"
	"fmt"
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

func networkExists(ctx context.Context, e *Env, name string) bool {
	_, ok := dockerOut(ctx, e, "network", "inspect", name)
	return ok
}

// retireLegacyNetwork detaches own from a pre-instance shared network and
// removes the network when nothing else uses it. A network still in use is
// left alone and reported as a warning.
func retireLegacyNetwork(ctx context.Context, e *Env, name, own string) (string, error) {
	if !networkExists(ctx, e, name) {
		return "", nil
	}
	if connectedNetworks(ctx, e, own)[name] {
		_, _ = dockerOut(ctx, e, "network", "disconnect", name, own)
	}
	out, _ := dockerOut(ctx, e, "network", "inspect", "-f",
		"{{range $k, $v := .Containers}}{{$v.Name}} {{end}}", name)
	if users := strings.Fields(out); len(users) > 0 {
		return fmt.Sprintf("legacy network %s still used by %s; remove it once they are gone", name, strings.Join(users, ", ")), nil
	}
	if _, ok := dockerOut(ctx, e, "network", "rm", name); !ok {
		return fmt.Sprintf("legacy network %s could not be removed; run `docker network rm %s`", name, name), nil
	}
	return "", nil
}

func joinWarnings(ws ...string) string {
	var out []string
	for _, w := range ws {
		if w != "" {
			out = append(out, w)
		}
	}
	return strings.Join(out, "; ")
}
