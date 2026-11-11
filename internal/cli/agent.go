package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/agent"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

type agentReport struct {
	Decisions []agent.DecisionLog `json:"decisions"`
}

// runAgentRegistryLogin logs in to the registry the token is for. It returns
// nil when the login succeeded or there is no token to log in with.
var runAgentRegistryLogin = func(ctx context.Context, run sys.Runner, hcfg *hostcfg.Config, secrets *hostcfg.Secrets) error {
	if secrets == nil || hcfg == nil {
		return nil
	}
	token, ok := secrets.Get("registry_token")
	if !ok || token == "" {
		return nil
	}
	host := hcfg.TokenHost()
	if host == "" {
		return nil
	}
	fsys := sys.NewOSFS()
	err := registry.LoginWithFS(ctx, run, fsys, host, hcfg.RegistryUser, token)
	if err == nil {
		err = registry.PruneAuths(fsys, host)
	}
	if err != nil {
		return fmt.Errorf("registry login failed for %s: %v", host, err)
	}
	return nil
}

// agentLogin keeps the registry credentials of the last successful login. A
// failed login is retried on every tick until it succeeds, since deploys
// cannot pull without it; a retry that fails the same way is not notified again.
type agentLogin struct {
	run      sys.Runner
	done     [3]string
	ok       bool
	reported string
}

func (l *agentLogin) ensure(ctx context.Context, hcfg *hostcfg.Config, secrets *hostcfg.Secrets, notif notify.Notifier, out io.Writer) {
	want := registryCreds(hcfg, secrets)
	if l.ok && l.done == want {
		return
	}
	err := runAgentRegistryLogin(ctx, l.run, hcfg, secrets)
	if err == nil {
		l.done, l.ok, l.reported = want, true, ""
		return
	}
	l.ok = false
	reason := err.Error()
	_ = json.NewEncoder(out).Encode(agent.DecisionLog{
		TS:     time.Now().UTC().Format(time.RFC3339),
		Action: "error",
		Reason: reason,
	})
	if reason == l.reported {
		return
	}
	l.reported = reason
	hostName := hcfg.HostName
	if hostName == "" {
		hostName, _ = os.Hostname()
	}
	_ = notif.Send(ctx, notify.Message{
		Host:  hostName,
		Level: "warn",
		Title: "registry login failed",
		Text:  reason,
	})
}

// hostReloader re-reads host.env and the secrets for each agent tick and
// applies them to the deployer, so a change made by cvctl init reaches the next
// deploy without restarting the agent. The notifier is rebuilt only when what
// it reads changed; the registry login is redone when the credentials changed
// or the last attempt failed.
func hostReloader(ctx context.Context, fsys sys.FS, dir string, deployer *deploy.Deployer, login *agentLogin, notifOut, logOut io.Writer) func() (*hostcfg.Config, *hostcfg.Secrets, notify.Notifier, error) {
	return func() (*hostcfg.Config, *hostcfg.Secrets, notify.Notifier, error) {
		hcfg, secrets, err := hostcfg.Load(fsys, dir)
		if err != nil {
			return nil, nil, nil, err
		}
		if !hcfg.Equal(deployer.Host) || !secrets.Equal(deployer.Secrets) {
			deploy.Rewire(deployer, login.run, hcfg, secrets)
			deployer.Notify = notify.FromHostLogged(hcfg, secrets, notifOut)
		}
		login.ensure(ctx, deployer.Host, deployer.Secrets, deployer.Notify, logOut)
		return deployer.Host, deployer.Secrets, deployer.Notify, nil
	}
}

func registryCreds(hcfg *hostcfg.Config, secrets *hostcfg.Secrets) [3]string {
	var r [3]string
	if hcfg != nil {
		r[0], r[1] = hcfg.TokenHost(), hcfg.RegistryUser
	}
	if secrets != nil {
		r[2], _ = secrets.Get("registry_token")
	}
	return r
}

func newAgentCmd() *cobra.Command {
	var (
		once     bool
		interval time.Duration
	)

	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Run the continuous deploy agent loop",
		Example: `  cvctl agent
  cvctl agent --once
  cvctl agent --interval 30s --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			run := sys.NewExecRunner()
			fsys := sys.NewOSFS()

			hcfg, secrets, err := hostcfg.Load(fsys, hostCfgDir)
			if err != nil && !os.IsNotExist(err) {
				return err
			}

			cat, err := kinds.Load()
			if err != nil {
				return err
			}

			store := state.NewStore(fsys, "/var/lib/cvctl")
			deployer := &deploy.Deployer{
				DeployRoot: "/srv/deploy",
				TraefikDir: "/etc/traefik/dynamic",
				FS:         fsys,
				Store:      store,
				Host:       hcfg,
				Secrets:    secrets,
				Catalog:    cat,
			}
			deploy.Wire(deployer, run)

			notif := notify.FromHostLogged(hcfg, secrets, cmd.ErrOrStderr())
			deployer.Notify = notif

			ctx, stop := signal.NotifyContext(cmd.Context(), syscall.SIGTERM, syscall.SIGINT)
			defer stop()

			asJSON, _ := cmd.Flags().GetBool("json")

			var logOut io.Writer = cmd.OutOrStdout()
			var buf bytes.Buffer
			if asJSON && once {
				// We capture to buffer so we can emit single JSON document if json mode
				logOut = &buf
			}

			login := &agentLogin{run: run}
			login.ensure(ctx, hcfg, secrets, notif, logOut)

			cfg := agent.Config{
				Interval:   interval,
				Store:      store,
				Deployer:   deployer,
				Notify:     notif,
				Out:        logOut,
				Once:       once,
				Run:        run,
				Host:       hcfg,
				Secrets:    secrets,
				DeployRoot: "/srv/deploy",
				StateRoot:  "/var/lib/cvctl",
				FS:         fsys,
				Catalog:    cat,
				ReloadHost: hostReloader(ctx, fsys, hostCfgDir, deployer, login, cmd.ErrOrStderr(), logOut),
			}

			if err := agent.Run(ctx, cfg); err != nil {
				return err
			}

			if asJSON && once {
				var decisions []agent.DecisionLog
				sc := strings.Split(strings.TrimSpace(buf.String()), "\n")
				for _, line := range sc {
					line = strings.TrimSpace(line)
					if line == "" {
						continue
					}
					var dl agent.DecisionLog
					if err := json.Unmarshal([]byte(line), &dl); err == nil {
						decisions = append(decisions, dl)
					}
				}
				if decisions == nil {
					decisions = []agent.DecisionLog{}
				}
				printerFrom(cmd).Result(agentReport{Decisions: decisions}, nil)
			}

			return nil
		},
	}

	cmd.Flags().BoolVar(&once, "once", false, "run a single tick and exit")
	cmd.Flags().DurationVar(&interval, "interval", 60*time.Second, "interval between ticks")

	return cmd
}
