package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/project"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

type projectOpts struct {
	NonInteractive bool
	In             io.Reader
	Out            io.Writer
}

type projectReport struct {
	Instance     string       `json:"instance"`
	Status       state.Status `json:"status"`
	DeployKeyPub string       `json:"deploy_key_pub,omitempty"`
	Missing      []string     `json:"missing,omitempty"`
}

// newProjectEnv builds the environment; tests replace it with fakes.
var newProjectEnv = func(o projectOpts) (*project.Env, error) {
	run := sys.NewExecRunner()
	fsys := sys.NewOSFS()
	interactive := !o.NonInteractive && term.IsTerminal(int(os.Stdin.Fd()))

	cfg, secrets, err := hostcfg.Load(fsys, hostCfgDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cat, err := kinds.Load()
	if err != nil {
		return nil, err
	}

	var prompter hostcfg.Prompter
	if interactive {
		prompter = hostcfg.NewHuhPrompter(o.In, o.Out)
	}

	var regResolver registry.Resolver
	if cfg != nil {
		token, _ := secrets.Get("registry_token")
		regResolver = registry.New(cfg.RegistryUser, token, registry.InsecureHostsFromEnv())
	}

	store := state.NewStore(fsys, "/var/lib/cvctl")
	deployer := &deploy.Deployer{
		DeployRoot: "/srv/deploy",
		TraefikDir: "/etc/traefik/dynamic",
		FS:         fsys,
		Store:      store,
		Host:       cfg,
		Secrets:    secrets,
		Catalog:    cat,
		Notify:     notifierFor(cfg, secrets),
	}
	deploy.Wire(deployer, run)

	return &project.Env{
		Run:         run,
		FS:          fsys,
		Prompt:      prompter,
		Interactive: interactive,
		Host:        cfg,
		Secrets:     secrets,
		Catalog:     cat,
		Deployer:    deployer,
		DeployRoot:  "/srv/deploy",
		StateRoot:   "/var/lib/cvctl",
		Resolver:    regResolver,
	}, nil
}

func parseSet(setFlags []string) (map[string]string, error) {
	out := make(map[string]string, len(setFlags))
	for _, s := range setFlags {
		k, v, found := strings.Cut(s, "=")
		if !found || strings.TrimSpace(k) == "" {
			return nil, &output.UsageError{Err: &output.Error{
				Code:    "E_USAGE_SET",
				Message: fmt.Sprintf("invalid --set flag %q: expected KEY=VALUE", s),
				Hint:    "pass --set KEY=VALUE, for example --set LOG_LEVEL=debug",
			}}
		}
		out[strings.TrimSpace(k)] = v
	}
	return out, nil
}

func parseDomains(domainFlags []string) (map[string]string, error) {
	out := make(map[string]string, len(domainFlags))
	for _, s := range domainFlags {
		k, v, found := strings.Cut(s, "=")
		if !found || strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return nil, &output.UsageError{Err: &output.Error{
				Code:    "E_USAGE_DOMAIN",
				Message: fmt.Sprintf("invalid --domain flag %q: expected svc=host", s),
				Hint:    "pass --domain <service>=<hostname>, for example --domain api=api.example.com",
			}}
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}

func newProjectCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "project",
		Short: "Manage cvctl projects and deployments",
		Example: `  cvctl project add git@github.com:myorg/my-infra.git
  cvctl project update myapp-dev`,
	}
	cmd.AddCommand(newProjectAddCmd())
	cmd.AddCommand(newProjectUpdateCmd())
	return cmd
}

func newProjectAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <repo-url>",
		Short: "Add and deploy a new project instance",
		Example: `  cvctl project add git@github.com:myorg/my-infra.git
  cvctl project add https://github.com/myorg/my-infra.git --env prod --branch main
  cvctl project add git@github.com:myorg/my-infra.git --set FOO=bar --domain api=api.example.com --non-interactive`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			repoURL := args[0]
			projectName, _ := cmd.Flags().GetString("name")
			envName, _ := cmd.Flags().GetString("env")
			branch, _ := cmd.Flags().GetString("branch")
			tag, _ := cmd.Flags().GetString("tag")
			strategy, _ := cmd.Flags().GetString("strategy")
			nonInt, _ := cmd.Flags().GetBool("non-interactive")
			asJSON, _ := cmd.Flags().GetBool("json")
			setFlags, _ := cmd.Flags().GetStringArray("set")
			domainFlags, _ := cmd.Flags().GetStringArray("domain")

			setMap, err := parseSet(setFlags)
			if err != nil {
				return err
			}
			domainMap, err := parseDomains(domainFlags)
			if err != nil {
				return err
			}

			e, err := newProjectEnv(projectOpts{
				NonInteractive: nonInt || asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			res, err := project.Add(cmd.Context(), e, repoURL, project.AddOptions{
				Project:  projectName,
				Env:      envName,
				Branch:   branch,
				TrackTag: tag,
				Strategy: strategy,
				Set:      setMap,
				Domains:  domainMap,
			})
			if err != nil {
				return err
			}

			rep := projectReport{
				Instance:     res.Instance,
				Status:       res.Deploy.Status,
				DeployKeyPub: res.DeployKeyPub,
				Missing:      res.Deploy.Missing,
			}

			if res.Deploy.Status == state.StatusFailed {
				failedErr := &output.Error{
					Code:    "E_DEPLOY_FAILED",
					Message: fmt.Sprintf("deploy failed: %s", res.Deploy.Detail),
					Hint:    "check container logs with cvctl logs " + res.Instance,
				}
				if asJSON {
					return &output.DataError{Err: failedErr, Data: rep}
				}
				writeProjectHuman(cmd.OutOrStdout(), rep)
				return failedErr
			}

			printerFrom(cmd).Result(rep, func(w io.Writer) {
				writeProjectHuman(w, rep)
			})
			return nil
		},
	}
	cmd.Flags().StringP("name", "n", "", "override project name")
	cmd.Flags().StringP("env", "e", "dev", "target environment (dev|prod)")
	cmd.Flags().StringP("branch", "b", "", "infra git branch to clone")
	cmd.Flags().StringP("tag", "t", "", "image tag to track (dev only)")
	cmd.Flags().String("strategy", "recreate", "deployment strategy (recreate|blue-green)")
	cmd.Flags().Bool("non-interactive", false, "never prompt; use defaults and --set values")
	cmd.Flags().StringArray("set", nil, "pre-seed app.env values (KEY=VALUE, repeatable)")
	cmd.Flags().StringArray("domain", nil, "domain for exposed service (svc=host, repeatable)")
	return cmd
}

func newProjectUpdateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "update <instance>",
		Short: "Update an existing project instance",
		Example: `  cvctl project update myapp-dev
  cvctl project update myapp-prod --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			nonInt, _ := cmd.Flags().GetBool("non-interactive")
			asJSON, _ := cmd.Flags().GetBool("json")

			e, err := newProjectEnv(projectOpts{
				NonInteractive: nonInt || asJSON,
				In:             cmd.InOrStdin(),
				Out:            cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}

			res, err := project.Update(cmd.Context(), e, inst)
			if err != nil {
				return err
			}

			rep := projectReport{
				Instance:     res.Instance,
				Status:       res.Deploy.Status,
				DeployKeyPub: res.DeployKeyPub,
				Missing:      res.Deploy.Missing,
			}

			if res.Deploy.Status == state.StatusFailed {
				failedErr := &output.Error{
					Code:    "E_DEPLOY_FAILED",
					Message: fmt.Sprintf("deploy failed: %s", res.Deploy.Detail),
					Hint:    "check container logs with cvctl logs " + res.Instance,
				}
				if asJSON {
					return &output.DataError{Err: failedErr, Data: rep}
				}
				writeProjectHuman(cmd.OutOrStdout(), rep)
				return failedErr
			}

			printerFrom(cmd).Result(rep, func(w io.Writer) {
				writeProjectHuman(w, rep)
			})
			return nil
		},
	}
	cmd.Flags().Bool("non-interactive", false, "never prompt; use defaults")
	return cmd
}

func writeProjectHuman(w io.Writer, rep projectReport) {
	if len(rep.Missing) > 0 {
		fmt.Fprintf(w, "%s: %s (missing: %s)\n", rep.Instance, rep.Status, strings.Join(rep.Missing, ", "))
		return
	}
	fmt.Fprintf(w, "%s: %s\n", rep.Instance, rep.Status)
	if rep.DeployKeyPub != "" {
		fmt.Fprintf(w, "deploy key:\n%s\n", strings.TrimSpace(rep.DeployKeyPub))
	}
}
