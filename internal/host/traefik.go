package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"slices"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	ProxyNetwork      = "cvctl-edge-proxy"
	CertResolver      = "le"
	DefaultMiddleware = "cvctl-default@file"
	EntryWeb          = "web"
	EntryWebSecure    = "websecure"

	traefikImage = "traefik:v3.7.15"

	traefikDir         = "/etc/cvctl/traefik"
	traefikStaticPath  = "/etc/cvctl/traefik/traefik.yml"
	traefikComposePath = "/etc/cvctl/traefik/compose.yml"
	traefikDynamicPath = "/etc/traefik/dynamic/_cvctl.yml"
	traefikSecretsPath = "/etc/cvctl/secrets/traefik.env"
	traefikProject     = "cvctl-traefik"
	traefikContainer   = "cvctl-traefik"

	defaultTraefikPollInterval = 2 * time.Second
	defaultTraefikPollTimeout  = 60 * time.Second
)

type TraefikStep struct {
	PollInterval time.Duration
	PollTimeout  time.Duration
}

func (TraefikStep) Name() string { return "traefik" }

type traefikRenderedFiles struct {
	staticBytes  []byte
	dynamicBytes []byte
	composeBytes []byte
	envBytes     []byte
	hasEnv       bool
}

func renderAllTraefikFiles(e *Env) (traefikRenderedFiles, error) {
	staticBytes, err := renderTraefikStatic(e.Cfg)
	if err != nil {
		return traefikRenderedFiles{}, err
	}
	dynBytes, err := renderTraefikDynamic(e.Cfg)
	if err != nil {
		return traefikRenderedFiles{}, err
	}
	composeBytes, err := renderTraefikCompose(e.Cfg)
	if err != nil {
		return traefikRenderedFiles{}, err
	}
	envBytes, hasEnv, err := renderTraefikEnv(e.Cfg, e.Secrets)
	if err != nil {
		return traefikRenderedFiles{}, err
	}
	return traefikRenderedFiles{
		staticBytes:  staticBytes,
		dynamicBytes: dynBytes,
		composeBytes: composeBytes,
		envBytes:     envBytes,
		hasEnv:       hasEnv,
	}, nil
}

func (TraefikStep) Check(ctx context.Context, e *Env) (Plan, error) {
	rf, err := renderAllTraefikFiles(e)
	if err != nil {
		return Plan{}, err
	}

	var changes []string
	var actions []Action
	for _, spec := range []struct {
		path string
		data []byte
	}{
		{traefikStaticPath, rf.staticBytes},
		{traefikDynamicPath, rf.dynamicBytes},
		{traefikComposePath, rf.composeBytes},
	} {
		needsWrite, err := fileNeedsWrite(e.FS, spec.path, spec.data, 0o640, rootDeployOwner)
		if err != nil {
			return Plan{}, traefikFSErr(spec.path, err)
		}
		if needsWrite {
			changes = append(changes, "configure "+spec.path)
			actions = append(actions, Action{Kind: "configure", Target: spec.path})
		}
	}

	if rf.hasEnv {
		needsWrite, err := fileNeedsWrite(e.FS, traefikSecretsPath, rf.envBytes, 0o640, rootDeployOwner)
		if err != nil {
			return Plan{}, traefikFSErr(traefikSecretsPath, err)
		}
		if needsWrite {
			changes = append(changes, "configure "+traefikSecretsPath)
			actions = append(actions, Action{Kind: "configure", Target: traefikSecretsPath})
		}
	} else {
		_, err := e.FS.Stat(traefikSecretsPath)
		if err == nil {
			changes = append(changes, "remove "+traefikSecretsPath)
			actions = append(actions, Action{Kind: "remove", Target: traefikSecretsPath})
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Plan{}, traefikFSErr(traefikSecretsPath, err)
		}
	}

	gwOut, netOK := dockerOut(ctx, e, "network", "inspect", "-f",
		"{{range .IPAM.Config}}{{.Gateway}} {{end}}", ProxyNetwork)
	if !netOK {
		changes = append(changes, "create network "+ProxyNetwork)
		actions = append(actions, Action{Kind: "network-create", Target: ProxyNetwork})
	} else if want := e.Cfg.EdgeGateway(); !slices.Contains(strings.Fields(gwOut), want) {
		return Plan{}, &output.Error{
			Code:    "E_TRAEFIK_EDGE_SUBNET",
			Message: fmt.Sprintf("network %s has gateway %q, want %s", ProxyNetwork, strings.TrimSpace(gwOut), want),
			Hint:    fmt.Sprintf("stop Traefik, run `docker network rm %s` and re-run `cvctl init`", ProxyNetwork),
		}
	}

	if networkExists(ctx, e, legacyEdgeNetwork) {
		changes = append(changes, "retire network "+legacyEdgeNetwork)
		actions = append(actions, Action{Kind: "network-remove", Target: legacyEdgeNetwork})
	}

	connected := connectedNetworks(ctx, e, traefikContainer)
	for _, n := range managedNetworks(ctx, e, "_edge") {
		if !connected[n] {
			changes = append(changes, "connect traefik to "+n)
			actions = append(actions, Action{Kind: "reconnect", Target: n})
		}
	}

	psRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "docker",
		Args: []string{"compose", "-p", traefikProject, "-f", traefikComposePath, "ps", "--format", "json"},
		Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	})
	if err != nil || psRes.ExitCode != 0 || !isTraefikRunningWithImage(psRes.Stdout, traefikImage) {
		changes = append(changes, "start traefik")
		actions = append(actions, Action{Kind: "start", Target: "traefik"})
	}

	return Plan{Changes: changes, Actions: actions}, nil
}

