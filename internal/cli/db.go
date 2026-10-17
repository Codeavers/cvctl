package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/output"
)

type dbCleanupReport struct {
	Volumes []backup.CleanupVolume `json:"volumes"`
	Removed bool                   `json:"removed"`
}

func newDBCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "db",
		Short: "Restore data services from backups and clean up old volumes",
		Example: `  cvctl db restore myapp-prod db --to latest
  cvctl db cleanup myapp-prod`,
	}
	cmd.AddCommand(newDBRestoreCmd(), newDBCleanupCmd())
	return cmd
}

func newDBRestoreCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "restore <instance> <service>",
		Short: "Restore a data service into a new volume and switch to it",
		Long: `Restore a data service into a NEW volume and switch the instance to it.

The instance is locked and its services are stopped while the data is recovered.
If anything fails after that, the previous volume is put back and the instance
is restarted; the new volume is kept for inspection. Volumes are never deleted
here: use cvctl db cleanup.

--to takes latest, an RFC3339 timestamp in the past, or a restore point
deploy-<id> for WAL-G postgres services; latest or a snapshot id for volume
backups.`,
		Example: `  cvctl db restore myapp-prod db --to latest
  cvctl db restore myapp-prod db --to 2026-10-17T10:30:00Z
  cvctl db restore myapp-prod db --to deploy-1760697000000000000 --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if to == "" {
				return &output.UsageError{Err: &output.Error{
					Code:    "E_USAGE_TO",
					Message: "--to is required",
					Hint:    "pass --to latest, an RFC3339 timestamp, a restore point deploy-<id> or a snapshot id",
				}}
			}
			e, err := backupEnv(cmd)
			if err != nil {
				return err
			}
			if err := e.checkInstance(args[0]); err != nil {
				return err
			}
			res, err := e.backupRunner().Restore(cmd.Context(), args[0], args[1], to)
			if res != nil && err == nil {
				printerFrom(cmd).Result(res, func(w io.Writer) {
					fmt.Fprintf(w, "restored %s of %s to %s\nnew volume: %s\nprevious volume (kept): %s\n",
						res.Service, res.Instance, res.Target, res.Volume, res.PreviousVolume)
				})
				return nil
			}
			if res != nil {
				return &output.DataError{Err: err, Data: res}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "restore target: latest, RFC3339 timestamp, deploy-<id> or snapshot id")
	return cmd
}

func newDBCleanupCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "cleanup <instance>",
		Short: "List or remove unused data volumes of an instance",
		Long: `List the data volumes of an instance that no cvctl.env decision references and
no container uses (left behind by db restore). Without --yes nothing is removed.`,
		Example: `  cvctl db cleanup myapp-prod
  cvctl db cleanup myapp-prod --yes --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			e, err := backupEnv(cmd)
			if err != nil {
				return err
			}
			if err := e.checkInstance(args[0]); err != nil {
				return err
			}
			vols, err := e.backupRunner().Cleanup(cmd.Context(), args[0], yes)
			if err != nil {
				if len(vols) > 0 {
					return &output.DataError{Err: err, Data: dbCleanupReport{Volumes: vols, Removed: yes}}
				}
				return err
			}
			printerFrom(cmd).Result(dbCleanupReport{Volumes: vols, Removed: yes}, func(w io.Writer) {
				if len(vols) == 0 {
					fmt.Fprintln(w, "no unused volumes")
					return
				}
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "VOLUME\tREMOVED")
				for _, v := range vols {
					fmt.Fprintf(tw, "%s\t%t\n", v.Name, v.Removed)
				}
				tw.Flush()
				if !yes {
					fmt.Fprintln(w, "run again with --yes to remove them")
				}
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "remove the listed volumes")
	return cmd
}
