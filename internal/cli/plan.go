package cli

import (
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
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
	// Aliases and Redirects are omitted when empty so the plan of an instance
	// without them stays what older versions print.
	Aliases   []string `json:"aliases,omitempty"`
	Redirects []string `json:"redirects,omitempty"`
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
	// Issues lists the codes a deploy would reject the instance with. Messages
	// quote decision values, so they never appear here.
	Issues []string `json:"issues,omitempty"`
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
	infraDir := filepath.Join(dir, "infra", infraPath)
	p, err := contract.Load(infraDir, cat)
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
		pa := planApp{Image: stripDigest(a.Image), Expose: a.ExposePort, Domain: d.Domains[a.Name], Aliases: d.Aliases[a.Name]}
		for _, r := range d.Redirects[a.Name] {
			pa.Redirects = append(pa.Redirects, r.From+"="+r.To)
		}
		pi.Apps[a.Name] = pa
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
	pi.Issues = planIssues(infraDir, p, d, cat)
	return pi
}

// planIssues runs the checks of a deploy. Repeated codes are kept: a version
// that adds a finding under a code already listed must still differ. Digests
// come from the desired state at deploy time, not from cvctl.env.
func planIssues(infraDir string, p *contract.Project, d *instance.Decisions, cat kinds.Catalog) []string {
	var codes []string
	spec, err := envspec.Load(filepath.Join(infraDir, "env.spec.yml"))
	if err != nil {
		code := "E_ENVSPEC_INVALID"
		var oe *output.Error
		if errors.As(err, &oe) {
			code = oe.Code
		}
		codes = append(codes, code)
	}
	issues := append(contract.Validate(p, cat, spec), instance.Check(instance.WithPlaceholderDigests(d, p), p, cat)...)
	for _, iss := range issues {
		codes = append(codes, iss.Code)
	}
	slices.Sort(codes)
	return codes
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
