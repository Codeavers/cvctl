package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"

	"github.com/joho/godotenv"
	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

var validEnvKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

type setEnvReport struct {
	Instance string   `json:"instance"`
	Key      string   `json:"key"`
	Warnings []string `json:"warnings,omitempty"`
	Next     string   `json:"next,omitempty"`
}

type getEnvReport struct {
	Values map[string]string `json:"values"`
}

type setStrategyReport struct {
	Instance string `json:"instance"`
	Strategy string `json:"strategy"`
}

func maskEnvValue(val string) string {
	r := []rune(val)
	if len(r) <= 6 {
		return "***"
	}
	return string(r[:2]) + "***"
}

func readAppEnv(fsys sys.FS, path string) (map[string]string, error) {
	data, err := fsys.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return make(map[string]string), nil
		}
		return nil, &output.Error{
			Code:    "E_APPENV_READ",
			Message: fmt.Sprintf("cannot read %s: %v", path, err),
			Hint:    "check file permissions on app.env",
		}
	}
	vals, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, &output.Error{
			Code:    "E_APPENV_READ",
			Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", path),
			Hint:    "fix syntax in app.env or remove the file",
		}
	}
	return vals, nil
}

func newSetEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-env <instance> <key> [value]",
		Short: "Set an environment variable in app.env",
		Example: `  cvctl set-env myapp-dev SECRET_KEY
  cvctl set-env myapp-dev LOG_LEVEL debug --json`,
		Args: cobra.RangeArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			key := args[1]
			asJSON, _ := cmd.Flags().GetBool("json")

			if !validEnvKey.MatchString(key) {
				return &output.UsageError{
					Err: &output.Error{
						Code:    "E_SETENV_KEY_INVALID",
						Message: fmt.Sprintf("invalid environment variable name %q: must match ^[A-Za-z_][A-Za-z0-9_]*$", key),
						Hint:    "use letters, numbers and underscores, starting with a letter or underscore",
					},
				}
			}

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

			val := ""
			if len(args) == 3 {
				val = args[2]
			} else {
				if !e.Interactive || e.Prompt == nil {
					return &output.UsageError{
						Err: &output.Error{
							Code:    "E_USAGE_SET",
							Message: "value argument required in non-interactive mode",
							Hint:    fmt.Sprintf("pass the value: cvctl set-env %s %s <value>", inst, key),
						},
					}
				}
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:    key,
					Prompt: fmt.Sprintf("Value for %s", key),
					Secret: true,
				}, "")
				if err != nil {
					return err
				}
				val = ans
			}

			unlock, err := e.Store.Lock(inst)
			if err != nil {
				return err
			}
			defer unlock()

			appEnvPath := filepath.Join(e.deployRoot(), inst, "app.env")
			existing, err := readAppEnv(e.fs(), appEnvPath)
			if err != nil {
				return err
			}

			existing[key] = val

			cvctlEnvPath := filepath.Join(e.deployRoot(), inst, "cvctl.env")
			d0, _ := instance.Read(cvctlEnvPath, &contract.Project{})
			infraPath := "."
			if d0 != nil && d0.InfraPath != "" {
				infraPath = d0.InfraPath
			}
			specPath := filepath.Join(e.deployRoot(), inst, "infra", infraPath, "env.spec.yml")
			var warnings []string
			spec, sErr := envspec.Load(specPath)
			if sErr != nil || spec == nil || spec.Vars == nil {
				warnings = append(warnings, fmt.Sprintf("%s is not in env.spec.yml", key))
			} else if _, ok := spec.Vars[key]; !ok {
				warnings = append(warnings, fmt.Sprintf("%s is not in env.spec.yml", key))
			}

			next := ""
			st, sLoadErr := e.Store.Load(inst)
			if sLoadErr == nil && st.Status == state.StatusBlocked {
				allResolved := true
				for _, bk := range st.Blocked {
					if v, ok := existing[bk]; !ok || v == "" {
						allResolved = false
						break
					}
				}
				if allResolved {
					next = "next agent cycle will deploy; or run cvctl deploy " + inst
				}
			}

			if err := e.fs().WriteFile(appEnvPath, instance.FormatEnv(existing), 0o640, deployOwner); err != nil {
				return err
			}

			rep := setEnvReport{
				Instance: inst,
				Key:      key,
				Warnings: warnings,
				Next:     next,
			}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				for _, warn := range warnings {
					fmt.Fprintf(w, "warning: %s\n", warn)
				}
				fmt.Fprintf(w, "%s: set %s\n", inst, key)
				if next != "" {
					fmt.Fprintln(w, next)
				}
			})
			return nil
		},
	}
	return cmd
}

func newGetEnvCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get-env <instance>",
		Short: "Get environment variables from app.env",
		Example: `  cvctl get-env myapp-dev
  cvctl get-env myapp-dev --reveal
  cvctl get-env myapp-dev --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			reveal, _ := cmd.Flags().GetBool("reveal")
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

			appEnvPath := filepath.Join(e.deployRoot(), inst, "app.env")
			vals, err := readAppEnv(e.fs(), appEnvPath)
			if err != nil {
				return err
			}

			outVals := make(map[string]string, len(vals))
			for k, v := range vals {
				if reveal {
					outVals[k] = v
				} else {
					outVals[k] = maskEnvValue(v)
				}
			}

			rep := getEnvReport{Values: outVals}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				keys := make([]string, 0, len(outVals))
				for k := range outVals {
					keys = append(keys, k)
				}
				slices.Sort(keys)
				for _, k := range keys {
					fmt.Fprintf(w, "%s=%s\n", k, outVals[k])
				}
			})
			return nil
		},
	}
	cmd.Flags().Bool("reveal", false, "reveal secret values without masking")
	return cmd
}

func newSetStrategyCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "set-strategy <instance> <strategy>",
		Short: "Set deployment strategy for an instance",
		Example: `  cvctl set-strategy myapp-dev recreate
  cvctl set-strategy myapp-dev blue-green --json`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			inst := args[0]
			strategy := args[1]
			asJSON, _ := cmd.Flags().GetBool("json")

			if strategy == "blue-green" {
				return &output.Error{
					Code:    "E_STRATEGY_UNSUPPORTED",
					Message: "blue-green strategy is not supported yet",
					Hint:    "use recreate strategy",
				}
			}
			if strategy != "recreate" {
				return &output.Error{
					Code:    "E_STRATEGY_UNSUPPORTED",
					Message: fmt.Sprintf("strategy %q is not supported", strategy),
					Hint:    "use recreate strategy",
				}
			}

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

			cvctlEnvPath := filepath.Join(e.deployRoot(), inst, "cvctl.env")
			data, err := e.fs().ReadFile(cvctlEnvPath)
			if err != nil {
				return instanceNotFound(inst)
			}
			m, err := godotenv.Unmarshal(string(data))
			if err != nil {
				return &output.Error{
					Code:    "E_DECISION_READ",
					Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", cvctlEnvPath),
					Hint:    "fix syntax in cvctl.env",
				}
			}

			m["CVCTL_STRATEGY"] = strategy
			if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(m), 0o640, deployOwner); err != nil {
				return err
			}

			rep := setStrategyReport{Instance: inst, Strategy: strategy}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				fmt.Fprintf(w, "%s: strategy set to %s\n", inst, strategy)
			})
			return nil
		},
	}
	return cmd
}
