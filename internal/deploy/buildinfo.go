package deploy

import (
	"cmp"
	"context"
	"path/filepath"
	"slices"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
)

// labelTimeout caps all label reads of one notice, so a slow registry delays
// the end of a deploy by at most this long.
var labelTimeout = 5 * time.Second

type labelResult struct {
	svc    string
	labels map[string]string
}

// deployInfo gathers what a deploy notice shows besides the result. It never
// fails: whatever cannot be read is left out.
func (d *Deployer) deployInfo(ctx context.Context, inst string, st *state.State, want state.Desired, level string) *notify.Deploy {
	info := &notify.Deploy{}
	if level != "info" {
		info.Logs = "sudo cvctl history " + inst
	}
	for svc, dig := range want.Digests {
		if st == nil || st.Current == nil || st.Current.Digests[svc] != dig {
			info.Changed = append(info.Changed, svc)
		}
	}
	slices.Sort(info.Changed)

	p, dec := d.loadForNotice(inst)
	if p == nil || dec == nil {
		return info
	}
	info.AppURL = appURL(dec.Domains, info.Changed)
	if d.Labels == nil || len(info.Changed) == 0 {
		return info
	}

	// WithoutCancel: a shutdown or deploy timeout must not suppress the notice.
	lctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), labelTimeout)
	defer cancel()

	images := map[string]string{}
	for _, a := range p.Apps {
		if !slices.Contains(info.Changed, a.Name) {
			continue
		}
		if d.Allowed != nil && registry.Allowed(a.Image, d.Allowed) != nil {
			continue
		}
		images[a.Name] = a.Image
	}
	// Buffered so a reader that outlives the ceiling can still deliver and exit.
	results := make(chan labelResult, len(images))
	for svc, image := range images {
		go func() {
			l, err := d.Labels.Labels(lctx, image, want.Digests[svc])
			if err != nil {
				l = nil
			}
			results <- labelResult{svc, l}
		}()
	}
	var got []labelResult
collect:
	for range images {
		select {
		case r := <-results:
			got = append(got, r)
		case <-lctx.Done():
			break collect
		}
	}
	slices.SortFunc(got, func(a, b labelResult) int {
		return cmp.Compare(a.svc, b.svc)
	})
	for _, r := range got {
		b := notify.Build{
			Service: r.svc,
			Repo:    r.labels["cvctl.repo"],
			Commit:  r.labels["cvctl.commit"],
			PR:      r.labels["cvctl.pr"],
			Title:   r.labels["cvctl.title"],
			Author:  r.labels["cvctl.author"],
		}
		if b == (notify.Build{Service: r.svc}) {
			continue
		}
		info.Builds = append(info.Builds, b)
	}
	return info
}

// loadForNotice reads the contract and decisions the way DeployLocked does,
// without failing the deploy on any error.
func (d *Deployer) loadForNotice(inst string) (*contract.Project, *instance.Decisions) {
	instDir := filepath.Join(d.deployRoot(), inst)
	envPath := filepath.Join(instDir, "cvctl.env")
	d0, err := instance.Read(envPath, &contract.Project{})
	if err != nil {
		return nil, nil
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	cat, err := d.catalog()
	if err != nil {
		return nil, nil
	}
	p, err := contract.Load(filepath.Join(instDir, "infra", infraPath), cat)
	if err != nil {
		return nil, nil
	}
	dec, err := instance.Read(envPath, p)
	if err != nil {
		return nil, nil
	}
	return p, dec
}

// appURL picks the first changed service with a domain, else the first
// service with a domain, both in alphabetical order.
func appURL(domains map[string]string, changed []string) string {
	for _, svc := range changed {
		if dom := domains[svc]; dom != "" {
			return "https://" + dom
		}
	}
	names := make([]string, 0, len(domains))
	for svc, dom := range domains {
		if dom != "" {
			names = append(names, svc)
		}
	}
	slices.Sort(names)
	if len(names) == 0 {
		return ""
	}
	return "https://" + domains[names[0]]
}
