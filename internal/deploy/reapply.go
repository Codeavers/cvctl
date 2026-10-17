package deploy

import (
	"context"
	"path/filepath"

	"github.com/codeavers/cvctl/internal/state"
)

// Reapply re-renders and brings up target with the current decisions, then
// runs the health check. The caller must hold the instance lock; nothing in
// the state is changed.
func (d *Deployer) Reapply(ctx context.Context, inst string, target *state.Desired) error {
	appEnvPath := filepath.Join(d.deployRoot(), inst, "app.env")
	existing, _, err := d.readAppEnv(appEnvPath)
	if err != nil {
		return err
	}
	return d.executeSwap(ctx, inst, target, existing, false)
}