func (s TraefikStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Actions) == 0 {
		return "", nil
	}

	rf, err := renderAllTraefikFiles(e)
	if err != nil {
		return "", err
	}

	var hasNetwork, hasStatic, hasCompose, hasDynamic, hasSecrets, hasRemoveSecrets bool
	for _, a := range p.Actions {
		switch {
		case a.Kind == "network-create" && a.Target == ProxyNetwork:
			hasNetwork = true
		case a.Kind == "configure" && a.Target == traefikStaticPath:
			hasStatic = true
		case a.Kind == "configure" && a.Target == traefikComposePath:
			hasCompose = true
		case a.Kind == "configure" && a.Target == traefikDynamicPath:
			hasDynamic = true
		case a.Kind == "configure" && a.Target == traefikSecretsPath:
			hasSecrets = true
		case a.Kind == "remove" && a.Target == traefikSecretsPath:
			hasRemoveSecrets = true
		}
	}

	if hasNetwork {
		if err := runTraefikCmd(ctx, e, sys.Cmd{
			Name: "docker",
			Args: []string{"network", "create", "--label", "cvctl.managed=true",
				"--subnet", e.Cfg.EdgeSubnetOrDefault(), "--gateway", e.Cfg.EdgeGateway(), ProxyNetwork},
			Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
		}); err != nil {
			return "", err
		}
	}

	if hasStatic || hasCompose {
		if err := e.FS.MkdirAll(traefikDir, 0o750, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikDir, err)
		}
	}
	if hasStatic {
		if err := e.FS.WriteFile(traefikStaticPath, rf.staticBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikStaticPath, err)
		}
	}
	if hasCompose {
		if err := e.FS.WriteFile(traefikComposePath, rf.composeBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikComposePath, err)
		}
	}

	if hasDynamic {
		// Ensure /etc/traefik is 0755 root:root so deployer can traverse into /etc/traefik/dynamic;
		// do not touch /etc/traefik/dynamic itself (owned by deployer:deploy 02775 from accounts step).
		if err := e.FS.MkdirAll("/etc/traefik", 0o755, rootOwner); err != nil {
			return "", traefikFSErr("/etc/traefik", err)
		}
		if err := e.FS.WriteFile(traefikDynamicPath, rf.dynamicBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikDynamicPath, err)
		}
	}

	if hasSecrets {
		if err := e.FS.MkdirAll("/etc/cvctl/secrets", 0o750, rootDeployOwner); err != nil {
			return "", traefikFSErr("/etc/cvctl/secrets", err)
		}
		if err := e.FS.WriteFile(traefikSecretsPath, rf.envBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikSecretsPath, err)
		}
	} else if hasRemoveSecrets {
		if err := e.FS.Remove(traefikSecretsPath); err != nil {
			return "", traefikFSErr(traefikSecretsPath, err)
		}
	}

	upArgs := []string{"compose", "-p", traefikProject, "-f", traefikComposePath, "up", "-d"}
	// traefik.yml is bind-mounted as a single file (atomic replacement swaps its inode)
	// and static config or env changes require restarting the container.
	if hasStatic || hasSecrets || hasRemoveSecrets {
		upArgs = append(upArgs, "--force-recreate")
	}
	if err := runTraefikCmd(ctx, e, sys.Cmd{
		Name: "docker",
		Args: upArgs,
		Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	}); err != nil {
		return "", err
	}

	for _, n := range managedNetworks(ctx, e, "_edge") {
		if err := sys.ConnectNetwork(ctx, e.Run, n, traefikContainer); err != nil {
			return "", err
		}
	}

	warn, err := retireLegacyNetwork(ctx, e, legacyEdgeNetwork, traefikContainer)
	if err != nil {
		return "", err
	}

	if err := s.waitForHealthy(ctx, e); err != nil {
		return "", err
	}

	return warn, nil
}

