package compose

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

type Project struct {
	Run      sys.Runner
	Instance string
	Dir      string
	Override string
	EnvFile  string
}

type Container struct {
	Service string `json:"Service"`
	Name    string `json:"Name"`
	State   string `json:"State"`
	Health  string `json:"Health"`
	Image   string `json:"Image"`
}

func (p Project) Cmd(args ...string) sys.Cmd {
	envFile := p.EnvFile
	if envFile == "" {
		envFile = filepath.Join(p.Dir, "app.env")
	}
	override := p.Override
	if override == "" {
		override = filepath.Join(p.Dir, "compose.cvctl.yml")
	}
	baseArgs := []string{"compose", "-p", p.Instance, "--env-file", envFile, "-f", override}
	return sys.Cmd{
		Name: "docker",
		Args: append(baseArgs, args...),
		Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	}
}

// hookSuffix is a variable so tests can pin the container name.
var hookSuffix = func() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (p Project) composeErr(cmd sys.Cmd, res sys.Result, runErr error) *output.Error {
	return sys.CmdErr("E_COMPOSE_FAILED", cmd, res, runErr, fmt.Sprintf("see the command output; check the compose files in %s", p.Dir))
}

func (p Project) Config(ctx context.Context) error {
	cmd := p.Cmd("config", "-q")
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return p.composeErr(cmd, res, err)
	}
	return nil
}

func (p Project) Pull(ctx context.Context, services ...string) error {
	args := append([]string{"pull"}, services...)
	cmd := p.Cmd(args...)
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return p.composeErr(cmd, res, err)
	}
	return nil
}

func (p Project) UpData(ctx context.Context, services []string) error {
	if len(services) == 0 {
		return nil
	}
	args := append([]string{"up", "-d", "--wait"}, services...)
	cmd := p.Cmd(args...)
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return p.composeErr(cmd, res, err)
	}
	return nil
}

func (p Project) RunHook(ctx context.Context, service, script string) (sys.Result, error) {
	// The name lets us remove the container: killing the docker CLI on timeout
	// or cancel leaves the run container alive.
	name := p.Instance + "-hook-" + hookSuffix()
	cmd := p.Cmd("run", "--rm", "--no-deps", "-T", "--name", name, "--entrypoint", "sh", service, "-c", script)
	res, err := p.Run.Run(ctx, cmd)
	if ctx.Err() != nil {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		rm := sys.Cmd{Name: "docker", Args: []string{"rm", "-f", name}, Env: cmd.Env}
		_, _ = p.Run.Run(rmCtx, rm)
	}
	if err != nil || res.ExitCode != 0 {
		return res, sys.CmdErr("E_HOOK_FAILED", cmd, res, err, fmt.Sprintf("see the command output; check the hook script for %s", service))
	}
	return res, nil
}

// ExecScript runs script with sh inside the running container of service. The
// script goes on stdin so secrets it carries never appear in argv.
func (p Project) ExecScript(ctx context.Context, service, script string) (sys.Result, error) {
	cmd := p.Cmd("exec", "-T", service, "sh", "-s")
	cmd.Stdin = []byte(script)
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return res, p.composeErr(cmd, res, err)
	}
	return res, nil
}

func (p Project) Up(ctx context.Context) error {
	cmd := p.Cmd("up", "-d", "--remove-orphans")
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return p.composeErr(cmd, res, err)
	}
	return nil
}

func (p Project) PS(ctx context.Context) ([]Container, error) {
	cmd := p.Cmd("ps", "--format", "json")
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return nil, p.composeErr(cmd, res, err)
	}
	raw := strings.TrimSpace(string(res.Stdout))
	if raw == "" {
		return nil, nil
	}
	var containers []Container
	if strings.HasPrefix(raw, "[") {
		if err := json.Unmarshal([]byte(raw), &containers); err != nil {
			return nil, &output.Error{
				Code:    "E_COMPOSE_FAILED",
				Message: fmt.Sprintf("cannot parse compose ps output: %v", err),
				Hint:    fmt.Sprintf("see the command output; check the compose files in %s", p.Dir),
			}
		}
		return containers, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	for {
		var c Container
		err := dec.Decode(&c)
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, &output.Error{
				Code:    "E_COMPOSE_FAILED",
				Message: fmt.Sprintf("cannot parse compose ps output: %v", err),
				Hint:    fmt.Sprintf("see the command output; check the compose files in %s", p.Dir),
			}
		}
		containers = append(containers, c)
	}
	return containers, nil
}

func (p Project) Logs(ctx context.Context, service string, tail int) (string, error) {
	args := []string{"logs"}
	if tail > 0 {
		args = append(args, "--tail", strconv.Itoa(tail))
	}
	if service != "" {
		args = append(args, service)
	}
	cmd := p.Cmd(args...)
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return "", p.composeErr(cmd, res, err)
	}
	out := string(res.Stdout)
	if out == "" {
		out = string(res.Stderr)
	}
	return out, nil
}
