package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

func newVersionCmd(version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the cvctl version",
		Example: `  cvctl version
  cvctl version --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			printerFrom(cmd).Result(map[string]string{"version": version}, func(w io.Writer) {
				fmt.Fprintln(w, version)
			})
			return nil
		},
	}
}
