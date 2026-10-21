package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/kinds"
)

type instanceStatusItem struct {
	Instance string `json:"instance"`
	Env      string `json:"env"`
	Strategy string `json:"strategy,omitempty"`
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
				instances = e.allInstances()
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
				if st != nil && st.Current != nil && (st.Current.InfraCommit != "" || st.Current.InfraDigest != "") {
					commitStr = deploy.RevShort(*st.Current)
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

				strategy := ""
				if d, err := instance.Read(filepath.Join(e.deployRoot(), inst, "cvctl.env"), &contract.Project{}); err == nil {
					strategy = d.Strategy
				}

				items = append(items, instanceStatusItem{
					Instance: inst,
					Env:      envName,
					Strategy: strategy,
					Status:   statusStr,
					Commit:   commitStr,
					Age:      ageStr,
				})
			}

			warnings := make([]string, 0)
			if (e.Host != nil && e.Host.Storage == "local") || os.Getenv("CVCTL_STORAGE") == "local" {
				warnings = append(warnings, "CVCTL_STORAGE=local: backups are stored on local disk; configure S3 for offsite backup")
			}

			if deploy.BackupTargetFrom(e.Host, e.Secrets) == nil && e.backupsNeeded(instances) {
				warnings = append(warnings, "backups are not configured: check CVCTL_S3_* and /etc/cvctl/secrets (run cvctl init)")
			}

			warnings = append(warnings, e.s3Warnings(instances)...)

			rep := statusReport{
				Instances: items,
				Warnings:  warnings,
			}

			printerFrom(cmd).Result(rep, func(w io.Writer) {
				for _, warn := range warnings {
					fmt.Fprintf(w, "warning: %s\n", warn)
				}
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "INSTANCE\tENV\tSTRATEGY\tSTATUS\tCOMMIT\tAGE")
				for _, it := range items {
					strategy := it.Strategy
					if strategy == "" {
						strategy = "-"
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", it.Instance, it.Env, strategy, it.Status, it.Commit, it.Age)
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
					detail := ev.Detail
					if len(ev.BlueGreen) > 0 {
						detail = strings.TrimSpace(detail + " (blue-green: " + strings.Join(ev.BlueGreen, ",") + ")")
					}
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", at, ev.Kind, ev.Result, ev.Actor, detail)
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
				p := compose.Project{
					Run:      e.runner(),
					Instance: inst,
					Dir:      filepath.Join(e.deployRoot(), inst),
					Override: overridePath,
					EnvFile:  appEnvPath,
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

// backupsNeeded reports whether any instance has a local data service whose
// backup (cvctl.backup label, else the kind default) is wal-g or volume.
func (e *OpsEnv) backupsNeeded(instances []string) bool {
	cat := e.Catalog
	if cat == nil {
		var err error
		if cat, err = kinds.Load(); err != nil {
			return false
		}
	}
	for _, inst := range instances {
		envPath := filepath.Join(e.deployRoot(), inst, "cvctl.env")
		d0, err := instance.Read(envPath, &contract.Project{})
		if err != nil {
			continue
		}
		infra := d0.InfraPath
		if infra == "" {
			infra = "."
		}
		p, err := contract.Load(filepath.Join(e.deployRoot(), inst, "infra", infra), cat)
		if err != nil {
			continue
		}
		d, err := instance.Read(envPath, p)
		if err != nil {
			continue
		}
		for _, ds := range p.Data {
			if b := ds.Backup; d.Services[ds.Name].Mode == "local" && (b == "wal-g" || b == "volume") {
				return true
			}
		}
	}
	return false
}

func (e *OpsEnv) s3Warnings(instances []string) []string {
	cat := e.Catalog
	if cat == nil {
		var err error
		if cat, err = kinds.Load(); err != nil {
			return nil
		}
	}
	loader := backup.LoadInstance(e.deployRoot(), cat)
	var warnings []string
	for _, inst := range instances {
		p, d, err := loader(inst)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", inst, err))
			continue
		}
		if len(backup.Targets(p, d)) > 0 {
			if !d.Backup.HasKeys() {
				warnings = append(warnings, fmt.Sprintf("%s: backups use the host keys; run cvctl project update %s", inst, inst))
			}
			if e.Host != nil && e.Host.Storage == "s3" && (d.Backup.Ext["access_key"] == "" || d.Backup.Ext["secret_key"] == "") {
				warnings = append(warnings, fmt.Sprintf("%s: backups use the host S3 credentials; any instance can delete them; per-instance backup credentials (--ext backup.*) can only be set at project add", inst))
			}
		}
		for _, ds := range p.Data {
			sd := d.Services[ds.Name]
			if d.Env != "prod" && sd.Mode == "local" {
				if pub := p.PublicBuckets(ds.Name); len(pub) > 0 {
					names := make([]string, 0, len(pub))
					for _, b := range pub {
						names = append(names, b.Name)
					}
					slices.Sort(names)
					warnings = append(warnings, fmt.Sprintf("%s: %s serves public bucket(s) %s from local silo; use external storage in prod", inst, ds.Name, strings.Join(names, ", ")))
				}
			}
			if k, ok := cat[ds.Kind]; ok && k.Buckets && sd.Mode == "external" {
				warnings = append(warnings, fmt.Sprintf("%s: %s is external; cvctl does not back it up (use bucket versioning or replication)", inst, ds.Name))
			}
		}

		orphansPath := filepath.Join(e.stateRoot(), inst, "s3-orphans.json")
		if data, err := e.fs().ReadFile(orphansPath); err == nil {
			var orphans map[string][]string
			if err := json.Unmarshal(data, &orphans); err == nil && len(orphans) > 0 {
				svcs := make([]string, 0, len(orphans))
				for svc := range orphans {
					svcs = append(svcs, svc)
				}
				slices.Sort(svcs)
				for _, svc := range svcs {
					names := orphans[svc]
					if len(names) == 0 {
						continue
					}
					sortedNames := slices.Clone(names)
					slices.Sort(sortedNames)
					warnings = append(warnings, fmt.Sprintf("%s: %s has buckets no longer declared: %s", inst, svc, strings.Join(sortedNames, ", ")))
				}
			}
		}
	}
	return warnings
}

// allInstances lists the instances known to the state store or present under the deploy root.
func (e *OpsEnv) allInstances() []string {
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
	instances := make([]string, 0, len(seen))
	for name := range seen {
		instances = append(instances, name)
	}
	slices.Sort(instances)
	return instances
}
