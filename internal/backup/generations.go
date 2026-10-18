package backup

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
)

var genDir = regexp.MustCompile(`^g([1-9][0-9]*)/?$`)

// BucketGenerations reads the WAL-G generations of svc from the bucket, which
// is the source of truth: cvctl.env (and a secrets bundle made from it) can be
// older than the archive. highestUsed is the highest generation holding any
// base backup or WAL, highestWithBase the highest holding a finished base
// backup; -1 means none.
func (r *Runner) BucketGenerations(ctx context.Context, inst, svc string) (highestUsed, highestWithBase int, err error) {
	if r.Host == nil {
		return -1, -1, notConfigured()
	}
	p, d, err := r.Load(inst)
	if err != nil {
		return -1, -1, err
	}
	if r, err = r.forInstance(ctx, inst, p, d); err != nil {
		return -1, -1, err
	}
	return r.bucketGenerations(ctx, inst, svc, p, d)
}

func (r *Runner) bucketGenerations(ctx context.Context, inst, svc string, p *contract.Project, d *instance.Decisions) (int, int, error) {
	image, err := r.serviceImage(p, d, svc)
	if err != nil {
		return -1, -1, err
	}
	// The generation folders live under the gen-0 prefix. A separate file
	// keeps the service's own backup-<svc>.env untouched.
	envFile := filepath.Join(r.instanceDir(inst), "walg-probe-"+svc+".env")
	if err := r.FS.WriteFile(envFile, instance.FormatEnv(r.Host.Env(inst, svc, 0)), 0o640, deployOwner); err != nil {
		return -1, -1, &output.Error{
			Code:    "E_FILE_WRITE",
			Message: r.redact(fmt.Sprintf("cannot write the wal-g environment of %s: %v", svc, err)),
			Hint:    "check permissions and free space of the instance directory",
		}
	}
	defer func() { _ = r.FS.Remove(envFile) }()

	net, err := r.backupNet(ctx, inst)
	if err != nil {
		return -1, -1, err
	}
	ls := func(path string) ([][]string, error) {
		args := []string{"run", "--rm", "--network", net, "--env-file", envFile, "--entrypoint", "wal-g", image, "st", "ls"}
		if path != "" {
			args = append(args, path)
		}
		res, err := r.exec(ctx, r.docker(args...))
		if err != nil {
			return nil, err
		}
		var rows [][]string
		for _, line := range strings.Split(string(res.Stdout), "\n") {
			if f := strings.Fields(line); len(f) > 0 && (f[0] == "dir" || f[0] == "obj") {
				rows = append(rows, f)
			}
		}
		return rows, nil
	}

	root, err := ls("")
	if err != nil {
		return -1, -1, err
	}
	// The listing's "last modified" column holds spaces, so names are
	// matched by shape instead of by column.
	var gens []int
	for _, f := range root {
		if f[0] != "dir" {
			continue
		}
		for _, field := range f[1:] {
			if m := genDir.FindStringSubmatch(field); m != nil {
				if k, err := strconv.Atoi(m[1]); err == nil {
					gens = append(gens, k)
				}
			}
			if strings.TrimSuffix(field, "/") == "basebackups_005" || strings.TrimSuffix(field, "/") == "wal_005" {
				gens = append(gens, 0)
			}
		}
	}
	if len(gens) == 0 {
		return -1, -1, nil
	}
	slices.Sort(gens)
	gens = slices.Compact(gens)
	used := gens[len(gens)-1]
	for i := len(gens) - 1; i >= 0; i-- {
		path := "basebackups_005"
		if gens[i] > 0 {
			path = "g" + strconv.Itoa(gens[i]) + "/" + path
		}
		rows, err := ls(path)
		if err != nil {
			return -1, -1, err
		}
		for _, f := range rows {
			for _, field := range f[1:] {
				if strings.HasPrefix(field, "base_") && strings.HasSuffix(field, "_backup_stop_sentinel.json") {
					return used, gens[i], nil
				}
			}
		}
	}
	return used, -1, nil
}

func (r *Runner) serviceImage(p *contract.Project, d *instance.Decisions, svc string) (string, error) {
	cat, err := r.catalog()
	if err != nil {
		return "", err
	}
	for _, ds := range p.Data {
		if ds.Name != svc {
			continue
		}
		version := d.Services[svc].Version
		if version == "" {
			version = ds.Version
		}
		return cat[ds.Kind].ImageFor(version)
	}
	return "", &output.Error{
		Code:    "E_BACKUP_NO_TARGET",
		Message: fmt.Sprintf("service %q of %s is not a data service", svc, d.Instance),
		Hint:    "run cvctl backup list <instance> to see which services are backed up",
	}
}
