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

var runAgentRegistryLogin = func(ctx context.Context, run sys.Runner, hcfg *hostcfg.Config, secrets *hostcfg.Secrets, notif notify.Notifier, out io.Writer) {
	if secrets == nil {
		return
	}
	token, ok := secrets.Get("registry_token")
	if !ok || token == "" {
		return
	}
	if hcfg == nil || hcfg.Registries == "" {
		return
	}

	seen := make(map[string]bool)
	for _, r := range strings.Split(hcfg.Registries, ",") {
		s := strings.TrimSpace(r)
		if s == "" {
			continue
		}
		host := strings.Split(s, "/")[0]
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true

		if err := registry.LoginWithFS(ctx, run, sys.NewOSFS(), host, hcfg.RegistryUser, token); err != nil {
			errReason := fmt.Sprintf("registry login failed for %s: %v", host, err)
			logEntry := agent.DecisionLog{
				TS:       time.Now().UTC().Format(time.RFC3339),
				Instance: "",
				Action:   "error",
				Reason:   errReason,
			}
			_ = json.NewEncoder(out).Encode(logEntry)

			hostName := hcfg.HostName
			if hostName == "" {
				hostName, _ = os.Hostname()
			}
			_ = notif.Send(ctx, notify.Message{
				Host:  hostName,
				Level: "warn",
				Title: "registry login failed",
				Text:  errReason,
			})
		}
	}
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
				Catalog:    cat,
			}
			deploy.Wire(deployer, run)

			notif, _ := notify.FromHost(hcfg, secrets)
			if notif == nil {
				notif = notify.NoopNotifier{}
			}
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

			runAgentRegistryLogin(ctx, run, hcfg, secrets, notif, logOut)

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
