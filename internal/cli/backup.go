package cli

import (
	"errors"
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/output"
)

type backupResultsReport struct {
	Results  []backup.Result `json:"results"`
	Warnings []string        `json:"warnings,omitempty"`
}

func isStatusWrite(err error) bool {
	var oe *output.Error
	return errors.As(err, &oe) && oe.Code == "E_BACKUP_STATUS_WRITE"
}

type backupListReport struct {
	Snapshots []backup.Snapshot `json:"snapshots"`
}

func (e *OpsEnv) backupRunner() *backup.Runner {
	run := e.runner()
	d := e.Deployer
	if d == nil {
		d = &deploy.Deployer{DeployRoot: e.deployRoot(), FS: e.fs()}
	}
	host := ""
	if e.Host != nil {
		host = e.Host.HostName
	}
	return &backup.Runner{
		Run:        run,
		FS:         e.fs(),
		Compose:    func(inst string) *compose.Project { return d.ComposeProject(run, inst) },
		Load:       backup.LoadInstance(e.deployRoot(), e.Catalog),
		Catalog:    e.Catalog,
		Host:       deploy.BackupTargetFrom(e.Host, e.Secrets),
		Store:      e.Store,
		Notify:     notifierFor(e.Host, e.Secrets),
		DeployRoot: e.deployRoot(),
		HostName:   host,
	}
}

func newBackupCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backup",
		Short: "Run and inspect backups of local data services",
		Example: `  cvctl backup now myapp-prod
  cvctl backup list myapp-prod`,
	}
	cmd.AddCommand(newBackupNowCmd(), newBackupRunCmd(), newBackupListCmd(), newBackupVerifyCmd())
	return cmd
}

func backupEnv(cmd *cobra.Command) (*OpsEnv, error) {
	asJSON, _ := cmd.Flags().GetBool("json")
	return newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
}

func printBackupResults(cmd *cobra.Command, results []backup.Result, warnings []string) {
	printerFrom(cmd).Result(backupResultsReport{Results: results, Warnings: warnings}, func(w io.Writer) {
		for _, warn := range warnings {
			fmt.Fprintf(w, "warning: %s\n", warn)
		}
		tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
		fmt.Fprintln(tw, "INSTANCE\tSERVICE\tSTRATEGY\tRESULT")
		for _, r := range results {
			res := "ok"
			if !r.OK {
				res = "failed: " + r.Error
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", r.Instance, r.Service, r.Strategy, res)
		}
		tw.Flush()
	})
}

func newBackupNowCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "now <instance> [service]",
		Short: "Back up an instance now",
		Example: `  cvctl backup now myapp-prod
  cvctl backup now myapp-prod db
  cvctl backup now myapp-prod --json`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := backupEnv(cmd)
			if err != nil {
				return err
			}
			if err := e.checkInstance(args[0]); err != nil {
				return err
			}
			only := ""
			if len(args) == 2 {
				only = args[1]
			}
			results, err := e.backupRunner().BackupNow(cmd.Context(), args[0], only)
			if err != nil {
				if len(results) > 0 {
					return &output.DataError{Err: err, Data: backupResultsReport{Results: results}}
				}
				return err
			}
			printBackupResults(cmd, results, nil)
			return nil
		},
	}
}

func newBackupRunCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Back up every instance (used by the timer)",
		Example: `  cvctl backup run
  cvctl backup run --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			e, err := backupEnv(cmd)
			if err != nil {
				return err
			}
			insts, err := e.Store.Instances()
			if err != nil {
				return err
			}
			r := e.backupRunner()
			var all []backup.Result
			var warnings []string
			failed := 0
			for _, inst := range insts {
				results, err := r.BackupNow(cmd.Context(), inst, "")
				all = append(all, results...)
				if isStatusWrite(err) {
					warnings = append(warnings, err.Error())
				} else if err != nil {
					failed++
					if len(results) == 0 {
						all = append(all, backup.Result{Instance: inst, Error: err.Error()})
					}
				}
			}
			if failed > 0 {
				return &output.DataError{
					Err: &output.Error{
						Code:    "E_BACKUP_FAILED",
						Message: fmt.Sprintf("backup failed for %d instance(s)", failed),
						Hint:    "see the per-service errors with cvctl backup run --json",
					},
					Data: backupResultsReport{Results: all, Warnings: warnings},
				}
			}
			printBackupResults(cmd, all, warnings)
			return nil
		},
	}
}

func newBackupListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list <instance> [service]",
		Short: "List the backups of an instance",
		Example: `  cvctl backup list myapp-prod
  cvctl backup list myapp-prod db --json`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := backupEnv(cmd)
			if err != nil {
				return err
			}
			if err := e.checkInstance(args[0]); err != nil {
				return err
			}
			svc := ""
			if len(args) == 2 {
				svc = args[1]
			}
			snaps, err := e.backupRunner().List(cmd.Context(), args[0], svc)
			if err != nil {
				return err
			}
			printerFrom(cmd).Result(backupListReport{Snapshots: snaps}, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "SERVICE\tSTRATEGY\tID\tTIME")
				for _, s := range snaps {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Service, s.Strategy, s.ID, s.Time)
				}
				tw.Flush()
			})
			return nil
		},
	}
}

func newBackupVerifyCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "verify [instance]",
		Short: "Restore the latest backups into throwaway containers and check them",
		Example: `  cvctl backup verify
  cvctl backup verify myapp-prod --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := backupEnv(cmd)
			if err != nil {
				return err
			}
			var insts []string
			if len(args) == 1 {
				if err := e.checkInstance(args[0]); err != nil {
					return err
				}
				insts = args
			} else if insts, err = e.Store.Instances(); err != nil {
				return err
			}
			r := e.backupRunner()
			var all []backup.Result
			var warnings []string
			failed := 0
			for _, inst := range insts {
				results, err := r.Verify(cmd.Context(), inst, "")
				all = append(all, results...)
				if isStatusWrite(err) {
					warnings = append(warnings, err.Error())
				} else if err != nil {
					failed++
					if len(results) == 0 {
						all = append(all, backup.Result{Instance: inst, Error: err.Error()})
					}
				}
			}
			if failed > 0 {
				return &output.DataError{
					Err: &output.Error{
						Code:    "E_VERIFY_FAILED",
						Message: fmt.Sprintf("restore verification failed for %d instance(s)", failed),
						Hint:    "see the per-service errors with cvctl backup verify --json",
					},
					Data: backupResultsReport{Results: all, Warnings: warnings},
				}
			}
			printBackupResults(cmd, all, warnings)
			return nil
		},
	}
}
