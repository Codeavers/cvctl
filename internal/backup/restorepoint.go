package backup

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

var restorePointID = regexp.MustCompile(`^deploy-[0-9]+$`)

// RestorePoint implements deploy.RestorePointer for wal-g postgres services.
type RestorePoint struct {
	Compose func(inst string) *compose.Project
	Load    Loader
}

// ErrFreshInstance is returned by Create, after creating the points that could
// be created, when a service container does not exist yet: a first deploy has
// no data to protect.
var ErrFreshInstance = errors.New("no restore point: fresh instance")

// serviceState probes docker by compose labels: the compose files and env of a
// fresh instance may not exist yet, and compose would fail on them.
func serviceState(ctx context.Context, proj *compose.Project, svc string) (exists, running bool, err error) {
	return sys.ContainerState(ctx, proj.Run, proj.Instance, svc)
}

// Create names a restore point in every wal-g postgres service of inst and
// forces the current WAL segment out so the point is archived promptly. The
// id is validated because it is interpolated into SQL.
func (rp *RestorePoint) Create(ctx context.Context, inst, id string) error {
	if !restorePointID.MatchString(id) {
		return &output.Error{
			Code:    "E_RESTORE_POINT_ID",
			Message: fmt.Sprintf("invalid restore point id %q", id),
			Hint:    "restore point ids look like deploy-<number>",
		}
	}
	p, d, err := rp.Load(inst)
	if err != nil {
		return err
	}
	fresh := false
	for _, t := range Targets(p, d) {
		if t.Strategy != StrategyWalG {
			continue
		}
		proj := rp.Compose(inst)
		exists, _, err := serviceState(ctx, proj, t.Service)
		if err != nil {
			return err
		}
		if !exists {
			fresh = true
			continue
		}
		for _, sql := range []string{
			fmt.Sprintf("select pg_create_restore_point('%s')", id),
			"select pg_switch_wal()",
		} {
			cmd := proj.Cmd("exec", "-T", "-u", "postgres", t.Service, "psql", "-v", "ON_ERROR_STOP=1", "-c", sql)
			res, runErr := proj.Run.Run(ctx, cmd)
			if runErr != nil || res.ExitCode != 0 {
				return sys.CmdErr("E_RESTORE_POINT_FAILED", cmd, res, runErr, "check that the postgres service is running and accepts local connections")
			}
		}
	}
	if fresh {
		return ErrFreshInstance
	}
	return nil
}
