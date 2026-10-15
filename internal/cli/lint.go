package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

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
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			cat, err := kinds.Load()
			if err != nil {
				return err
			}
			p, err := contract.Load(dir, cat)
			if err != nil {
				return err
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
					return err
				}
				extra = append(extra, output.Issue{Code: oe.Code, Message: oe.Message, Hint: oe.Hint})
			}
			issues := append(contract.Validate(p, cat, spec), extra...)
			slices.SortFunc(issues, func(a, b output.Issue) int {
				return strings.Compare(a.Code+"\x00"+a.Service+"\x00"+a.Message, b.Code+"\x00"+b.Service+"\x00"+b.Message)
			})
			if len(issues) > 0 {
				return &output.IssuesError{Issues: issues}
			}

			res := map[string]int{"services": len(p.Apps), "data": len(p.Data)}
			printerFrom(cmd).Result(res, func(w io.Writer) {
				fmt.Fprintf(w, "ok: %d service(s), %d data service(s)\n", res["services"], res["data"])
			})
			return nil
		},
	}
}
