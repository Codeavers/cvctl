package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/kinds"
)

const renderIssuesHint = "fix cvctl.env (or the infra checkout) as each issue's hint says, then re-run render"

func newRenderCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render compose.cvctl.yml, traefik.yml and app.env for one instance (offline)",
		Example: `  cvctl render --instance-dir /srv/deploy/demo-dev
  cvctl render --instance-dir /srv/deploy/demo-dev --edge behind-proxy --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dir, _ := cmd.Flags().GetString("instance-dir")
			edge, _ := cmd.Flags().GetString("edge")
			if dir == "" {
				return &output.UsageError{Err: errors.New("--instance-dir is required")}
			}
			if edge != "edge" && edge != "behind-proxy" {
				return &output.UsageError{Err: fmt.Errorf("--edge must be edge or behind-proxy, got %q", edge)}
			}
			res, err := renderInstance(dir, edge)
			if err != nil {
				return err
			}
			printerFrom(cmd).Result(res.data(), func(w io.Writer) {
				for _, f := range res.Files {
					fmt.Fprintf(w, "wrote %s\n", f)
				}
				for _, k := range res.Missing {
					fmt.Fprintf(w, "missing %s: run cvctl set-env %s %s\n", k, res.Instance, k)
				}
			})
			return nil
		},
	}
	cmd.Flags().String("instance-dir", "", "instance directory holding cvctl.env, infra/ and app.env")
	cmd.Flags().String("edge", "edge", "edge | behind-proxy")
	return cmd
}

type renderResult struct {
	Instance  string
	Missing   []string
	Generated []string
	Files     []string
}

func (r renderResult) data() map[string]any {
	d := map[string]any{
		"instance":  r.Instance,
		"missing":   r.Missing,
		"generated": r.Generated,
		"files":     r.Files,
	}
	if len(r.Missing) > 0 {
		d["hint"] = fmt.Sprintf("cvctl set-env %s %s", r.Instance, r.Missing[0])
	}
	return d
}

func renderInstance(dir, edge string) (renderResult, error) {
	// env_file entries in the override must be absolute: compose resolves
	// relative paths against its own project directory, not the cwd.
	dir, err := filepath.Abs(dir)
	if err != nil {
		return renderResult{}, err
	}
	cat, err := kinds.Load()
	if err != nil {
		return renderResult{}, err
	}
	d0, err := instance.Read(filepath.Join(dir, "cvctl.env"), &contract.Project{})
	if err != nil {
		return renderResult{}, err
	}
	// InfraPath is needed to locate the contract, which Read needs to map
	// service keys; the first read only extracts the path.
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	infra := filepath.Join(dir, "infra", infraPath)
	p, err := contract.Load(infra, cat)
	if err != nil {
		return renderResult{}, err
	}
	d, err := instance.Read(filepath.Join(dir, "cvctl.env"), p)
	if err != nil {
		return renderResult{}, err
	}
	if d.InfraPath == "" {
		d.InfraPath = "."
	}

	specPath := filepath.Join(infra, "env.spec.yml")
	if _, statErr := os.Stat(specPath); statErr != nil {
		return renderResult{}, &output.IssuesError{Hint: renderIssuesHint, Issues: []output.Issue{{
			Code:    "E_CONTRACT_NO_ENVSPEC",
			Message: "env.spec.yml not found",
			Hint:    "create env.spec.yml next to compose.yml declaring every variable the compose file references",
		}}}
	}
	spec, err := envspec.Load(specPath)
	if err != nil {
		return renderResult{}, err
	}

	issues := append(contract.Validate(p, cat, spec), instance.Check(d, p, cat)...)
	if len(issues) > 0 {
		output.SortIssues(issues)
		return renderResult{}, &output.IssuesError{Issues: issues, Hint: renderIssuesHint}
	}

	svcVars, err := render.ServiceVars(p, d, cat)
	if err != nil {
		return renderResult{}, err
	}
	appEnvPath := filepath.Join(dir, "app.env")
	var existing map[string]string
	if _, statErr := os.Stat(appEnvPath); statErr == nil {
		if existing, err = godotenv.Read(appEnvPath); err != nil {
			return renderResult{}, &output.Error{
				Code:    "E_APPENV_READ",
				Message: fmt.Sprintf("cannot read %s: %v", appEnvPath, err),
				Hint:    "fix or remove the file; values set with cvctl set-env live there",
			}
		}
	}
	res, err := envspec.Resolve(spec, envspec.Input{
		Env: d.Env, Existing: existing, ServiceVars: svcVars, Domains: d.Domains,
	})
	if err != nil {
		return renderResult{}, err
	}

	opts := render.Options{InstanceDir: dir, Edge: edge}
	compose, err := render.Override(p, d, cat, opts)
	if err != nil {
		return renderResult{}, err
	}
	traefik, err := render.Traefik(p, d, opts)
	if err != nil {
		return renderResult{}, err
	}

	out := renderResult{
		Instance:  d.Instance,
		Missing:   nonNil(res.Missing),
		Generated: nonNil(res.Generated),
		Files:     []string{},
	}
	if out.Instance == "" {
		out.Instance = filepath.Base(filepath.Clean(dir))
	}

	// Order matters: app.env last, so a failure never leaves freshly generated
	// secrets on disk without the compose files that consume them.
	composePath := filepath.Join(dir, "compose.cvctl.yml")
	if err := instance.WriteFileAtomic(composePath, compose, 0o640); err != nil {
		return renderResult{}, err
	}
	out.Files = append(out.Files, composePath)
	traefikPath := filepath.Join(dir, "traefik.yml")
	if traefik != nil {
		if err := instance.WriteFileAtomic(traefikPath, traefik, 0o640); err != nil {
			return renderResult{}, err
		}
		out.Files = append(out.Files, traefikPath)
	} else if err := os.Remove(traefikPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return renderResult{}, err
	}
	if err := instance.WriteEnvFile(appEnvPath, res.Values); err != nil {
		return renderResult{}, err
	}
	out.Files = append(out.Files, appEnvPath)
	slices.Sort(out.Files)
	return out, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
