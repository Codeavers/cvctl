package cli

import (
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
)

type deployReport struct {
	Instance string       `json:"instance"`
	Status   state.Status `json:"status"`
	ID       string       `json:"id,omitempty"`
	Detail   string       `json:"detail,omitempty"`
	Warnings []string     `json:"warnings,omitempty"`
}

type rollbackReport struct {
	Instance string       `json:"instance"`
	Status   state.Status `json:"status"`
	ID       string       `json:"id,omitempty"`
	Detail   string       `json:"detail,omitempty"`
}

type pauseReport struct {
	Instance string `json:"instance"`
	Status   string `json:"status"`
	Reason   string `json:"reason"`
}

type resumeReport struct {
	Instance string `json:"instance"`
	Status   string `json:"status"`
}

func newDeployCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "deploy <instance>",
		Short: "Deploy an instance",
		Example: `  cvctl deploy myapp-dev
  cvctl deploy myapp-prod --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newOpsEnv(opsOpts{
				NonInteractive: asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			if err := e.checkInstance(inst); err != nil {
				return err
			}

			instDir := filepath.Join(e.deployRoot(), inst)
			cvctlEnvPath := filepath.Join(instDir, "cvctl.env")

			d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
			if err != nil {
				return err
			}
			infraPath := d0.InfraPath
			if infraPath == "" {
				infraPath = "."
			}
			infraDir := filepath.Join(instDir, "infra", infraPath)

			cat, err := e.catalog()
			if err != nil {
				return err
			}
			// A contract on disk that fails rules added after it was deployed
			// must not block deploying a fixed one: d0 is enough for Desired,
			// and Desired and the pipeline validate the wanted contract.
			var warnings []string
			d := d0
			if p, lErr := contract.Load(infraDir, cat); lErr != nil {
				warnings = append(warnings, "contract on disk rejected: "+instance.WrapLoadErr(lErr).Error())
			} else if dp, rErr := instance.Read(cvctlEnvPath, p); rErr != nil {
				warnings = append(warnings, "contract on disk rejected: "+rErr.Error())
			} else {
				d = dp
			}

			deps := deploy.SourceDepsWithRoots(inst, e.runner(), e.Host, e.Secrets, e.deployRoot(), e.stateRoot(), e.fs())
			if e.Resolver != nil {
				deps.Resolver = e.Resolver
			}
			if e.RepoFactory != nil {
				keyPath := filepath.Join(e.stateRoot(), inst, "deploy_key")
				if _, err := e.fs().Stat(keyPath); err != nil {
					keyPath = ""
				}
				deps.Repo = e.repo(infraDir, keyPath)
			}

			src := deploy.SourceFor(d, deps)
			desired, err := src.Desired(cmd.Context(), d)
			if err != nil {
				return err
			}

			if e.Deployer.Repo == nil {
				e.Deployer.Repo = func(string) deploy.CheckoutAPI {
					if c, ok := deps.Repo.(deploy.CheckoutAPI); ok {
						return c
					}
					return nil
				}
			}
			if e.Deployer.Contracts == nil {
				e.Deployer.Contracts = deploy.OCIFetcher(deps.OCI)
			}
			if e.Deployer.Catalog == nil {
				e.Deployer.Catalog = cat
			}
			if e.Deployer.DeployRoot == "" {
				e.Deployer.DeployRoot = e.deployRoot()
			}
			if e.Deployer.Store == nil {
				e.Deployer.Store = e.Store
			}

			res, err := e.Deployer.Deploy(cmd.Context(), inst, desired, deploy.Options{
				Force: true,
				Actor: "cli",
			})
			if err != nil {
				return err
			}

			rep := deployReport{
				Instance: inst,
				Status:   res.Status,
				ID:       res.ID,
				Detail:   res.Detail,
				Warnings: warnings,
			}

			if res.Status == state.StatusFailed {
				failedErr := &output.Error{
					Code:    "E_DEPLOY_FAILED",
					Message: fmt.Sprintf("deploy failed: %s", res.Detail),
					Hint:    "check container logs with cvctl logs " + inst,
				}
				if asJSON {
					return &output.DataError{Err: failedErr, Data: rep}
				}
				for _, wn := range warnings {
					fmt.Fprintf(cmd.OutOrStdout(), "warning: %s\n", wn)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", inst, res.Status)
				return failedErr
			}

			printerFrom(cmd).Result(rep, func(w io.Writer) {
				for _, wn := range warnings {
					fmt.Fprintf(w, "warning: %s\n", wn)
				}
				fmt.Fprintf(w, "%s: %s\n", inst, res.Status)
			})
			return nil
		},
	}
	return cmd
}

func newRollbackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rollback <instance>",
		Short: "Rollback an instance to a previous state",
		Example: `  cvctl rollback myapp-dev
  cvctl rollback myapp-dev --to 1
  cvctl rollback myapp-dev --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			toIndex, _ := cmd.Flags().GetInt("to")
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newOpsEnv(opsOpts{
				NonInteractive: asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			if err := e.checkInstance(inst); err != nil {
				return err
			}

			var target *state.Desired
			if toIndex >= 0 {
				events, err := e.Store.History(inst, 0)
				if err != nil {
					return err
				}
				// Only completed deploys/rollbacks are valid targets: other events
				// carry the desired state of a failed or merely attempted change.
				var targets []state.Event
				for _, ev := range events {
					if (ev.Kind == "deploy" || ev.Kind == "rollback") && ev.Result == string(state.StatusOK) && ev.To != nil {
						targets = append(targets, ev)
					}
				}
				if toIndex >= len(targets) {
					return &output.Error{
						Code:    "E_ROLLBACK_TARGET_INVALID",
						Message: fmt.Sprintf("--to %d is out of range (%d successful deploys in history)", toIndex, len(targets)),
						Hint:    "list candidates with cvctl history " + inst + "; index 0 is the newest successful deploy",
					}
				}
				target = targets[toIndex].To
			}

			if e.Deployer.DeployRoot == "" {
				e.Deployer.DeployRoot = e.deployRoot()
			}
			if e.Deployer.Store == nil {
				e.Deployer.Store = e.Store
			}
			if e.Deployer.Catalog == nil {
				cat, _ := e.catalog()
				e.Deployer.Catalog = cat
			}

			res, err := e.Deployer.Rollback(cmd.Context(), inst, target, "cli")
			if err != nil {
				return err
			}

			rep := rollbackReport{
				Instance: inst,
				Status:   res.Status,
				ID:       res.ID,
				Detail:   res.Detail,
			}

			if res.Status == state.StatusFailed {
				failedErr := &output.Error{
					Code:    "E_ROLLBACK_FAILED",
					Message: fmt.Sprintf("rollback failed: %s", res.Detail),
					Hint:    "check container logs with cvctl logs " + inst,
				}
				if asJSON {
					return &output.DataError{Err: failedErr, Data: rep}
				}
				fmt.Fprintf(cmd.OutOrStdout(), "%s: %s\n", inst, res.Status)
				return failedErr
			}

			printerFrom(cmd).Result(rep, func(w io.Writer) {
				fmt.Fprintf(w, "%s: %s\n", inst, res.Status)
			})
			return nil
		},
	}
	cmd.Flags().Int("to", -1, "history index to roll back to")
	return cmd
}

func newPauseCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pause <instance>",
		Short: "Pause deployments for an instance",
		Example: `  cvctl pause myapp-dev
  cvctl pause myapp-dev --reason "maintenance"
  cvctl pause myapp-dev --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			reason, _ := cmd.Flags().GetString("reason")
			if reason == "" {
				reason = "paused by operator"
			}
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newOpsEnv(opsOpts{
				NonInteractive: asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			if err := e.checkInstance(inst); err != nil {
				return err
			}

			unlock, err := e.Store.Lock(inst)
			if err != nil {
				return err
			}
			defer unlock()

			st, err := e.Store.Load(inst)
			if err != nil {
				return err
			}

			if st.Status == state.StatusDeploying {
				return &output.Error{
					Code:    "E_DEPLOY_IN_PROGRESS",
					Message: fmt.Sprintf("cannot pause instance %q: deploy in progress", inst),
					Hint:    fmt.Sprintf("wait for the deploy to finish or check cvctl status %s", inst),
				}
			}

			now := time.Now()
			st.Status = state.StatusPaused
			st.PausedReason = reason
			st.PausedWantHash = ""
			st.UpdatedAt = now

			if err := e.Store.Save(st); err != nil {
				return err
			}

			if err := e.Store.Append(inst, state.Event{
				ID:     fmt.Sprintf("pause-%d", now.UnixNano()),
				At:     now,
				Kind:   "pause",
				From:   st.Current,
				To:     st.Current,
				Result: string(state.StatusPaused),
				Detail: reason,
				Actor:  "cli",
			}); err != nil {
				return err
			}

			rep := pauseReport{
				Instance: inst,
				Status:   string(state.StatusPaused),
				Reason:   reason,
			}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				fmt.Fprintf(w, "%s: paused (%s)\n", inst, reason)
			})
			return nil
		},
	}
	cmd.Flags().String("reason", "paused by operator", "reason for pausing")
	return cmd
}

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <instance>",
		Short: "Resume deployments for an instance",
		Example: `  cvctl resume myapp-dev
  cvctl resume myapp-dev --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newOpsEnv(opsOpts{
				NonInteractive: asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			if err := e.checkInstance(inst); err != nil {
				return err
			}

			unlock, err := e.Store.Lock(inst)
			if err != nil {
				return err
			}
			defer unlock()

			st, err := e.Store.Load(inst)
			if err != nil {
				return err
			}

			if st.Status != state.StatusPaused {
				return &output.Error{
					Code:    "E_NOT_PAUSED",
					Message: fmt.Sprintf("cannot resume instance %q: status is %s (not PAUSED)", inst, st.Status),
					Hint:    fmt.Sprintf("cvctl status %s", inst),
				}
			}

			now := time.Now()
			st.Status = state.StatusOK
			st.PausedReason = ""
			st.PausedWantHash = ""
			st.UpdatedAt = now

			if err := e.Store.Save(st); err != nil {
				return err
			}

			if err := e.Store.Append(inst, state.Event{
				ID:     fmt.Sprintf("resume-%d", now.UnixNano()),
				At:     now,
				Kind:   "resume",
				From:   st.Current,
				To:     st.Current,
				Result: string(state.StatusOK),
				Detail: "resumed by operator",
				Actor:  "cli",
			}); err != nil {
				return err
			}

			rep := resumeReport{
				Instance: inst,
				Status:   string(state.StatusOK),
			}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				fmt.Fprintf(w, "%s: resumed\n", inst)
			})
			return nil
		},
	}
	return cmd
}
