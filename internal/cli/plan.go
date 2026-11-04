package cli

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
)

const planSchema = 1

type planReport struct {
	Schema    int                     `json:"schema"`
	Instances map[string]planInstance `json:"instances"`
}

type planApp struct {
	Image  string `json:"image"`
	Expose int    `json:"expose"`
	Domain string `json:"domain"`
}

type planData struct {
	Kind    string `json:"kind"`
	Version string `json:"version"`
	Mode    string `json:"mode"`
	Backup  string `json:"backup"`
	// Buckets maps bucket name to "public" or "private".
	Buckets map[string]string `json:"buckets,omitempty"`
}

type planHealth struct {
	Service string `json:"service"`
	HTTP    string `json:"http"`
	Port    int    `json:"port"`
	Timeout string `json:"timeout"`
}

// planInstance holds only values derived from the contract, the catalog and
// non-secret decisions: env values, credentials and git URLs never appear.
type planInstance struct {
	Env      string              `json:"env,omitempty"`
	Strategy string              `json:"strategy,omitempty"`
	Source   string              `json:"source,omitempty"`
	Track    string              `json:"track,omitempty"`
	Health   *planHealth         `json:"health,omitempty"`
	Hooks    map[string]int      `json:"hooks,omitempty"`
	Apps     map[string]planApp  `json:"apps,omitempty"`
	Data     map[string]planData `json:"data,omitempty"`
	// Error is set when the instance cannot be read; the other fields are then partial.
	Error string `json:"error,omitempty"`
}

func newPlanCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "plan",
		Short: "Describe what this binary would do for each instance (read-only, offline)",
		Example: `  cvctl plan
  cvctl plan --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			e, err := newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			rep := planReport{Schema: planSchema, Instances: map[string]planInstance{}}
			for _, inst := range e.allInstances() {
				rep.Instances[inst] = e.planInstance(inst)
			}
			printerFrom(cmd).Result(rep, func(w io.Writer) { writePlan(w, rep) })
			return nil
		},
	}
}

// planInstance reads cvctl.env and the installed contract only: no network, no docker.
func (e *OpsEnv) planInstance(inst string) planInstance {
	var pi planInstance
	dir := filepath.Join(e.deployRoot(), inst)
	envPath := filepath.Join(dir, "cvctl.env")
	d0, err := instance.Read(envPath, &contract.Project{})
	if err != nil {
		pi.Error = errText(err)
		return pi
	}
	pi.Env = d0.Env
	pi.Strategy = d0.Strategy
	pi.Source = "git"
	if d0.IsOCI() {
		pi.Source = "oci"
	}
	if d0.Env == "dev" {
		pi.Track = d0.TrackTag
	}
	cat, err := e.catalog()
	if err != nil {
		pi.Error = errText(err)
		return pi
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	p, err := contract.Load(filepath.Join(dir, "infra", infraPath), cat)
	if err != nil {
		pi.Error = errText(err)
		return pi
	}
	d, err := instance.Read(envPath, p)
	if err != nil {
		pi.Error = errText(err)
		return pi
	}
	pi.Health = &planHealth{
		Service: p.X.Health.Service,
		HTTP:    p.X.Health.HTTP,
		Port:    p.HealthPort(),
		Timeout: p.X.Health.Timeout.String(),
	}
	pi.Hooks = map[string]int{"pre_deploy": len(p.X.Hooks.PreDeploy), "post_deploy": len(p.X.Hooks.PostDeploy)}
	pi.Apps = map[string]planApp{}
	for _, a := range p.Apps {
		pi.Apps[a.Name] = planApp{Image: stripDigest(a.Image), Expose: a.ExposePort, Domain: d.Domains[a.Name]}
	}
	pi.Data = map[string]planData{}
	for _, ds := range p.Data {
		mode := d.Services[ds.Name].Mode
		if mode == "" {
			mode = "local"
		}
		pd := planData{Kind: ds.Kind, Version: ds.Version, Mode: mode, Backup: ds.Backup}
		for _, b := range p.Buckets(ds.Name) {
			if pd.Buckets == nil {
				pd.Buckets = map[string]string{}
			}
			pd.Buckets[b.Name] = "private"
			if b.Public {
				pd.Buckets[b.Name] = "public"
			}
		}
		pi.Data[ds.Name] = pd
	}
	return pi
}

func stripDigest(image string) string {
	before, _, _ := strings.Cut(image, "@")
	return before
}

// writePlan prints the same document as --json, indented; encoding/json sorts map keys.
func writePlan(w io.Writer, rep planReport) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}
