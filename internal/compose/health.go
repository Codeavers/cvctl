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

func normalizeProject(instance string) string {
	lower := strings.ToLower(instance)
	var sb strings.Builder
	for _, r := range lower {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}

func Health(ctx context.Context, run sys.Runner, p Project, h contract.HealthSpec, port int, client *http.Client) error {
	return checkHealth(ctx, run, p, h, port, client, "", true)
}

// CheckContainer is Health for the single container id: the other containers
// of the project, including the old one during a blue-green swap, are ignored.
// With httpCheck false only the running and restart checks apply.
func CheckContainer(ctx context.Context, run sys.Runner, p Project, h contract.HealthSpec, port int, client *http.Client, id string, httpCheck bool) error {
	if id == "" {
		return &output.Error{Code: "E_HEALTH_FAILED", Message: "no container ID given", Hint: "this is a bug in cvctl; report it"}
	}
	return checkHealth(ctx, run, p, h, port, client, id, httpCheck)
}

// checkHealth checks the whole project, or only the container with ID id when
// id is not empty.
func checkHealth(ctx context.Context, run sys.Runner, p Project, h contract.HealthSpec, port int, client *http.Client, id string, httpCheck bool) error {
	if run == nil {
		run = p.Run
	}
	if p.Run == nil {
		p.Run = run
	}
	if client == nil {
		client = &http.Client{
			Timeout: 3 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
	}

	list := func() ([]Container, error) {
		all, err := p.PS(ctx)
		if err != nil || id == "" {
			return all, err
		}
		var own []Container
		for _, c := range all {
			if c.ID == id {
				own = append(own, c)
			}
		}
		return own, nil
	}

	containers, err := list()
	if err != nil {
		return err
	}

	var target *Container
	for i := range containers {
		if id != "" || (containers[i].Service == h.Service && strings.EqualFold(containers[i].State, "running")) {
			target = &containers[i]
			break
		}
	}
	if target == nil {
		msg := fmt.Sprintf("no running container found for health service %s", h.Service)
		if id != "" {
			msg = fmt.Sprintf("container %s not found for service %s", id, h.Service)
		}
		return &output.Error{
			Code:    "E_HEALTH_FAILED",
			Message: msg,
			Hint:    fmt.Sprintf("check status with `cvctl status %s` or `docker compose ps`", p.Instance),
		}
	}

	ref := target.Name
	if id != "" {
		ref = id
	}
	var ip string
	if httpCheck {
		netName := normalizeProject(p.Instance) + "_app"
		var err error
		ip, err = containerIP(ctx, run, ref, netName, "E_HEALTH_FAILED", "check docker daemon status")
		if err != nil {
			return err
		}
	}

	timeout := h.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}

	interval := healthIntervalFor(ctx)

	path := h.HTTP
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	healthURL := fmt.Sprintf("http://%s:%d%s", ip, port, path)
	if !httpCheck {
		healthURL = ""
	}

	// Baseline restart counts are recorded once, right after Up, so containers
	// that restarted before this deploy do not count as failures.
	baseline := map[string]int{}
	restartCount := func(name string) (int, bool, error) {
		cmd := sys.Cmd{
			Name: "docker",
			Args: []string{"inspect", "-f", "{{.RestartCount}}", name},
			Env:  []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
		}
		res, err := run.Run(ctx, cmd)
		if err != nil || res.ExitCode != 0 {
			return 0, false, sys.CmdErr("E_HEALTH_FAILED", cmd, res, err, "check container status")
		}
		count, convErr := strconv.Atoi(strings.TrimSpace(string(res.Stdout)))
		if convErr != nil {
			return 0, false, nil
		}
		return count, true, nil
	}
	healthFail := func(msg string) error {
		return &output.Error{
			Code:    "E_HEALTH_FAILED",
			Message: msg,
			Hint:    fmt.Sprintf("check logs with `cvctl logs %s %s`", p.Instance, h.Service),
		}
	}
	// checkContainers fails when any container is not running or restarted
	// beyond its baseline. It returns a non-empty note when a count is unreadable.
	checkContainers := func(list []Container) (string, error) {
		note := ""
		for _, c := range list {
			if !strings.EqualFold(c.State, "running") {
				return "", healthFail(fmt.Sprintf("container %s is not running (state %q)", c.Name, c.State))
			}
			count, ok, err := restartCount(c.Name)
			if err != nil {
				return "", err
			}
			if !ok {
				note = fmt.Sprintf("container %s restart count is unknown", c.Name)
				continue
			}
			base, seen := baseline[c.Name]
			if !seen {
				baseline[c.Name] = count
				continue
			}
			if count > base {
				return "", healthFail(fmt.Sprintf("container %s restarted", c.Name))
			}
		}
		return note, nil
	}

	if _, err := checkContainers(containers); err != nil {
		return err
	}
	if !httpCheck {
		// Baseline taken above; a second pass catches a container that
		// crashed on start.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthIntervalFor(ctx)):
		}
		after, err := list()
		if err != nil {
			return err
		}
		_, err = checkContainers(after)
		return err
	}

	ctxTimeout, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var lastErr string
	for {
		current, err := list()
		if err != nil {
			return err
		}
		note, err := checkContainers(current)
		if err != nil {
			return err
		}
		if note != "" {
			lastErr = note
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
				after, err := list()
				if err != nil {
					return err
				}
				note, err := checkContainers(after)
				if err != nil {
					return err
				}
				if note == "" {
					return nil
				}
				lastErr = note
			} else {
				lastErr = fmt.Sprintf("HTTP status %d", status)
			}
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

func healthIntervalFor(ctx context.Context) time.Duration {
	interval := HealthInterval
	if d, ok := ctx.Value(intervalKey{}).(time.Duration); ok && d > 0 {
		interval = d
	}
	if interval <= 0 {
		interval = DefaultHealthInterval
	}
	return interval
}

// containerIP returns the address of container ref on network. Errors carry code.
func containerIP(ctx context.Context, run sys.Runner, ref, network, code, hint string) (string, error) {
	cmd := dockerCmd("inspect", "-f", "{{json .NetworkSettings.Networks}}", ref)
	res, err := run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr(code, cmd, res, err, hint)
	}
	var networks map[string]struct {
		IPAddress string `json:"IPAddress"`
	}
	if err := json.Unmarshal(res.Stdout, &networks); err != nil {
		return "", &output.Error{
			Code:    code,
			Message: fmt.Sprintf("cannot parse network settings for %s: %v", ref, err),
			Hint:    "check docker inspect output",
		}
	}
	setting, ok := networks[network]
	ip := strings.TrimSpace(setting.IPAddress)
	if !ok || ip == "" {
		return "", &output.Error{
			Code:    code,
			Message: fmt.Sprintf("container %s has no IP on network %s", ref, network),
			Hint:    fmt.Sprintf("check network settings for %s", ref),
		}
	}
	return ip, nil
}
