package cli

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
)

type projectListItem struct {
	Instance string            `json:"instance"`
	Env      string            `json:"env"`
	Source   string            `json:"source"`
	Track    string            `json:"track,omitempty"`
	Domains  map[string]string `json:"domains"`
	Data     map[string]string `json:"data"`
	Paused   bool              `json:"paused"`
	// Error is set when the contract cannot be read; the other fields are then partial.
	Error string `json:"error,omitempty"`
}

type projectListReport struct {
	Instances []projectListItem `json:"instances"`
}

func newProjectListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List instances with their contract source, domains and data modes",
		Example: `  cvctl project list
  cvctl project list --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			e, err := newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			rep := projectListReport{Instances: []projectListItem{}}
			for _, inst := range e.allInstances() {
				rep.Instances = append(rep.Instances, e.projectListItem(inst))
			}
			printerFrom(cmd).Result(rep, func(w io.Writer) { writeProjectList(w, rep) })
			return nil
		},
	}
	return cmd
}

// projectListItem reads cvctl.env and the installed contract only: no network, no docker.
func (e *OpsEnv) projectListItem(inst string) projectListItem {
	it := projectListItem{Instance: inst, Domains: map[string]string{}, Data: map[string]string{}}
	if st, err := e.Store.Load(inst); err == nil && st != nil {
		it.Paused = st.Status == state.StatusPaused || st.PausedReason != ""
	}
	dir := filepath.Join(e.deployRoot(), inst)
	envPath := filepath.Join(dir, "cvctl.env")
	d0, err := instance.Read(envPath, &contract.Project{})
	if err != nil {
		it.Error = errText(err)
		return it
	}
	it.Env = d0.Env
	if d0.Env == "dev" {
		it.Track = d0.TrackTag
	}
	if d0.IsOCI() {
		it.Source = "oci://" + d0.InfraRef
	} else {
		it.Source = "git " + infraURL(envPath) + " " + d0.InfraBranch
		it.Source = strings.Join(strings.Fields(it.Source), " ")
	}
	cat, err := e.catalog()
	if err != nil {
		it.Error = errText(err)
		return it
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	p, err := contract.Load(filepath.Join(dir, "infra", infraPath), cat)
	if err != nil {
		it.Error = errText(err)
		return it
	}
	d, err := instance.Read(envPath, p)
	if err != nil {
		it.Error = errText(err)
		return it
	}
	for svc, host := range d.Domains {
		it.Domains[svc] = host
	}
	for _, ds := range p.Data {
		mode := d.Services[ds.Name].Mode
		if mode == "" {
			mode = "local"
		}
		it.Data[ds.Name] = mode
	}
	return it
}

// infraURL is the git URL recorded at add; cvctl.env holds secrets, so read errors are dropped.
func infraURL(envPath string) string {
	m, err := godotenv.Read(envPath)
	if err != nil {
		return ""
	}
	return m["CVCTL_INFRA_URL"]
}

func errText(err error) string {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe.Code + ": " + oe.Message
	}
	return err.Error()
}

func joinPairs(m map[string]string) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

func writeProjectList(w io.Writer, rep projectListReport) {
	tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
	fmt.Fprintln(tw, "INSTANCE\tENV\tSOURCE\tTRACK\tDOMAINS\tDATA\tPAUSED")
	for _, it := range rep.Instances {
		paused := "no"
		if it.Paused {
			paused = "yes"
		}
		track := it.Track
		if track == "" {
			track = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", it.Instance, it.Env, it.Source, track, joinPairs(it.Domains), joinPairs(it.Data), paused)
	}
	tw.Flush()
	for _, it := range rep.Instances {
		if it.Error != "" {
			fmt.Fprintf(w, "warning: %s: %s\n", it.Instance, it.Error)
		}
	}
}
