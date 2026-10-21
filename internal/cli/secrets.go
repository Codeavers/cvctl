package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/secrets"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	passphraseEnv   = "CVCTL_SECRETS_PASSPHRASE"
	allowRootEnv    = "CVCTL_ALLOW_SECRETS_ROOT"
	secretsFilePerm = 0o600
)

// Replaced by tests; the defaults are the real system.
var (
	secretsFS     = func() sys.FS { return sys.NewOSFS() }
	secretsRunner = func() sys.Runner { return sys.NewExecRunner() }
	secretsNow    = time.Now
)

var (
	hostArgRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]*$`)
	userArgRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]*$`)
	instArgRe = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
)

type secretsExportReport struct {
	Output   string   `json:"output"`
	Files    int      `json:"files,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
}

type secretsImportReport struct {
	Files []string `json:"files"`
}

func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Export or import every host and instance secret as an encrypted bundle",
		Example: `  sudo cvctl secrets export -o secrets.age
  sudo cvctl secrets import secrets.age`,
	}
	cmd.PersistentFlags().String("root", "", "")
	_ = cmd.PersistentFlags().MarkHidden("root")
	cmd.AddCommand(newSecretsExportCmd(), newSecretsImportCmd())
	return cmd
}

func secretsRoot(cmd *cobra.Command) (string, error) {
	root, _ := cmd.Flags().GetString("root")
	if root == "" {
		return "", nil
	}
	if os.Getenv(allowRootEnv) != "1" {
		return "", &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_ROOT",
			Message: "--root is only available for tests",
			Hint:    "remove --root",
		}}
	}
	return root, nil
}

func newSecretsExportCmd() *cobra.Command {
	var out, instance string
	var hostOnly bool
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Write an age-encrypted bundle of all secrets (run as root)",
		Long: `Write an age-encrypted bundle of host.env, the host secrets, every instance
cvctl.env/app.env/backup-*.env/restic-*.env and every deploy key.

Pass --instance <name> to bundle only that instance's env files, or --host to
bundle only host.env and /etc/cvctl/secrets/*. The two flags are mutually
exclusive; omitting both bundles everything.

The passphrase is typed twice on the terminal (hidden). Without a terminal it is
read from CVCTL_SECRETS_PASSPHRASE. It is never accepted as a flag. Use -o - to
write the (ASCII-armored) bundle to stdout.`,
		Example: `  sudo cvctl secrets export -o secrets.age
  sudo cvctl secrets export --instance shop-prod -o shop.age
  sudo cvctl secrets export --host -o host.age
  sudo cvctl secrets export -o - > secrets.age.txt`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if out == "" {
				return &output.UsageError{Err: &output.Error{
					Code: "E_USAGE_OUTPUT", Message: "-o is required", Hint: "pass -o FILE or -o -",
				}}
			}
			if (instance != "" || cmd.Flags().Changed("instance")) && hostOnly {
				return &output.UsageError{Err: &output.Error{
					Code:    "E_USAGE_SCOPE",
					Message: "--instance and --host cannot be used together",
					Hint:    "choose either --instance or --host, or omit both for a full bundle",
				}}
			}
			if cmd.Flags().Changed("instance") && (instance == "" || !instArgRe.MatchString(instance)) {
				return &output.UsageError{Err: &output.Error{
					Code:    "E_INSTANCE_NAME",
					Message: fmt.Sprintf("invalid instance name %q", instance),
					Hint:    "instance names must contain only lowercase letters, digits, and single hyphens",
				}}
			}
			root, err := secretsRoot(cmd)
			if err != nil {
				return err
			}
			fsys := secretsFS()
			var entries []secrets.Entry
			var warns []string
			switch {
			case hostOnly:
				entries, warns, err = secrets.CollectHost(fsys, root)
			case instance != "":
				entries, warns, err = secrets.CollectInstance(fsys, root, instance)
			default:
				entries, warns, err = secrets.Collect(fsys, root)
			}
			for _, w := range warns {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning: "+w)
			}
			if err != nil {
				return err
			}
			pass, err := readPassphrase(cmd, true)
			if err != nil {
				return err
			}
			var buf bytes.Buffer
			if err := secrets.Encrypt(&buf, entries, pass, out == "-"); err != nil {
				return err
			}
			if out == "-" {
				_, err := cmd.OutOrStdout().Write(buf.Bytes())
				return err
			}
			if err := fsys.WriteFile(out, buf.Bytes(), secretsFilePerm, sys.Owner{}); err != nil {
				return &output.Error{
					Code: "E_SECRETS_WRITE", Message: fmt.Sprintf("cannot write %s: %v", out, err),
					Hint: "choose a writable output path",
				}
			}
			printerFrom(cmd).Result(secretsExportReport{Output: out, Files: len(entries), Warnings: warns}, func(w io.Writer) {
				fmt.Fprintf(w, "wrote %d files to %s\n", len(entries), out)
			})
			return nil
		},
	}
	cmd.Flags().StringVarP(&out, "output", "o", "", "output file, or - for stdout")
	cmd.Flags().StringVar(&instance, "instance", "", "export only secrets for this instance")
	cmd.Flags().BoolVar(&hostOnly, "host", false, "export only host secrets (host.env and /etc/cvctl/secrets/*)")
	return cmd
}

func newSecretsImportCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "import <file|->",
		Short: "Restore a secrets bundle (run as root)",
		Long: `Decrypt a bundle made by cvctl secrets export and restore its files with their
stored mode and owner. Every entry is validated before anything is written.
Existing files are never overwritten unless --force is given.`,
		Example: `  sudo cvctl secrets import secrets.age
  sudo cvctl secrets import --force secrets.age`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			root, err := secretsRoot(cmd)
			if err != nil {
				return err
			}
			fsys := secretsFS()
			var data []byte
			if args[0] == "-" {
				data, err = io.ReadAll(cmd.InOrStdin())
			} else {
				data, err = fsys.ReadFile(args[0])
			}
			if err != nil {
				return &output.Error{
					Code: "E_SECRETS_READ", Message: fmt.Sprintf("cannot read the bundle %s: %v", args[0], err),
					Hint: "check the path",
				}
			}
			pass, err := readPassphrase(cmd, false)
			if err != nil {
				return err
			}
			entries, err := secrets.Decrypt(bytes.NewReader(data), pass)
			if err != nil {
				return err
			}
			paths, err := secrets.Restore(fsys, root, entries, force)
			if err != nil {
				return err
			}
			printerFrom(cmd).Result(secretsImportReport{Files: paths}, func(w io.Writer) {
				for _, p := range paths {
					fmt.Fprintln(w, p)
				}
			})
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite files that already exist")
	return cmd
}

// readPassphrase prompts on the terminal when there is one, otherwise it falls
// back to the environment. The value never reaches errors or logs.
func readPassphrase(cmd *cobra.Command, confirm bool) (string, error) {
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		errw := cmd.ErrOrStderr()
		fmt.Fprint(errw, "Passphrase: ")
		first, err := term.ReadPassword(fd)
		fmt.Fprintln(errw)
		if err != nil {
			return "", passphraseErr("cannot read the passphrase from the terminal")
		}
		if len(first) == 0 {
			return "", passphraseErr("the passphrase is empty")
		}
		if confirm {
			fmt.Fprint(errw, "Repeat passphrase: ")
			second, err := term.ReadPassword(fd)
			fmt.Fprintln(errw)
			if err != nil {
				return "", passphraseErr("cannot read the passphrase from the terminal")
			}
			if !bytes.Equal(first, second) {
				return "", passphraseErr("the passphrases do not match")
			}
		}
		return string(first), nil
	}
	if v := os.Getenv(passphraseEnv); v != "" {
		return v, nil
	}
	return "", passphraseErr("no passphrase available: not a terminal and " + passphraseEnv + " is not set")
}

func passphraseErr(msg string) error {
	return &output.Error{
		Code:    "E_SECRETS_PASSPHRASE",
		Message: msg,
		Hint:    "run on a terminal, or set " + passphraseEnv + " for non-interactive use",
	}
}

func newRemoteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "remote",
		Short:   "Operate on a remote host over ssh from your machine",
		Example: `  cvctl remote export server.example.com --user admin`,
	}
	cmd.AddCommand(newRemoteExportCmd())
	return cmd
}

func newRemoteExportCmd() *cobra.Command {
	var port int
	var user, out, instance string
	var hostOnly bool
	cmd := &cobra.Command{
		Use:   "export <host>",
		Short: "Fetch an encrypted secrets bundle from a host over ssh",
		Long: `Run "sudo cvctl secrets export" on the host over ssh -t and save the bundle
locally (0600). Pass --instance <name> or --host to export only a single instance
or the host secrets. The remote prompts for the passphrase on your terminal.`,
		Example: `  cvctl remote export server.example.com --user admin
  cvctl remote export server.example.com --port 2228 -o backup.age
  cvctl remote export server.example.com --instance shop-prod
  cvctl remote export server.example.com --host`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			host := args[0]
			if !hostArgRe.MatchString(host) {
				return &output.UsageError{Err: &output.Error{
					Code: "E_USAGE_HOST", Message: fmt.Sprintf("invalid host %q", host),
					Hint: "pass a host name or IP address",
				}}
			}
			if user != "" && !userArgRe.MatchString(user) {
				return &output.UsageError{Err: &output.Error{
					Code: "E_USAGE_USER", Message: fmt.Sprintf("invalid user %q", user),
					Hint: "pass a plain unix user name",
				}}
			}
			if port < 1 || port > 65535 {
				return &output.UsageError{Err: &output.Error{
					Code: "E_USAGE_PORT", Message: fmt.Sprintf("invalid port %d", port),
					Hint: "pass a port between 1 and 65535",
				}}
			}
			if (instance != "" || cmd.Flags().Changed("instance")) && hostOnly {
				return &output.UsageError{Err: &output.Error{
					Code:    "E_USAGE_SCOPE",
					Message: "--instance and --host cannot be used together",
					Hint:    "choose either --instance or --host, or omit both for a full bundle",
				}}
			}
			if cmd.Flags().Changed("instance") && (instance == "" || !instArgRe.MatchString(instance)) {
				return &output.UsageError{Err: &output.Error{
					Code:    "E_INSTANCE_NAME",
					Message: fmt.Sprintf("invalid instance name %q", instance),
					Hint:    "instance names must contain only lowercase letters, digits, and single hyphens",
				}}
			}
			if out == "" {
				out = fmt.Sprintf("cvctl-secrets-%s-%s.age", strings.ReplaceAll(host, ":", "_"), secretsNow().Format("2006-01-02"))
			}
			target := host
			if user != "" {
				target = user + "@" + host
			}
			sshArgs := []string{"-t", "-p", fmt.Sprint(port), target, "sudo", "cvctl", "secrets", "export", "-o", "-"}
			if hostOnly {
				sshArgs = append(sshArgs, "--host")
			} else if instance != "" {
				sshArgs = append(sshArgs, "--instance", instance)
			}
			var raw bytes.Buffer
			c := sys.Cmd{
				Name:    "ssh",
				Args:    sshArgs,
				Stdout:  &raw,
				Inherit: true,
			}
			res, err := secretsRunner().Run(cmd.Context(), c)
			if err != nil || res.ExitCode != 0 {
				var msg string
				if err != nil {
					msg = fmt.Sprintf("cannot run ssh: %v", err)
				} else {
					msg = fmt.Sprintf("ssh failed (exit %d)", res.ExitCode)
				}
				return &output.Error{Code: "E_REMOTE_EXPORT", Message: msg, Hint: "check ssh access, sudo rights and that cvctl is installed on the host"}
			}
			// -t merges the remote stderr (prompts) and CRLF-translates stdout, so
			// the bundle travels armored and is recovered from the noise here.
			bin, err := secrets.Dearmor(raw.Bytes())
			if err != nil {
				return err
			}
			if err := secretsFS().WriteFile(out, bin, secretsFilePerm, sys.Owner{}); err != nil {
				return &output.Error{
					Code: "E_SECRETS_WRITE", Message: fmt.Sprintf("cannot write %s: %v", out, err),
					Hint: "choose a writable output path with -o",
				}
			}
			printerFrom(cmd).Result(secretsExportReport{Output: out}, func(w io.Writer) {
				fmt.Fprintf(w, "wrote %s\n", out)
			})
			return nil
		},
	}
	cmd.Flags().IntVar(&port, "port", 22, "ssh port")
	cmd.Flags().StringVar(&user, "user", "", "ssh user")
	cmd.Flags().StringVarP(&out, "output", "o", "", "output file (default cvctl-secrets-<host>-<date>.age)")
	cmd.Flags().StringVar(&instance, "instance", "", "export only secrets for this instance")
	cmd.Flags().BoolVar(&hostOnly, "host", false, "export only host secrets (host.env and /etc/cvctl/secrets/*)")
	return cmd
}
