package cli

import (
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/adopt"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

var (
	adoptEUID = os.Geteuid
	adoptDial = net.DialTimeout
)

const adoptDialTimeout = 3 * time.Second

type adoptListReport struct {
	Adopted []adopt.Service `json:"adopted"`
}

func (e *OpsEnv) adoptDir() string {
	if e.AdoptDir != "" {
		return e.AdoptDir
	}
	return adopt.Dir
}

func (e *OpsEnv) syncAdopted() error {
	return adopt.Sync(e.fs(), e.adoptDir(), e.Deployer.TraefikDir, e.edgeMode(), e.Host.EdgeGateway())
}

func portShared(svcs []adopt.Service, s adopt.Service) bool {
	return slices.ContainsFunc(svcs, func(o adopt.Service) bool { return o.Name != s.Name && o.Port() == s.Port() })
}

func (e *OpsEnv) edgeMode() string {
	if e.Host != nil && e.Host.EdgeMode != "" {
		return e.Host.EdgeMode
	}
	return "edge"
}

// ufwAdopted opens or closes a port for Traefik's subnet only: the point of
// adopting is that nothing else reaches the service.
func (e *OpsEnv) ufwAdopted(cmd *cobra.Command, port int, verb ...string) error {
	if !e.Host.FirewallManage {
		return nil
	}
	which, err := e.runner().Run(cmd.Context(), sys.Cmd{Name: "sh", Args: []string{"-c", "command -v ufw"}})
	if err != nil || which.ExitCode != 0 {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: ufw not installed; firewall not managed for port %d\n", port)
		return nil
	}
	args := append(verb, "from", e.Host.EdgeSubnetOrDefault(), "to", "any", "port", strconv.Itoa(port), "proto", "tcp")
	res, err := e.runner().Run(cmd.Context(), sys.Cmd{Name: "ufw", Args: args})
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return &output.Error{Code: "E_FIREWALL_FAILED", Message: fmt.Sprintf("ufw failed for port %d: %s", port, res.Stderr),
			Hint: "check `ufw status`, or set CVCTL_FIREWALL_MANAGE=false"}
	}
	return nil
}

func (e *OpsEnv) adoptPreflight() error {
	if adoptEUID() != 0 {
		return &output.Error{Code: "E_ADOPT_ROOT", Message: "cvctl adopt needs root", Hint: "run it with sudo"}
	}
	if e.Host == nil {
		return &output.Error{Code: "E_ADOPT_NO_HOST", Message: "host is not configured", Hint: "run cvctl init"}
	}
	return nil
}

func (e *OpsEnv) adoptConflict(s adopt.Service, existing []adopt.Service) error {
	instances := e.allInstances()
	if slices.Contains(instances, s.Name) || slices.ContainsFunc(existing, func(o adopt.Service) bool { return o.Name == s.Name }) {
		return &output.Error{Code: "E_ADOPT_EXISTS", Message: fmt.Sprintf("%q already exists", s.Name), Hint: "choose another name"}
	}
	inUse := func(owner string) error {
		return &output.Error{Code: "E_ADOPT_DOMAIN_IN_USE", Message: fmt.Sprintf("domain %s is used by %s", s.Domain, owner),
			Hint: "free the domain or choose another one"}
	}
	for _, o := range existing {
		if o.Domain == s.Domain {
			return inUse(o.Name)
		}
	}
	for _, inst := range instances {
		it := e.projectListItem(inst)
		if it.Error != "" {
			return &output.Error{Code: "E_ADOPT_INSTANCE_UNREADABLE",
				Message: fmt.Sprintf("cannot read the domains of %s: %s", inst, it.Error),
				Hint:    "fix the instance first: its domain could clash with this one"}
		}
		for _, host := range it.Domains {
			if host == s.Domain {
				return inUse(inst)
			}
		}
	}
	return nil
}

func newAdoptCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "adopt",
		Short: "Expose services cvctl does not deploy through Traefik",
		Long: `Adopted services only get a route and TLS. cvctl does not start, update,
notify about or back up what it adopts.`,
		Example: `  cvctl adopt add panel --domain panel.example.com --target https://host:9090 --insecure-tls
  cvctl adopt list
  cvctl adopt rm panel`,
	}
	cmd.AddCommand(newAdoptAddCmd(), newAdoptListCmd(), newAdoptRmCmd())
	return cmd
}

func newAdoptAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "add <name>",
		Short:   "Adopt a service running on this host",
		Example: `  cvctl adopt add panel --domain panel.example.com --target https://host:9090 --insecure-tls`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			domain, _ := cmd.Flags().GetString("domain")
			target, _ := cmd.Flags().GetString("target")
			insecure, _ := cmd.Flags().GetBool("insecure-tls")
			force, _ := cmd.Flags().GetBool("force")

			e, err := newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			if err := e.adoptPreflight(); err != nil {
				return err
			}
			s := adopt.Service{Name: args[0], Domain: domain, Target: target, InsecureTLS: insecure}
			if err := s.Validate(); err != nil {
				return err
			}
			existing, err := adopt.Load(e.fs(), e.adoptDir())
			if err != nil {
				return err
			}
			if err := e.adoptConflict(s, existing); err != nil {
				return err
			}
			if !force {
				addr := net.JoinHostPort(e.Host.EdgeGateway(), strconv.Itoa(s.Port()))
				conn, err := adoptDial("tcp", addr, adoptDialTimeout)
				if err != nil {
					return &output.Error{Code: "E_ADOPT_UNREACHABLE", Message: fmt.Sprintf("cannot reach %s: %v", addr, err),
						Hint: "the process may listen on 127.0.0.1 only; bind it to all interfaces or to the gateway, or use --force if it is stopped"}
				}
				_ = conn.Close()
			}
			if err := adopt.Save(e.fs(), e.adoptDir(), s); err != nil {
				return err
			}
			if err := e.ufwAdopted(cmd, s.Port(), "allow"); err != nil {
				_ = adopt.Remove(e.fs(), e.adoptDir(), s.Name)
				return err
			}
			if err := e.syncAdopted(); err != nil {
				_ = adopt.Remove(e.fs(), e.adoptDir(), s.Name)
				if !portShared(existing, s) {
					_ = e.ufwAdopted(cmd, s.Port(), "delete", "allow")
				}
				return err
			}
			printerFrom(cmd).Result(s, func(w io.Writer) {
				fmt.Fprintf(w, "adopted %s: %s -> %s\n", s.Name, s.Domain, s.Target)
			})
			return nil
		},
	}
	cmd.Flags().String("domain", "", "public domain")
	cmd.Flags().String("target", "", "backend: http://host:<port> or https://host:<port>")
	cmd.Flags().Bool("insecure-tls", false, "do not verify the backend certificate")
	cmd.Flags().Bool("force", false, "adopt even if the target does not answer now")
	return cmd
}

func newAdoptListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List adopted services",
		Example: `  cvctl adopt list
  cvctl adopt list --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			e, err := newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			svcs, err := adopt.Load(e.fs(), e.adoptDir())
			if err != nil {
				return err
			}
			rep := adoptListReport{Adopted: append([]adopt.Service{}, svcs...)}
			printerFrom(cmd).Result(rep, func(w io.Writer) {
				tw := tabwriter.NewWriter(w, 0, 8, 2, ' ', 0)
				fmt.Fprintln(tw, "NAME\tDOMAIN\tTARGET")
				for _, s := range rep.Adopted {
					fmt.Fprintf(tw, "%s\t%s\t%s\n", s.Name, s.Domain, s.Target)
				}
				tw.Flush()
			})
			return nil
		},
	}
}

func newAdoptRmCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "rm <name>",
		Short:   "Stop exposing an adopted service; the service itself keeps running",
		Example: `  cvctl adopt rm panel`,
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			asJSON, _ := cmd.Flags().GetBool("json")
			e, err := newOpsEnv(opsOpts{NonInteractive: asJSON, In: cmd.InOrStdin(), Out: cmd.ErrOrStderr()})
			if err != nil {
				return err
			}
			if err := e.adoptPreflight(); err != nil {
				return err
			}
			svcs, err := adopt.Load(e.fs(), e.adoptDir())
			if err != nil {
				return err
			}
			i := slices.IndexFunc(svcs, func(s adopt.Service) bool { return s.Name == args[0] })
			if i < 0 {
				return &output.Error{Code: "E_ADOPT_NOT_FOUND", Message: fmt.Sprintf("%q is not adopted", args[0]),
					Hint: "check the name with cvctl adopt list"}
			}
			s := svcs[i]
			// The registry entry goes last: it is what lets a failed rm be retried.
			rest := slices.Delete(slices.Clone(svcs), i, i+1)
			if err := adopt.WriteRoutes(e.fs(), e.Deployer.TraefikDir, rest, e.edgeMode(), e.Host.EdgeGateway()); err != nil {
				return err
			}
			if !portShared(svcs, s) {
				if err := e.ufwAdopted(cmd, s.Port(), "delete", "allow"); err != nil {
					return err
				}
			}
			if err := adopt.Remove(e.fs(), e.adoptDir(), s.Name); err != nil {
				return err
			}
			printerFrom(cmd).Result(s, func(w io.Writer) { fmt.Fprintf(w, "removed %s\n", s.Name) })
			return nil
		},
	}
}
