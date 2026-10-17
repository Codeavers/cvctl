package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/state"
)

type instanceStatusItem struct {
	Instance string `json:"instance"`
	Env      string `json:"env"`
	Status   string `json:"status"`
	Commit   string `json:"commit"`
	Age      string `json:"age"`
}

type statusReport struct {
	Instances []instanceStatusItem `json:"instances"`
	Warnings  []string             `json:"warnings"`
}

type historyReport struct {
	History []state.Event `json:"history"`
}

type logsReport struct {
	Logs string `json:"logs"`
}

func humanizeAge(now, t time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	days := int(d.Hours()) / 24
	if days >= 1 {
		return fmt.Sprintf("%dd", days)
	}
	hours := int(d.Hours())
	if hours >= 1 {
		return fmt.Sprintf("%dh", hours)
	}
	mins := int(d.Minutes())
	return fmt.Sprintf("%dm", mins)
}

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "status [instance]",
		Short: "Show deployment status of instances",
		Example: `  cvctl status
  cvctl status myapp-dev
  cvctl status --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newOpsEnv(opsOpts{
				NonInteractive: asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			var instances []string
			if len(args) == 1 {
				inst := args[0]
				if err := e.checkInstance(inst); err != nil {
					return err
				}
				instances = []string{inst}
			} else {
				seen := make(map[string]bool)
				if storeInsts, err := e.Store.Instances(); err == nil {
					for _, name := range storeInsts {
						if e.checkInstance(name) == nil {
							seen[name] = true
						}
					}
				}
				if entries, err := e.fs().ReadDir(e.deployRoot()); err == nil {
					for _, ent := range entries {
						if ent.IsDir() && e.checkInstance(ent.Name()) == nil {
							seen[ent.Name()] = true
						}
					}
				}
				for name := range seen {
					instances = append(instances, name)
				}
				slices.Sort(instances)
			}

			now := time.Now()
			items := make([]instanceStatusItem, 0)
			for _, inst := range instances {
				st, _ := e.Store.Load(inst)
				statusStr := string(state.StatusOK)
				if st != nil && st.Status != "" {
					statusStr = string(st.Status)
				}

				commitStr := "-"
				if st != nil && st.Current != nil && st.Current.InfraCommit != "" {
					c := st.Current.InfraCommit
					if len(c) > 7 {
						commitStr = c[:7]
					} else {
						commitStr = c
					}
				}

				ageStr := "-"
				if st != nil {
					ageStr = humanizeAge(now, st.UpdatedAt)
				}

				envName := ""
				if strings.HasSuffix(inst, "-dev") {
					envName = "dev"
				} else if strings.HasSuffix(inst, "-prod") {
					envName = "prod"
				}

				items = append(items, instanceStatusItem{
					Instance: inst,
					Env:      envName,
					Status:   statusStr,
					Commit:   commitStr,
					Age:      ageStr,
				})
			}

			warnings := make([]string, 0)
			if (e.Host != nil && e.Host.Storage == "local") || os.Getenv("CVCTL_STORAGE") == "local" {
				warnings = append(warnings, "CVCTL_STORAGE=local: backups are stored on local disk; configure S3 for offsite backup")
			}

			rep := statusReport{
				Instances: items,
				Warnings:  warnings,
			}

			printerFrom(cmd).Result(rep, func(w io.Writer) {
				for _, warn := range warnings {
					fmt.Fprintf(w, "warning: %s\n", warn)
				}
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "INSTANCE\tENV\tSTATUS\tCOMMIT\tAGE")
				for _, it := range items {
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", it.Instance, it.Env, it.Status, it.Commit, it.Age)
				}
				tw.Flush()
			})
			return nil
		},
	}
	return cmd
}

func newHistoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "history <instance>",
		Short: "Show deployment history for an instance",
		Example: `  cvctl history myapp-dev
  cvctl history myapp-dev -n 10
  cvctl history myapp-dev --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			n, _ := cmd.Flags().GetInt("number")
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

			events, err := e.Store.History(inst, n)
			if err != nil {
				return err
			}

			rep := historyReport{History: events}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "TIME\tKIND\tRESULT\tACTOR\tDETAIL")
				for _, ev := range events {
					at := ev.At.Format("2006-01-02 15:04:05")
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", at, ev.Kind, ev.Result, ev.Actor, ev.Detail)
				}
				tw.Flush()
			})
			return nil
		},
	}
	cmd.Flags().IntP("number", "n", 20, "number of events to show")
	return cmd
}

func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <instance> [service]",
		Short: "View container logs for an instance",
		Example: `  cvctl logs myapp-dev
  cvctl logs myapp-dev web
  cvctl logs myapp-dev --tail 50`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			service := ""
			if len(args) == 2 {
				service = args[1]
			}
			tail, _ := cmd.Flags().GetInt("tail")
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

			overridePath := filepath.Join(e.deployRoot(), inst, "compose.cvctl.yml")
			appEnvPath := filepath.Join(e.deployRoot(), inst, "app.env")

			var logsOut string
			if e.Deployer != nil && e.Deployer.Compose != nil {
				comp := e.Deployer.Compose(inst, overridePath, appEnvPath)
				out, err := comp.Logs(cmd.Context(), service, tail)
				if err != nil {
					return err
				}
				logsOut = out
			} else {
				cvctlEnvPath := filepath.Join(e.deployRoot(), inst, "cvctl.env")
				d0, _ := instance.Read(cvctlEnvPath, &contract.Project{})
				infraPath := "."
				if d0 != nil && d0.InfraPath != "" {
					infraPath = d0.InfraPath
				}
				p := compose.Project{
					Run:       e.runner(),
					Instance:  inst,
					Dir:       filepath.Join(e.deployRoot(), inst),
					InfraPath: infraPath,
					Override:  overridePath,
					EnvFile:   appEnvPath,
				}
				out, err := p.Logs(cmd.Context(), service, tail)
				if err != nil {
					return err
				}
				logsOut = out
			}

			rep := logsReport{Logs: logsOut}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				fmt.Fprint(w, logsOut)
			})
			return nil
		},
	}
	cmd.Flags().Int("tail", 100, "number of lines to show")
	return cmd
}
