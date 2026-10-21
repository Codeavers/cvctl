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
	ID      string    `json:"ID"`
	Service string    `json:"Service"`
	Name    string    `json:"Name"`
	State   string    `json:"State"`
	Health  string    `json:"Health"`
	Image   string    `json:"Image"`
	Created time.Time `json:"-"`
}

// UnmarshalJSON reads Created from compose ps, which prints a unix timestamp
// in current releases and an RFC 3339 string in some older ones.
func (c *Container) UnmarshalJSON(data []byte) error {
	type plain Container
	aux := struct {
		*plain
		Created json.RawMessage `json:"Created"`
	}{plain: (*plain)(c)}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	c.Created = time.Time{}
	raw := strings.TrimSpace(string(aux.Created))
	switch {
	case raw == "" || raw == "null":
	case strings.HasPrefix(raw, `"`):
		var s string
		if err := json.Unmarshal(aux.Created, &s); err != nil {
			return err
		}
		if s == "" {
			break
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return fmt.Errorf("cannot parse container Created %q: %w", s, err)
		}
		c.Created = t
	default:
		var n int64
		if err := json.Unmarshal(aux.Created, &n); err != nil {
			return err
		}
		c.Created = time.Unix(n, 0)
	}
	return nil
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

// DefaultWaitTimeout is the --wait-timeout when the caller gives none: the
// default health timeout (30s) plus 2 minutes.
const DefaultWaitTimeout = 150 * time.Second

// UpData starts services and waits until healthy for at most waitTimeout
// (DefaultWaitTimeout when zero), rounded up to whole seconds.
func (p Project) UpData(ctx context.Context, services []string, waitTimeout time.Duration) error {
	if len(services) == 0 {
		return nil
	}
	if waitTimeout <= 0 {
		waitTimeout = DefaultWaitTimeout
	}
	secs := int((waitTimeout + time.Second - 1) / time.Second)
	args := append([]string{"up", "-d", "--wait", "--wait-timeout", strconv.Itoa(secs)}, services...)
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
	return p.ps(ctx)
}

func (p Project) ps(ctx context.Context, services ...string) ([]Container, error) {
	cmd := p.Cmd(append([]string{"ps", "--format", "json"}, services...)...)
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

// UpServices runs `up -d` for the given services only. An empty list is a
// no-op: compose would otherwise start every service.
func (p Project) UpServices(ctx context.Context, services []string, removeOrphans bool) error {
	if len(services) == 0 {
		return nil
	}
	args := []string{"up", "-d"}
	if removeOrphans {
		args = append(args, "--remove-orphans")
	}
	cmd := p.Cmd(append(args, services...)...)
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return p.composeErr(cmd, res, err)
	}
	return nil
}

// ScaleNoRecreate scales svc to n without touching its dependencies or
// recreating the containers that already run: compose keeps the old ones and
// starts new ones from the current config.
func (p Project) ScaleNoRecreate(ctx context.Context, svc string, n int) error {
	cmd := p.Cmd("up", "-d", "--no-deps", "--no-recreate", "--scale", svc+"="+strconv.Itoa(n), svc)
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return p.composeErr(cmd, res, err)
	}
	return nil
}

// Containers lists the containers of one service.
func (p Project) Containers(ctx context.Context, svc string) ([]Container, error) {
	return p.ps(ctx, svc)
}

func dockerCmd(args ...string) sys.Cmd {
	return sys.Cmd{Name: "docker", Args: args, Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}}
}

// IP returns the address of container id on the named network.
func (p Project) IP(ctx context.Context, id, network string) (string, error) {
	return containerIP(ctx, p.Run, id, network, "E_COMPOSE_FAILED", "check docker daemon status")
}

// StopRemove stops the container, giving it grace to finish in-flight work,
// and removes it.
func (p Project) StopRemove(ctx context.Context, id string, grace time.Duration) error {
	secs := int((grace + time.Second - 1) / time.Second)
	if secs < 0 {
		secs = 0
	}
	stop := dockerCmd("stop", "-t", strconv.Itoa(secs), id)
	res, err := p.Run.Run(ctx, stop)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_COMPOSE_FAILED", stop, res, err, "check docker daemon status")
	}
	return p.remove(ctx, dockerCmd("rm", id))
}

// RemoveForce kills and removes the container.
func (p Project) RemoveForce(ctx context.Context, id string) error {
	return p.remove(ctx, dockerCmd("rm", "-f", id))
}

func (p Project) remove(ctx context.Context, cmd sys.Cmd) error {
	res, err := p.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_COMPOSE_FAILED", cmd, res, err, "check docker daemon status")
	}
	return nil
}
