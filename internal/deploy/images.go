package deploy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/codeavers/cvctl/internal/images"
)

const imagePruneTimeout = 2 * time.Minute

// pruneImages runs after the deploy is recorded, so it never changes the
// deploy's result: disk space is not worth failing a good deploy over.
func (d *Deployer) pruneImages(ctx context.Context, inst string) {
	if d.Images == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), imagePruneTimeout)
	defer cancel()
	if err := d.removeSurplusImages(ctx, inst); err != nil {
		fmt.Fprintf(d.log(), "images: %s: cleanup skipped or incomplete: %v\n", inst, err)
	}
}

func (d *Deployer) removeSurplusImages(ctx context.Context, inst string) error {
	all, err := d.readImageInstances()
	if err != nil {
		return err
	}
	plan := images.Decide(all, images.KeepPrevious)
	i := slices.IndexFunc(all, func(in images.Instance) bool { return in.Name == inst })
	if i < 0 {
		return fmt.Errorf("instance %s not found", inst)
	}
	// Every instance feeds Keep, but only inst's own digests may go.
	plan.Candidates = images.Decide(all[i:i+1], images.KeepPrevious).Candidates
	items, err := d.Images.Prune(ctx, plan, false)
	d.logPruned(inst, items)
	if err != nil {
		return err
	}
	// Prune records a failed removal per image, which hides an expired context.
	return ctx.Err()
}

// logPruned leaves a trace of every irreversible removal. Images in use stay
// quiet: stopped containers make them a normal result of most deploys.
func (d *Deployer) logPruned(inst string, items []images.Item) {
	for _, it := range items {
		switch {
		case it.Removed:
			fmt.Fprintf(d.log(), "images: %s: removed %s\n", inst, it.Ref)
		case !it.InUse:
			fmt.Fprintf(d.log(), "images: %s: not removed: %s: %s\n", inst, it.Ref, firstLine(it.Reason))
		}
	}
}

// readImageInstances reads without the other instances' locks: taking them
// could stall this deploy behind theirs. Any unreadable instance aborts, since
// what it keeps is unknown.
func (d *Deployer) readImageInstances() ([]images.Instance, error) {
	names, err := d.Store.Instances()
	if err != nil {
		return nil, err
	}
	// The store lists only instances with a state file. Directories under the
	// deploy root that are not instances have no history and add nothing.
	entries, err := d.fs().ReadDir(d.deployRoot())
	if err != nil {
		return nil, err
	}
	for _, ent := range entries {
		if ent.IsDir() {
			names = append(names, ent.Name())
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)

	all := make([]images.Instance, 0, len(names))
	for _, name := range names {
		in, err := d.readImageInstance(name)
		if err != nil {
			return nil, fmt.Errorf("reading instance %s: %w", name, err)
		}
		all = append(all, in)
	}
	return all, nil
}

func (d *Deployer) readImageInstance(name string) (images.Instance, error) {
	in := images.Instance{Name: name}
	// Store.Load answers a missing file with an empty healthy state, but
	// Decide needs nil to keep the whole history of such an instance.
	_, err := d.fs().Stat(filepath.Join(d.Store.Root(), name, "state.json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return in, err
	}
	if err == nil {
		if in.State, err = d.Store.Load(name); err != nil {
			return in, err
		}
	}
	// 0 is the whole history, which Decide needs newest first as returned.
	in.Events, err = d.Store.History(name, 0)
	return in, err
}

func (d *Deployer) log() io.Writer {
	if d.Log != nil {
		return d.Log
	}
	return os.Stderr
}
