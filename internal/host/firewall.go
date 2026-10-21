package host

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const firewallSkipMsg = "firewall not managed (CVCTL_FIREWALL_MANAGE=false)"

type FirewallStep struct{}

func (FirewallStep) Name() string { return "firewall" }

func desiredFirewallRules(sshPorts []string, edgeMode string) []string {
	var rules []string
	seen := make(map[string]bool)
	add := func(r string) {
		if !seen[r] {
			seen[r] = true
			rules = append(rules, r)
		}
	}
	for _, p := range sshPorts {
		add(p + "/tcp")
	}
	if edgeMode == "edge" {
		add("80/tcp")
		add("443/tcp")
	}
	return rules
}

func (FirewallStep) Check(ctx context.Context, e *Env) (Plan, error) {
	if e.Cfg == nil || !e.Cfg.FirewallManage {
		return Plan{Skip: firewallSkipMsg}, nil
	}

	osi, err := hostOS(e.FS)
	if err != nil {
		return Plan{}, firewallFSErr(err)
	}
	if !osi.apt() && !hasCommand(ctx, e, "ufw") {
		return Plan{Skip: "ufw not installed; firewall not managed: allow ssh, 80 and 443 yourself"}, nil
	}

	sshdCmd := sys.Cmd{
		Name: "sshd",
		Args: []string{"-T", "-C", "user=root,host=localhost,addr=127.0.0.1"},
	}
	sshdRes, err := e.Run.Run(ctx, sshdCmd)
	if err != nil {
		return Plan{}, &output.Error{
			Code:    "E_FIREWALL_FAILED",
			Message: "cannot determine the ssh port; refusing to enable the firewall",
			Hint:    "check `sshd -T` output, or set CVCTL_FIREWALL_MANAGE=false",
		}
	}
	sshPorts := parseSSHPorts(sshdRes.Stdout)
	if sshdRes.ExitCode != 0 || len(sshPorts) == 0 {
		return Plan{}, &output.Error{
			Code:    "E_FIREWALL_FAILED",
			Message: "cannot determine the ssh port; refusing to enable the firewall",
			Hint:    "check `sshd -T` output, or set CVCTL_FIREWALL_MANAGE=false",
		}
	}

	rules := desiredFirewallRules(sshPorts, e.Cfg.EdgeMode)

	c := sys.Cmd{Name: "ufw", Args: []string{"status", "verbose"}}
	res, err := e.Run.Run(ctx, c)
	var active, denyIncoming, allowOutgoing bool
	var presentRules map[string]bool
	if err != nil {
		// ufw cannot start (e.g. not installed yet on a fresh host); treat as inactive and needs all rules.
		presentRules = make(map[string]bool)
	} else {
		if res.ExitCode != 0 {
			return Plan{}, firewallCmdFailed(c, res)
		}
		active, denyIncoming, allowOutgoing, presentRules = parseUfwStatus(res.Stdout, rules, sshPorts)
	}

	var changes []string
	var actions []Action
	if !denyIncoming {
		changes = append(changes, "ufw default deny incoming")
	}
	if !allowOutgoing {
		changes = append(changes, "ufw default allow outgoing")
	}
	for _, r := range rules {
		if !presentRules[r] {
			changes = append(changes, "ufw allow "+r)
			actions = append(actions, Action{Kind: "allow", Target: r})
		}
	}
	if !denyIncoming {
		actions = append(actions, Action{Kind: "default", Target: "deny", Arg: "incoming"})
	}
	if !allowOutgoing {
		actions = append(actions, Action{Kind: "default", Target: "allow", Arg: "outgoing"})
	}
	if !active {
		changes = append(changes, "ufw --force enable")
		actions = append(actions, Action{Kind: "enable", Target: "ufw"})
	}

	return Plan{Changes: changes, Actions: actions}, nil
}

func (FirewallStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	for _, a := range p.Actions {
		switch a.Kind {
		case "allow":
			if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"allow", a.Target}}); err != nil {
				return "", err
			}
		case "default":
			if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"default", a.Target, a.Arg}}); err != nil {
				return "", err
			}
		case "enable":
			if err := runFirewallCmd(ctx, e, sys.Cmd{Name: "ufw", Args: []string{"--force", "enable"}}); err != nil {
				return "", err
			}
		}
	}

	return "", nil
}

func parseSSHPorts(stdout []byte) []string {
	var ports []string
	seen := make(map[string]bool)
	for _, line := range strings.Split(string(stdout), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.ToLower(fields[0]) == "port" {
			p := fields[1]
			if n, err := strconv.Atoi(p); err == nil && n > 0 && n <= 65535 && !seen[p] {
				seen[p] = true
				ports = append(ports, p)
			}
		}
	}
	return ports
}

func parseUfwStatus(stdout []byte, rules []string, sshPorts []string) (active, denyIncoming, allowOutgoing bool, present map[string]bool) {
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
		target, action, source, ok := parseUfwRuleLine(line)
		if !ok {
			continue
		}
		if strings.HasSuffix(target, "(v6)") {
			continue
		}
		if action != "ALLOW IN" && action != "LIMIT IN" {
			continue
		}
		for _, r := range rules {
			port := strings.TrimSuffix(r, "/tcp")
			matchesTarget := target == port || target == port+"/tcp" || (port == "22" && target == "OpenSSH")
			if !matchesTarget {
				continue
			}
			isSSH := slices.Contains(sshPorts, port)
			if isSSH || source == "Anywhere" {
				present[r] = true
			}
		}
	}
	return active, denyIncoming, allowOutgoing, present
}

func parseUfwRuleLine(line string) (target, action, source string, ok bool) {
	if idx := strings.IndexByte(line, '#'); idx >= 0 {
		line = line[:idx]
	}
	fields := strings.Fields(line)
	for i := 1; i+1 < len(fields); i++ {
		if (fields[i] == "ALLOW" || fields[i] == "LIMIT") && fields[i+1] == "IN" {
			target = strings.Join(fields[:i], " ")
			action = fields[i] + " " + fields[i+1]
			source = strings.Join(fields[i+2:], " ")
			return target, action, source, source != ""
		}
	}
	return "", "", "", false
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
	return sys.CmdErr("E_FIREWALL_FAILED", c, res, nil, fmt.Sprintf("check `%s` output and ufw status", c.String()))
}

func firewallFSErr(err error) error {
	return &output.Error{
		Code:    "E_FIREWALL_FAILED",
		Message: fmt.Sprintf("cannot read /etc/os-release: %v", err),
		Hint:    "check permissions of /etc/os-release, or set CVCTL_FIREWALL_MANAGE=false",
	}
}
