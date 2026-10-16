package deploy

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
)

// executeSwap performs checkout, render, promotion, Up, and health check for target state.
func (d *Deployer) executeSwap(ctx context.Context, inst string, target *state.Desired, appEnv map[string]string) error {
	if d.Repo != nil {
		if err := d.Repo.Checkout(ctx, target.InfraCommit); err != nil {
			return err
		}
	}

	instDir := filepath.Join(d.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	d.ensureDiskFile(cvctlEnvPath)

	d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
	if err != nil {
		return err
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	infraDir := filepath.Join(instDir, "infra", infraPath)
	d.ensureDiskFile(filepath.Join(infraDir, "compose.yml"))

	cat, err := d.catalog()
	if err != nil {
		return err
	}

	p, err := contract.Load(infraDir, cat)
	if err != nil {
		return err
	}

	dec, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
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

	finalOverridePath := filepath.Join(instDir, "compose.cvctl.yml")
	finalRoutePath := filepath.Join(d.traefikDir(), inst+".yml")
	renderOpts := render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  filepath.Join(instDir, "app.env"),
	}

	overrideBytes, err := render.Override(p, dec, cat, renderOpts)
	if err != nil {
		return err
	}
	routeBytes, err := render.Traefik(p, dec, renderOpts)
	if err != nil {
		return err
	}

	fsys := d.fs()
	if err := fsys.WriteFile(finalOverridePath, overrideBytes, 0o640, deployOwner); err != nil {
		return err
	}
	if len(appEnv) > 0 {
		finalAppEnvPath := filepath.Join(instDir, "app.env")
		if err := fsys.WriteFile(finalAppEnvPath, instance.FormatEnv(appEnv), 0o640, deployOwner); err != nil {
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

	finalCompose := d.getCompose(finalOverridePath)
	if finalCompose != nil {
		if err := finalCompose.Up(ctx); err != nil {
			return err
		}
	}

	if err := d.checkHealth(ctx, inst, finalOverridePath); err != nil {
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
	deployID := fmt.Sprintf("deploy-%d", now.Unix())

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
	existingEnv, err := d.readAppEnv(finalAppEnvPath)
	if err != nil {
		existingEnv = map[string]string{}
	}

	swapErr := d.executeSwap(ctx, inst, target, existingEnv)
	if swapErr != nil {
		st.Status = state.StatusFailed
		st.RollbackFailed = true
		st.UpdatedAt = now
		_ = d.Store.Save(st)

		_ = d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "rollback",
			From:   st.Current,
			To:     target,
			Result: string(state.StatusFailed),
			Detail: swapErr.Error(),
			Actor:  actor,
		})
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "error",
			Title: fmt.Sprintf("%s deploy FAILED", inst),
			Text:  fmt.Sprintf("manual rollback failed: %v", swapErr),
		})

		return Result{
			Status: state.StatusFailed,
			ID:     deployID,
			Detail: swapErr.Error(),
		}, nil
	}

	pausedReason := fmt.Sprintf("manual rollback; resume with cvctl resume %s", inst)
	st.Previous = st.Current
	st.Current = target
	st.Status = state.StatusPaused
	st.PausedReason = pausedReason
	st.RollbackFailed = false
	st.FailedHash = ""
	st.UpdatedAt = now
	_ = d.Store.Save(st)

	_ = d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "rollback",
		From:   st.Previous,
		To:     st.Current,
		Result: string(state.StatusPaused),
		Detail: pausedReason,
		Actor:  actor,
	})
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
