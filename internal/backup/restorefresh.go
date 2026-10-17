package backup

import (
	"context"
	"fmt"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
)

// RestoreFresh recovers the latest backup of svc into the not-yet-existing
// volume, reading wal-g from the generation recorded in the decisions. It is
// for rebuilding a lost host: no container of the instance runs yet, so
// nothing is stopped, switched or rolled back. The caller holds the instance
// lock.
func (r *Runner) RestoreFresh(ctx context.Context, inst, svc string, p *contract.Project, d *instance.Decisions, volume string) error {
	if r.Host == nil {
		return notConfigured()
	}
	var target *Target
	for _, t := range Targets(p, d) {
		if t.Service == svc {
			t := t
			target = &t
		}
	}
	if target == nil {
		return &output.Error{
			Code:    "E_BACKUP_NO_TARGET",
			Message: fmt.Sprintf("service %q of %s has no backup (not a local data service with backups)", svc, inst),
			Hint:    "run cvctl backup list <instance> to see which services are backed up",
		}
	}
	exists, err := r.volumeExists(ctx, volume)
	if err != nil {
		return err
	}
	if exists {
		return &output.Error{
			Code:    "E_RESTORE_VOLUME_EXISTS",
			Message: fmt.Sprintf("volume %s already exists", volume),
			Hint:    "restore only runs on a host without the data; remove the volume by hand if it is a leftover",
		}
	}
	if _, err := r.exec(ctx, r.docker("volume", "create", volume)); err != nil {
		return err
	}
	tgt := restoreTarget{raw: "latest", kind: "latest"}
	if target.Strategy == StrategyWalG {
		// backup-<svc>.env is only written by a deploy, which has not run yet
		env := r.Host.Env(inst, svc, d.Services[svc].BackupGen)
		if err := r.FS.WriteFile(render.BackupEnvPath(r.instanceDir(inst), svc), instance.FormatEnv(env), 0o640, deployOwner); err != nil {
			return &output.Error{
				Code:    "E_FILE_WRITE",
				Message: r.redact(fmt.Sprintf("cannot write the wal-g environment of %s: %v", svc, err)),
				Hint:    "check permissions and free space of the instance directory",
			}
		}
		return r.walgRecover(ctx, inst, svc, p, d, "LATEST", tgt, volume)
	}
	return r.resticRecover(ctx, inst, svc, tgt, volume)
}

// BaseBackup pushes a base backup of svc so the generation it now archives
// into is restorable on its own.
func (r *Runner) BaseBackup(ctx context.Context, inst, svc string) error {
	_, err := r.exec(ctx, r.Compose(inst).Cmd("exec", "-T", "-u", "postgres", svc, "wal-g", "backup-push", pgData))
	return err
}
