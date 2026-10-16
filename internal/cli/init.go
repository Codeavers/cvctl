package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/codeavers/cvctl/internal/host"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const hostCfgDir = "/etc/cvctl"

type initOpts struct {
	Adopt, NonInteractive bool
	In                    io.Reader
	Out                   io.Writer
}

// newInitEnv builds the real environment; tests replace it with fakes.
// Interactive is true only when the user did not opt out and stdin is a terminal.
var newInitEnv = func(o initOpts) (*host.Env, hostcfg.Facts, error) {
	run := sys.NewExecRunner()
	hostname := ""
	if res, err := run.Run(context.Background(), sys.Cmd{Name: "hostname", Args: []string{"-s"}}); err == nil && res.ExitCode == 0 {
		hostname = strings.TrimSpace(string(res.Stdout))
	}
	if hostname == "" {
		hostname, _ = os.Hostname()
	}
	interactive := !o.NonInteractive && term.IsTerminal(int(os.Stdin.Fd()))
	e := &host.Env{
		Run: run, FS: sys.NewOSFS(), Interactive: interactive, Adopt: o.Adopt,
		EUID: os.Geteuid(), Out: o.Out,
	}
	if interactive {
		e.Prompt = hostcfg.NewHuhPrompter(o.In, o.Out)
	}
	return e, hostcfg.Facts{Hostname: hostname, SudoUser: os.Getenv("SUDO_USER"), Adopt: o.Adopt}, nil
}

// initSteps is replaced in tests.
var initSteps = host.DefaultSteps

type initReport struct {
	Host    string            `json:"host"`
	Steps   []host.StepResult `json:"steps"`
	Pending []string          `json:"pending"`
}

func newInitCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Bring this host to the desired state (idempotent)",
		Example: `  sudo cvctl init
  sudo cvctl init --dry-run --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			adopt, _ := cmd.Flags().GetBool("adopt")
			dry, _ := cmd.Flags().GetBool("dry-run")
			nonInt, _ := cmd.Flags().GetBool("non-interactive")
			asJSON, _ := cmd.Flags().GetBool("json")

			e, facts, err := newInitEnv(initOpts{
				Adopt: adopt, NonInteractive: nonInt || asJSON,
				In: cmd.InOrStdin(), Out: cmd.ErrOrStderr(),
			})
			if err != nil {
				return err
			}
			if e.Out == nil {
				e.Out = cmd.ErrOrStderr()
			}
			if e.Cfg, e.Secrets, err = hostcfg.Load(e.FS, hostCfgDir); err != nil {
				return err
			}
			if err := hostcfg.Resolve(e.Cfg, e.Secrets, facts, e.Prompt, e.Interactive); err != nil {
				return err
			}
			// Saved before the steps so answers survive a failing step.
			if !dry {
				if err := hostcfg.Save(e.FS, hostCfgDir, e.Cfg, e.Secrets); err != nil {
					return err
				}
			}
			results := host.Run(cmd.Context(), e, initSteps(), dry)

			rep := initReport{Host: e.Cfg.HostName, Steps: results, Pending: []string{}}
			failed := false
			for _, r := range results {
				if r.Status == host.StatusFailed {
					failed = true
				}
				if r.Status == host.StatusPending {
					rep.Pending = append(rep.Pending, r.Note)
				}
			}
			printerFrom(cmd).Result(rep, func(w io.Writer) { writeInitHuman(w, rep) })
			if failed {
				if !asJSON {
					for _, r := range results {
						if r.Error != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "error %s: %s\nhint: %s\n", r.Error.Code, r.Error.Message, r.Error.Hint)
						}
					}
				}
				return &output.ReportedError{Msg: "a step failed"}
			}
			return nil
		},
	}
	cmd.Flags().Bool("adopt", false, "adopt an already-configured host (conservative defaults)")
	cmd.Flags().Bool("dry-run", false, "show what would change without changing anything")
	cmd.Flags().Bool("non-interactive", false, "never prompt; use host.env values and defaults")
	return cmd
}

func writeInitHuman(w io.Writer, rep initReport) {
	for _, r := range rep.Steps {
		detail := strings.Join(r.Changes, "; ")
		if detail == "" {
			detail = r.Note
		}
		fmt.Fprintf(w, "%-10s %s  %s\n", r.Status, r.Name, detail)
	}
	for _, n := range rep.Pending {
		fmt.Fprintf(w, "pending: %s\n", n)
	}
}
