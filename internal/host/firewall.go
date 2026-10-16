package host

import (
	"context"
	"fmt"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const firewallSkipMsg = "firewall not managed (CVCTL_FIREWALL_MANAGE=false)"

type FirewallStep struct{}

func (FirewallStep) Name() string { return "firewall" }

func desiredFirewallRules(edgeMode string) []string {
	if edgeMode == "edge" {
		return []string{"22/tcp", "80/tcp", "443/tcp"}
	}
	return []string{"22/tcp"}
}

func (FirewallStep) Check(ctx context.Context, e *Env) (Plan, error) {
	if e.Cfg == nil || !e.Cfg.FirewallManage {
		return Plan{Skip: firewallSkipMsg}, nil
	}

	c := sys.Cmd{Name: "ufw", Args: []string{"status", "verbose"}}
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 {
		return Plan{}, firewallCmdFailed(c, res)
	}

	rules := desiredFirewallRules(e.Cfg.EdgeMode)
	active, denyIncoming, allowOutgoing, presentRules := parseUfwStatus(res.Stdout, rules)

	var changes []string
	if !denyIncoming {
		changes = append(changes, "ufw default deny incoming")
	}
	if !allowOutgoing {
		changes = append(changes, "ufw default allow outgoing")
	}
	for _, r := range rules {
		if !presentRules[r] {
			changes = append(changes, "ufw allow "+r)
		}
	}
	if !active {
		changes = append(changes, "ufw --force enable")
	}

	return Plan{Changes: changes}, nil
}

func (FirewallStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	changeSet := make(map[string]bool, len(p.Changes))
	for _, c := range p.Changes {
		changeSet[c] = true
	}

	edgeMode := ""
	if e.Cfg != nil {
		edgeMode = e.Cfg.EdgeMode
	}

	// Allow rules (especially 22/tcp) must be added before enabling ufw so a default-deny policy never locks out SSH.
	if changeSet["ufw default deny incoming"] {
		if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"default", "deny", "incoming"}}); err != nil {
			return "", err
		}
	}
	if changeSet["ufw default allow outgoing"] {
		if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"default", "allow", "outgoing"}}); err != nil {
			return "", err
		}
	}
	for _, r := range desiredFirewallRules(edgeMode) {
		if changeSet["ufw allow "+r] {
			if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"allow", r}}); err != nil {
				return "", err
			}
		}
	}
	if changeSet["ufw --force enable"] {
		if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"--force", "enable"}}); err != nil {
			return "", err
		}
	}

	return "", nil
}

func parseUfwStatus(stdout []byte, rules []string) (active, denyIncoming, allowOutgoing bool, present map[string]bool) {
	present = make(map[string]bool, len(rules))
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if line == "Status: active" {
			active = true
			continue
		}
		if strings.HasPrefix(line, "Default:") {
			if strings.Contains(line, "deny (incoming)") {
				denyIncoming = true
			}
			if strings.Contains(line, "allow (outgoing)") {
				allowOutgoing = true
			}
			continue
		}
		for _, r := range rules {
			if strings.HasPrefix(line, r) && strings.Contains(line, "ALLOW IN") {
				present[r] = true
			}
		}
	}
	return active, denyIncoming, allowOutgoing, present
}

func runFirewallCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return firewallCmdFailed(c, res)
	}
	return nil
}

func firewallCmdFailed(c sys.Cmd, res sys.Result) error {
	stderr := strings.TrimSpace(string(res.Stderr))
	msg := fmt.Sprintf("%s failed (exit %d)", c.String(), res.ExitCode)
	if stderr != "" {
		msg += ": " + stderr
	}
	return &output.Error{
		Code:    "E_FIREWALL_FAILED",
		Message: msg,
		Hint:    fmt.Sprintf("check `%s` output and ufw status", c.String()),
	}
}
