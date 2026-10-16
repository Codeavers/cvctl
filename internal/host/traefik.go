package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	EdgeNetwork       = "cvctl-edge"
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
		}
	}

	if rf.hasEnv {
		needsWrite, err := fileNeedsWrite(e.FS, traefikSecretsPath, rf.envBytes, 0o640, rootDeployOwner)
		if err != nil {
			return Plan{}, traefikFSErr(traefikSecretsPath, err)
		}
		if needsWrite {
			changes = append(changes, "configure "+traefikSecretsPath)
		}
	} else {
		_, err := e.FS.Stat(traefikSecretsPath)
		if err == nil {
			changes = append(changes, "remove "+traefikSecretsPath)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return Plan{}, traefikFSErr(traefikSecretsPath, err)
		}
	}

	netRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "docker",
		Args: []string{"network", "inspect", EdgeNetwork},
	})
	if err != nil || netRes.ExitCode != 0 {
		changes = append(changes, "create network "+EdgeNetwork)
	}

	psRes, err := e.Run.Run(ctx, sys.Cmd{
		Name: "docker",
		Args: []string{"compose", "-p", traefikProject, "-f", traefikComposePath, "ps", "--format", "json"},
	})
	if err != nil || psRes.ExitCode != 0 || !isTraefikRunningWithImage(psRes.Stdout, traefikImage) {
		changes = append(changes, "start traefik")
	}

	return Plan{Changes: changes}, nil
}

func (s TraefikStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	rf, err := renderAllTraefikFiles(e)
	if err != nil {
		return "", err
	}

	changeSet := make(map[string]bool, len(p.Changes))
	for _, c := range p.Changes {
		changeSet[c] = true
	}

	if changeSet["create network "+EdgeNetwork] {
		if err := runTraefikCmd(ctx, e, sys.Cmd{
			Name: "docker",
			Args: []string{"network", "create", EdgeNetwork},
		}); err != nil {
			return "", err
		}
	}

	if changeSet["configure "+traefikStaticPath] || changeSet["configure "+traefikComposePath] {
		if err := e.FS.MkdirAll(traefikDir, 0o750, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikDir, err)
		}
	}
	if changeSet["configure "+traefikStaticPath] {
		if err := e.FS.WriteFile(traefikStaticPath, rf.staticBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikStaticPath, err)
		}
	}
	if changeSet["configure "+traefikComposePath] {
		if err := e.FS.WriteFile(traefikComposePath, rf.composeBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikComposePath, err)
		}
	}

	if changeSet["configure "+traefikDynamicPath] {
		// Ensure /etc/traefik is 0755 root:root so deployer can traverse into /etc/traefik/dynamic;
		// do not touch /etc/traefik/dynamic itself (owned by deployer:deploy 02775 from accounts step).
		if err := e.FS.MkdirAll("/etc/traefik", 0o755, rootOwner); err != nil {
			return "", traefikFSErr("/etc/traefik", err)
		}
		if err := e.FS.WriteFile(traefikDynamicPath, rf.dynamicBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikDynamicPath, err)
		}
	}

	if changeSet["configure "+traefikSecretsPath] {
		if err := e.FS.MkdirAll("/etc/cvctl/secrets", 0o750, rootDeployOwner); err != nil {
			return "", traefikFSErr("/etc/cvctl/secrets", err)
		}
		if err := e.FS.WriteFile(traefikSecretsPath, rf.envBytes, 0o640, rootDeployOwner); err != nil {
			return "", traefikFSErr(traefikSecretsPath, err)
		}
	} else if changeSet["remove "+traefikSecretsPath] {
		if err := e.FS.Remove(traefikSecretsPath); err != nil {
			return "", traefikFSErr(traefikSecretsPath, err)
		}
	}

	upArgs := []string{"compose", "-p", traefikProject, "-f", traefikComposePath, "up", "-d"}
	// traefik.yml is bind-mounted as a single file (atomic replacement swaps its inode)
	// and static config or env changes require restarting the container.
	if changeSet["configure "+traefikStaticPath] || changeSet["configure "+traefikSecretsPath] || changeSet["remove "+traefikSecretsPath] {
		upArgs = append(upArgs, "--force-recreate")
	}
	if err := runTraefikCmd(ctx, e, sys.Cmd{
		Name: "docker",
		Args: upArgs,
	}); err != nil {
		return "", err
	}

	if err := s.waitForHealthy(ctx, e); err != nil {
		return "", err
	}

	return "", nil
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
	stderr := strings.TrimSpace(string(res.Stderr))
	msg := fmt.Sprintf("%s failed (exit %d)", c.String(), res.ExitCode)
	if stderr != "" {
		msg += ": " + stderr
	}
	return &output.Error{
		Code:    "E_TRAEFIK_FAILED",
		Message: msg,
		Hint:    fmt.Sprintf("check `%s` output and docker status", c.String()),
	}
}

func traefikFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_TRAEFIK_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
