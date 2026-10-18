package deploy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
)

// swapOrRestore runs executeSwap and, when it fails, checks out current again
// so infra/ never stays at a revision that was not installed, as DeployLocked
// does before promotion.
func (d *Deployer) swapOrRestore(ctx context.Context, inst string, target, current *state.Desired, appEnv map[string]string, appEnvAbsent bool) error {
	err := d.executeSwap(ctx, inst, target, appEnv, appEnvAbsent)
	if err == nil || !hasInfra(current) {
		return err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	if rErr := d.checkout(rctx, inst, *current); rErr != nil {
		return fmt.Errorf("%w; restore checkout failed: %v", err, rErr)
	}
	return err
}

// executeSwap performs checkout, render, promotion, Up, and health check for target state.
func (d *Deployer) executeSwap(ctx context.Context, inst string, target *state.Desired, appEnv map[string]string, appEnvAbsent bool) error {
	if err := d.checkout(ctx, inst, *target); err != nil {
		return err
	}

	instDir := filepath.Join(d.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")

	d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
	if err != nil {
		return err
	}
	if err := instance.CheckDir(d0, cvctlEnvPath); err != nil {
		return err
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	infraDir := filepath.Join(instDir, "infra", infraPath)

	cat, err := d.catalog()
	if err != nil {
		return err
	}

	p, err := contract.Load(infraDir, cat)
	if err != nil {
		return instance.WrapLoadErr(err)
	}

	dec, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return err
	}
	if err := instance.CheckDir(dec, cvctlEnvPath); err != nil {
		return err
	}
	if dec.InfraPath == "" {
		dec.InfraPath = "."
	}
	if dec.Digests == nil {
		dec.Digests = make(map[string]string)
	}
	for k, v := range target.Digests {
		dec.Digests[k] = v
	}
	if err := instance.Verify(infraDir, p, dec, cat, true); err != nil {
		return err
	}

	finalOverridePath := filepath.Join(instDir, "compose.cvctl.yml")
	finalAppEnvPath := filepath.Join(instDir, "app.env")
	finalRoutePath := filepath.Join(d.traefikDir(), inst+".yml")
	backupTarget := render.BackupTargetFor(d.Backup, dec)
	renderOpts := render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  finalAppEnvPath,
		Backup:      backupTarget,
	}

	overrideBytes, err := d.renderOverride(p, dec, cat, renderOpts)
	if err != nil {
		return err
	}
	routeBytes, err := d.renderTraefik(p, dec, renderOpts)
	if err != nil {
		return err
	}

	fsys := d.fs()
	// Promote order: app.env -> backup envs -> route -> override
	if appEnvAbsent {
		_ = fsys.Remove(finalAppEnvPath)
	} else if len(appEnv) > 0 {
		if err := fsys.WriteFile(finalAppEnvPath, instance.FormatEnv(appEnv), 0o640, deployOwner); err != nil {
			return err
		}
	}
	for svc, env := range render.BackupEnvs(p, dec, cat, renderOpts) {
		if err := fsys.WriteFile(render.BackupEnvPath(instDir, svc), instance.FormatEnv(env), 0o640, deployOwner); err != nil {
			return err
		}
	}
	if routeBytes != nil {
		if err := fsys.WriteFile(finalRoutePath, routeBytes, 0o640, deployOwner); err != nil {
			return err
		}
	} else {
		_ = fsys.Remove(finalRoutePath)
	}
	if err := fsys.WriteFile(finalOverridePath, overrideBytes, 0o640, deployOwner); err != nil {
		return err
	}

	finalCompose := d.Compose(inst, finalOverridePath, finalAppEnvPath)
	if finalCompose == nil {
		return &output.Error{
			Code:    "E_DEPLOY_MISCONFIGURED",
			Message: "deployer misconfigured: Compose returned nil",
			Hint:    "ensure Compose factory returns a valid ComposeAPI",
		}
	}
	if err := d.ensureNetworks(ctx, inst, p, dec); err != nil {
		return err
	}
	if err := d.provisionBackupUser(ctx, inst, p, dec, backupTarget); err != nil {
		return err
	}
	if err := finalCompose.Up(ctx); err != nil {
		return err
	}
	if err := d.connectTraefik(ctx, inst); err != nil {
		return err
	}
	// The restored app.env may need buckets or policy grants that a later deploy
	// or an older volume snapshot removed; the script is idempotent.
	if err := d.provisionBuckets(ctx, finalCompose, inst, p, dec, cat, withSecrets(appEnv, d.cvctlSecrets(cvctlEnvPath))); err != nil {
		return err
	}

	if err := d.Health(ctx, inst, finalOverridePath); err != nil {
		return err
	}

	return nil
}

// Rollback executes manual or targeted rollback to a desired state.
func (d *Deployer) Rollback(ctx context.Context, inst string, to *state.Desired, actor string) (Result, error) {
	if actor == "" {
		actor = "agent"
	}
	now := d.clock()
	deployID := fmt.Sprintf("deploy-%d", now.UnixNano())

	if d.Compose == nil || d.Health == nil {
		return Result{}, &output.Error{
			Code:    "E_DEPLOY_MISCONFIGURED",
			Message: "deployer misconfigured: Compose and Health are required",
			Hint:    "configure Compose and Health before running rollback",
		}
	}

	unlock, err := d.Store.Lock(inst)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	st, err := d.Store.Load(inst)
	if err != nil {
		return Result{}, err
	}

	target := to
	if target == nil {
		target = st.Previous
	}
	if target == nil {
		return Result{}, &output.Error{
			Code:    "E_ROLLBACK_NO_TARGET",
			Message: fmt.Sprintf("instance %q has no previous state to roll back to", inst),
			Hint:    "specify --to or deploy the instance first",
		}
	}

	instDir := filepath.Join(d.deployRoot(), inst)
	finalAppEnvPath := filepath.Join(instDir, "app.env")
	existingEnv, _, err := d.readAppEnv(finalAppEnvPath)
	if err != nil {
		return Result{}, err
	}

	swapErr := d.swapOrRestore(ctx, inst, target, st.Current, existingEnv, false)
	if swapErr != nil {
		st.Status = state.StatusFailed
		st.RollbackFailed = true
		st.UpdatedAt = now

		redactedSwapErr := redact(swapErr.Error(), withSecrets(existingEnv, d.cvctlSecrets(filepath.Join(instDir, "cvctl.env"))))
		res := Result{
			Status: state.StatusFailed,
			ID:     deployID,
			Detail: redactedSwapErr,
		}
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "error",
			Title: fmt.Sprintf("%s ROLLBACK FAILED", inst),
			Text:  fmt.Sprintf("manual rollback failed: %s", redactedSwapErr),
		})

		var errs []error
		if err := d.Store.Save(st); err != nil {
			errs = append(errs, err)
		}
		if err := d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "rollback",
			From:   st.Current,
			To:     target,
			Result: "rollback_failed",
			Detail: redactedSwapErr,
			Actor:  actor,
		}); err != nil {
			errs = append(errs, err)
		}

		return res, errors.Join(errs...)
	}

	pausedReason := fmt.Sprintf("manual rollback; resume with cvctl resume %s", inst)
	pausedWantHash := st.FailedHash
	if pausedWantHash == "" && st.Current != nil {
		pausedWantHash = st.Current.Hash()
	}

	st.Previous = st.Current
	st.Current = target
	st.Status = state.StatusPaused
	st.PausedReason = pausedReason
	st.PausedWantHash = pausedWantHash
	st.RollbackFailed = false
	st.UpdatedAt = now
	if err := d.Store.Save(st); err != nil {
		return Result{}, err
	}
	d.pruneContracts(inst, st)

	if err := d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "rollback",
		From:   st.Previous,
		To:     st.Current,
		Result: string(state.StatusPaused),
		Detail: pausedReason,
		Actor:  actor,
	}); err != nil {
		return Result{}, err
	}
	_ = d.notifier().Send(ctx, notify.Message{
		Host:  d.hostName(),
		Level: "info",
		Title: fmt.Sprintf("%s deploy PAUSED", inst),
		Text:  pausedReason,
	})

	return Result{
		Status: state.StatusPaused,
		ID:     deployID,
		Detail: pausedReason,
	}, nil
}
