package compose

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

type intervalKey struct{}

// DefaultHealthInterval is the default interval between health check attempts.
const DefaultHealthInterval = 2 * time.Second

// HealthInterval is the package-level default interval, overrideable in tests.
var HealthInterval = DefaultHealthInterval

// WithHealthInterval returns a new context with the given health polling interval.
func WithHealthInterval(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, intervalKey{}, d)
}

func Health(ctx context.Context, run sys.Runner, p Project, h contract.HealthSpec, port int, client *http.Client) error {
	if run == nil {
		run = p.Run
	}
	if p.Run == nil {
		p.Run = run
	}
	if client == nil {
		client = http.DefaultClient
	}

	containers, err := p.PS(ctx)
	if err != nil {
		return err
	}

	var target *Container
	for i := range containers {
		if containers[i].Service == h.Service && strings.EqualFold(containers[i].State, "running") {
			target = &containers[i]
			break
		}
	}
	if target == nil {
		return &output.Error{
			Code:    "E_HEALTH_FAILED",
			Message: fmt.Sprintf("no running container found for health service %s", h.Service),
			Hint:    fmt.Sprintf("check status with `cvctl status %s` or `docker compose ps`", p.Instance),
		}
	}

	inspectNetCmd := sys.Cmd{
		Name: "docker",
		Args: []string{"inspect", "-f", "{{json .NetworkSettings.Networks}}", target.Name},
	}
	netRes, err := run.Run(ctx, inspectNetCmd)
	if err != nil || netRes.ExitCode != 0 {
		return sys.CmdErr("E_HEALTH_FAILED", inspectNetCmd, netRes, err, "check docker daemon status")
	}

	var networks map[string]struct {
		IPAddress string `json:"IPAddress"`
	}
	if err := json.Unmarshal(netRes.Stdout, &networks); err != nil {
		return &output.Error{
			Code:    "E_HEALTH_FAILED",
			Message: fmt.Sprintf("cannot parse network settings for %s: %v", target.Name, err),
			Hint:    "check docker inspect output",
		}
	}

	netName := p.Instance + "_app"
	netSetting, ok := networks[netName]
	ip := strings.TrimSpace(netSetting.IPAddress)
	if !ok || ip == "" {
		return &output.Error{
			Code:    "E_HEALTH_FAILED",
			Message: fmt.Sprintf("container %s has no IP on network %s", target.Name, netName),
			Hint:    fmt.Sprintf("check network settings for %s", target.Name),
		}
	}

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	interval := HealthInterval
	if d, ok := ctx.Value(intervalKey{}).(time.Duration); ok && d > 0 {
		interval = d
	}
	if interval <= 0 {
		interval = DefaultHealthInterval
	}

	path := h.HTTP
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	healthURL := fmt.Sprintf("http://%s:%d%s", ip, port, path)

	inspectRestartCmd := sys.Cmd{
		Name: "docker",
		Args: []string{"inspect", "-f", "{{.RestartCount}}", target.Name},
	}

	checkRestarts := func() error {
		res, err := run.Run(ctx, inspectRestartCmd)
		if err != nil || res.ExitCode != 0 {
			return sys.CmdErr("E_HEALTH_FAILED", inspectRestartCmd, res, err, "check container status")
		}
		count, _ := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
		if count > 0 {
			return &output.Error{
				Code:    "E_HEALTH_FAILED",
				Message: fmt.Sprintf("container %s restarted", target.Name),
				Hint:    fmt.Sprintf("check logs with `cvctl logs %s %s`", p.Instance, h.Service),
			}
		}
		return nil
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr string
	for {
		if err := checkRestarts(); err != nil {
			return err
		}

		req, err := http.NewRequestWithContext(ctxTimeout, http.MethodGet, healthURL, nil)
		if err != nil {
			return &output.Error{
				Code:    "E_HEALTH_FAILED",
				Message: fmt.Sprintf("cannot create health request: %v", err),
				Hint:    "check health configuration",
			}
		}

		resp, err := client.Do(req)
		if err == nil {
			status := resp.StatusCode
			_ = resp.Body.Close()
			if status >= 200 && status < 300 {
				if err := checkRestarts(); err != nil {
					return err
				}
				return nil
			}
			lastErr = fmt.Sprintf("HTTP status %d", status)
		} else {
			lastErr = err.Error()
		}

		select {
		case <-ctxTimeout.Done():
			return &output.Error{
				Code:    "E_HEALTH_FAILED",
				Message: fmt.Sprintf("health check timed out after %s for %s (%s): %s", timeout, target.Name, healthURL, lastErr),
				Hint:    fmt.Sprintf("check container logs with `cvctl logs %s %s`", p.Instance, h.Service),
			}
		case <-time.After(interval):
		}
	}
}