func (s TraefikStep) waitForHealthy(ctx context.Context, e *Env) error {
	interval := s.PollInterval
	if interval <= 0 {
		interval = defaultTraefikPollInterval
	}
	timeout := s.PollTimeout
	if timeout <= 0 {
		timeout = defaultTraefikPollTimeout
	}

	inspectCmd := sys.Cmd{
		Name: "docker",
		Args: []string{"inspect", "-f", "{{.State.Health.Status}}", traefikContainer},
		Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	}

	deadline := time.Now().Add(timeout)
	for {
		res, err := e.Run.Run(ctx, inspectCmd)
		if err != nil {
			return err
		}
		if res.ExitCode == 0 && strings.TrimSpace(string(res.Stdout)) == "healthy" {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}

	logRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "docker",
		Args: []string{"logs", "--tail", "20", traefikContainer},
		Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	})
	if err != nil {
		return err
	}
	logs := lastLines(string(logRes.Stdout)+string(logRes.Stderr), 20)
	msg := fmt.Sprintf("traefik container %s did not become healthy within %s", traefikContainer, timeout)
	if logs != "" {
		msg += ": " + logs
	}
	return &output.Error{
		Code:    "E_TRAEFIK_UNHEALTHY",
		Message: msg,
		Hint:    fmt.Sprintf("check `docker logs %s` and %s", traefikContainer, traefikStaticPath),
	}
}

func lastLines(s string, n int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

type composePSItem struct {
	Service string `json:"Service"`
	State   string `json:"State"`
	Image   string `json:"Image"`
}

func isTraefikRunningWithImage(stdout []byte, wantImage string) bool {
	trimmed := strings.TrimSpace(string(stdout))
	if trimmed == "" {
		return false
	}
	var items []composePSItem
	if strings.HasPrefix(trimmed, "[") {
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return false
		}
	} else {
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var item composePSItem
			if err := json.Unmarshal([]byte(line), &item); err != nil {
				return false
			}
			items = append(items, item)
		}
	}
	for _, it := range items {
		if it.Service == "traefik" && it.State == "running" && it.Image == wantImage {
			return true
		}
	}
	return false
}

func runTraefikCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return traefikCmdFailed(c, res)
	}
	return nil
}

func traefikCmdFailed(c sys.Cmd, res sys.Result) error {
	return sys.CmdErr("E_TRAEFIK_FAILED", c, res, nil, fmt.Sprintf("check `%s` output and docker status", c.String()))
}

func traefikFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_TRAEFIK_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
