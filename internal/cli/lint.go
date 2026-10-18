package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

func newLintCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "lint [dir]",
		Short: "Validate compose.yml and env.spec.yml against the cvctl contract",
		Example: `  cvctl lint
  cvctl lint ./infra --json`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			p, err := lintDir(dir)
			if err != nil {
				return err
			}

			res := map[string]int{"services": len(p.Apps), "data": len(p.Data)}
			printerFrom(cmd).Result(res, func(w io.Writer) {
				fmt.Fprintf(w, "ok: %d service(s), %d data service(s)\n", res["services"], res["data"])
			})
			return nil
		},
	}
}

// lintDir runs the contract and env spec checks shared by lint and publish.
func lintDir(dir string) (*contract.Project, error) {
	cat, err := kinds.Load()
	if err != nil {
		return nil, err
	}
	p, err := contract.Load(dir, cat)
	if err != nil {
		return nil, err
	}

	var extra []output.Issue
	specPath := filepath.Join(dir, "env.spec.yml")
	var spec *envspec.Spec
	if _, statErr := os.Stat(specPath); statErr != nil {
		extra = append(extra, output.Issue{
			Code:    "E_CONTRACT_NO_ENVSPEC",
			Message: "env.spec.yml not found",
			Hint:    "create env.spec.yml next to compose.yml declaring every variable the compose file references",
		})
	} else if spec, err = envspec.Load(specPath); err != nil {
		var oe *output.Error
		if !errors.As(err, &oe) {
			return nil, err
		}
		extra = append(extra, output.Issue{Code: oe.Code, Message: oe.Message, Hint: oe.Hint})
	}
	issues := append(contract.Validate(p, cat, spec), extra...)
	output.SortIssues(issues)
	if len(issues) > 0 {
		return nil, &output.IssuesError{Issues: issues}
	}

	return p, nil
}
