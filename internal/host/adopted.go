package host

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"

	"github.com/codeavers/cvctl/internal/adopt"
	"github.com/codeavers/cvctl/internal/sys"
)

const dynamicDir = "/etc/traefik/dynamic"

// AdoptedStep keeps adopted routes and their ufw rules in line with the edge
// mode and subnet, which only init changes.
type AdoptedStep struct{}

func (AdoptedStep) Name() string { return "adopted" }

// adoptedFirewall is what reconciling the ufw rules of adopted services takes.
type adoptedFirewall struct {
	allow, remove []adopt.Rule
	// records holds the new record of each entry whose record changes.
	records map[string]*adopt.Rule
}

func (f adoptedFirewall) changes() []string {
	var out []string
	for _, r := range f.allow {
		out = append(out, "ufw allow "+r.String())
	}
	for _, r := range f.remove {
		out = append(out, "ufw delete allow "+r.String())
	}
	for _, name := range slices.Sorted(maps.Keys(f.records)) {
		out = append(out, "update firewall record of adopted "+name)
	}
	return out
}

// planAdoptedFirewall wants the current subnet's rule for every adopted port.
// A rule already in ufw that no entry recorded is the operator's: it is used
// but never recorded, so cvctl never deletes it. Only recorded rules for
// another subnet are deleted.
func planAdoptedFirewall(ctx context.Context, e *Env, svcs []adopt.Service) (adoptedFirewall, error) {
	var f adoptedFirewall
	if e.Cfg == nil || !e.Cfg.FirewallManage || len(svcs) == 0 || !hasCommand(ctx, e, "ufw") {
		return f, nil
	}
	c := sys.Cmd{Name: "ufw", Args: []string{"show", "added"}, Env: []string{"LC_ALL=C"}}
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return f, err
	}
	if res.ExitCode != 0 {
		return f, firewallCmdFailed(c, res)
	}
	present := adopt.ParseAdded(res.Stdout)
	subnet := e.Cfg.EdgeSubnetOrDefault()

	owned := map[adopt.Rule]bool{}
	for _, s := range svcs {
		want := adopt.RuleFor(subnet, s.Port())
		if _, seen := owned[want]; seen {
			continue
		}
		if slices.Contains(present, want) {
			owned[want] = adopt.Recorded(svcs, want, "")
		} else {
			f.allow = append(f.allow, want)
			owned[want] = true
		}
	}
	for _, s := range svcs {
		var next *adopt.Rule
		if want := adopt.RuleFor(subnet, s.Port()); owned[want] {
			next = &want
		}
		if s.Firewall != nil && !owned[*s.Firewall] && !slices.Contains(f.remove, *s.Firewall) &&
			slices.Contains(present, *s.Firewall) {
			f.remove = append(f.remove, *s.Firewall)
		}
		if !sameRecord(s.Firewall, next) {
			if f.records == nil {
				f.records = map[string]*adopt.Rule{}
			}
			f.records[s.Name] = next
		}
	}
	return f, nil
}

func sameRecord(a, b *adopt.Rule) bool {
	return (a == nil && b == nil) || (a != nil && b != nil && *a == *b)
}

// applyAdoptedFirewall adds before it deletes and saves the records last: an
// interrupted run leaves at worst an unrecorded rule for the current subnet,
// never a recorded one for a subnet Traefik no longer uses.
func applyAdoptedFirewall(ctx context.Context, e *Env, svcs []adopt.Service, f adoptedFirewall) error {
	for _, r := range f.allow {
		if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: append([]string{"allow"}, r.Args()...)}); err != nil {
			return err
		}
	}
	for _, r := range f.remove {
		if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: append([]string{"delete", "allow"}, r.Args()...)}); err != nil {
			return err
		}
	}
	for _, s := range svcs {
		next, ok := f.records[s.Name]
		if !ok {
			continue
		}
		s.Firewall = next
		if err := adopt.Save(e.FS, adopt.Dir, s); err != nil {
			return err
		}
	}
	return nil
}

func (AdoptedStep) Check(ctx context.Context, e *Env) (Plan, error) {
	svcs, err := adopt.Load(e.FS, adopt.Dir)
	if err != nil {
		return Plan{}, err
	}
	fw, err := planAdoptedFirewall(ctx, e, svcs)
	if err != nil {
		return Plan{}, err
	}
	changes := fw.changes()
	want, err := adopt.Render(svcs, effectiveEdgeMode(e.Cfg), e.Cfg.EdgeGateway())
	if err != nil {
		return Plan{}, err
	}
	dst := filepath.Join(dynamicDir, adopt.RouteFile)
	have, err := e.FS.ReadFile(dst)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return Plan{}, err
	}
	if !bytes.Equal(want, have) {
		changes = append(changes, "regenerate "+dst)
	}
	return Plan{Changes: changes}, nil
}

func (AdoptedStep) Apply(ctx context.Context, e *Env, _ Plan) (string, error) {
	svcs, err := adopt.Load(e.FS, adopt.Dir)
	if err != nil {
		return "", err
	}
	fw, err := planAdoptedFirewall(ctx, e, svcs)
	if err != nil {
		return "", err
	}
	if err := applyAdoptedFirewall(ctx, e, svcs, fw); err != nil {
		return "", err
	}
	return "", adopt.Sync(e.FS, adopt.Dir, dynamicDir, effectiveEdgeMode(e.Cfg), e.Cfg.EdgeGateway())
}
