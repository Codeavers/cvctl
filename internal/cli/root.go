// Package cli wires the cobra command tree to the output layer.
package cli

import (
	"io"
	"slices"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/output"
)

type printerKey struct{}

func NewRoot(version string) *cobra.Command {
	root := &cobra.Command{
		Use:           "cvctl",
		Short:         "Codeaver deploy CLI",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().Bool("json", false, "print exactly one JSON document on stdout")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return &output.UsageError{Err: err}
	})
	root.AddCommand(newVersionCmd(version))
	root.AddCommand(newLintCmd())
	root.AddCommand(newRenderCmd())
	return root
}

// Execute runs root in-process and returns the exit code.
func Execute(root *cobra.Command, args []string, stdout, stderr io.Writer) int {
	p := &output.Printer{Out: stdout, Err: stderr}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)

	// Arg/unknown-command validation fails before PersistentPreRun, so a
	// failure with ran==false is a usage error.
	ran := false
	root.PersistentPreRun = func(cmd *cobra.Command, _ []string) {
		ran = true
		p.JSON, _ = cmd.Flags().GetBool("json")
	}

	err := root.Execute()
	if err == nil {
		return output.ExitOK
	}
	if !ran {
		var u *output.UsageError
		if !asUsage(err, &u) {
			err = &output.UsageError{Err: err}
		}
		// Flag parsing may have stopped before --json was seen.
		p.JSON = slices.Contains(args, "--json")
	}
	return p.Fail(err)
}

func asUsage(err error, target **output.UsageError) bool {
	u, ok := err.(*output.UsageError)
	if ok {
		*target = u
	}
	return ok
}

func printerFrom(cmd *cobra.Command) *output.Printer {
	j, _ := cmd.Flags().GetBool("json")
	return &output.Printer{JSON: j, Out: cmd.OutOrStdout(), Err: cmd.ErrOrStderr()}
}
