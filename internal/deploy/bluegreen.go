package deploy

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
)

const (
	// routeSettle is how long the pinned route has to reach Traefik before the
	// old container is stopped; its file watcher reloads in milliseconds.
	routeSettle = 2 * time.Second
	// defaultStopGrace applies when the service sets no stop_grace_period.
	defaultStopGrace = 10 * time.Second
)

// SwapAPI is what a blue-green swap needs beyond ComposeAPI. *compose.Project
// implements it; a blue-green deploy fails without it.
type SwapAPI interface {
	Up(ctx context.Context) error
	UpServices(ctx context.Context, services []string, removeOrphans bool) error
	UpServicesNoDeps(ctx context.Context, services []string, removeOrphans bool) error
	ScaleNoRecreate(ctx context.Context, svc string, n int) error
	Containers(ctx context.Context, svc string) ([]compose.Container, error)
	IP(ctx context.Context, id, network string) (string, error)
	CheckContainer(ctx context.Context, h contract.HealthSpec, port int, client *http.Client, id string, httpCheck bool) error
	StopRemove(ctx context.Context, id string, grace time.Duration) error
	RemoveForce(ctx context.Context, id string) error
}

func (d *Deployer) sleep(ctx context.Context, dur time.Duration) error {
	if d.Sleep != nil {
		return d.Sleep(ctx, dur)
	}
	t := time.NewTimer(dur)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// exposedApps lists the app services that receive traffic, in contract order.
func exposedApps(p *contract.Project) []contract.AppService {
	var out []contract.AppService
	for _, a := range p.Apps {
		if a.Role == "app" && a.ExposePort > 0 {
			out = append(out, a)
		}
	}
	return out
}

// bringUp starts the project for the strategy and returns the services that
// were swapped without downtime.
func (d *Deployer) bringUp(ctx context.Context, inst string, p *contract.Project, dec *instance.Decisions, c ComposeAPI) ([]string, error) {
	if dec.Strategy != "blue-green" || len(exposedApps(p)) == 0 {
		return nil, c.Up(ctx)
	}
	sw, ok := c.(SwapAPI)
	if !ok {
		return nil, &output.Error{
			Code:    "E_DEPLOY_FAILED",
			Message: "blue-green needs the compose swap API",
			Hint:    "this is a bug in cvctl; report it",
		}
	}
	return d.swapBlueGreen(ctx, inst, p, dec, sw)
}

// swapBlueGreen replaces each exposed service by a second container of the
// same compose service: the old one keeps serving until the new one is healthy
// and the route is pinned to it. Everything else is brought up as usual.
func (d *Deployer) swapBlueGreen(ctx context.Context, inst string, p *contract.Project, dec *instance.Decisions, c SwapAPI) ([]string, error) {
	exposed := exposedApps(p)
	isExposed := map[string]bool{}
	for _, a := range exposed {
		isExposed[a.Name] = true
	}
	var others []string
	for name := range p.Compose.Services {
		if !isExposed[name] {
			others = append(others, name)
		}
	}
	sort.Strings(others)
	// --no-deps: depends_on must not start or recreate an exposed service
	// before its swap. Data services were started by UpData already.
	if err := c.UpServicesNoDeps(ctx, others, true); err != nil {
		return nil, err
	}

	var swapped []string
	for _, a := range exposed {
		did, err := d.swapService(ctx, inst, p, dec, c, a)
		if did {
			swapped = append(swapped, a.Name)
		}
		if err != nil {
			return swapped, err
		}
	}
	// Removes orphans; the swapped containers already match their config, so
	// it leaves them alone.
	return swapped, c.Up(ctx)
}

// swapService reports whether the service was swapped, as opposed to started
// plainly because nothing ran.
func (d *Deployer) swapService(ctx context.Context, inst string, p *contract.Project, dec *instance.Decisions, c SwapAPI, a contract.AppService) (bool, error) {
	cs, err := c.Containers(ctx, a.Name)
	if err != nil {
		return false, err
	}
	var running []compose.Container
	for _, ct := range cs {
		if strings.EqualFold(ct.State, "running") {
			running = append(running, ct)
		}
	}
	if len(running) == 0 {
		return false, c.UpServices(ctx, []string{a.Name}, false)
	}
	// The oldest container is the one st.Current describes; a crashed swap
	// left the others behind.
	sort.SliceStable(running, func(i, j int) bool { return running[i].Created.Before(running[j].Created) })
	old := running[0]
	for _, ct := range cs {
		if ct.ID != old.ID {
			if err := c.RemoveForce(ctx, ct.ID); err != nil {
				return false, err
			}
		}
	}

	// dropNew removes what the scale created. The route was not pinned to it,
	// or was already put back, so the old container keeps serving.
	dropNew := func(cause error) error {
		after, lerr := c.Containers(context.WithoutCancel(ctx), a.Name)
		if lerr == nil {
			for _, ct := range after {
				if ct.ID != old.ID {
					_ = c.RemoveForce(context.WithoutCancel(ctx), ct.ID)
				}
			}
		}
		return cause
	}

	if err := c.ScaleNoRecreate(ctx, a.Name, 2); err != nil {
		return false, dropNew(err)
	}
	after, err := c.Containers(ctx, a.Name)
	if err != nil {
		return false, dropNew(err)
	}
	var fresh *compose.Container
	for i := range after {
		if after[i].ID == old.ID {
			continue
		}
		if fresh == nil || after[i].Created.After(fresh.Created) {
			fresh = &after[i]
		}
	}
	if fresh == nil {
		return false, fmt.Errorf("blue-green: compose created no new container for %s", a.Name)
	}

	httpCheck := a.Name == p.X.Health.Service
	port := a.ExposePort
	if httpCheck {
		port = p.HealthPort()
	}
	h := p.X.Health
	h.Service = a.Name
	if err := c.CheckContainer(ctx, h, port, nil, fresh.ID, httpCheck); err != nil {
		return false, dropNew(err)
	}
	ip, err := c.IP(ctx, fresh.ID, render.NetworkEdge(inst))
	if err != nil {
		return false, dropNew(err)
	}

	pin := map[string]string{a.Name: fmt.Sprintf("http://%s:%d", ip, a.ExposePort)}
	if err := d.WriteRoute(p, dec, pin); err != nil {
		return false, dropNew(err)
	}
	if err := d.sleep(ctx, routeSettle); err != nil {
		_ = d.WriteRoute(p, dec, nil)
		return false, dropNew(err)
	}
	if err := c.StopRemove(ctx, old.ID, stopGrace(p, a.Name)); err != nil {
		_ = d.WriteRoute(p, dec, nil)
		return true, err
	}
	return true, d.WriteRoute(p, dec, nil)
}

func stopGrace(p *contract.Project, svc string) time.Duration {
	if s, ok := p.Compose.Services[svc]; ok && s.StopGracePeriod != nil {
		return time.Duration(*s.StopGracePeriod)
	}
	return defaultStopGrace
}
