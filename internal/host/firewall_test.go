package host

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func newFirewallTestEnv(manage bool, edgeMode string) (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	r.On("sshd -T -C user=root,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("port 22\n"),
		ExitCode: 0,
	})
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		FirewallManage: manage,
		EdgeMode:       edgeMode,
	}
	return &Env{Run: r, FS: fs, Cfg: cfg, EUID: 0}, r, fs
}

func TestFirewallRulesBeforeEnable(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "edge")
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte("Status: inactive\n"),
		ExitCode: 0,
	})
	runner.On("ufw default deny incoming", sys.Result{ExitCode: 0})
	runner.On("ufw default allow outgoing", sys.Result{ExitCode: 0})
	runner.On("ufw allow 22/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw allow 80/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw allow 443/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw --force enable", sys.Result{ExitCode: 0})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when ufw is inactive, got none")
	}

	callsBeforeApply := len(runner.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}
	if pending != "" {
		t.Fatalf("expected empty pending, got %q", pending)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"ufw default deny incoming",
		"ufw default allow outgoing",
		"ufw allow 22/tcp",
		"ufw allow 80/tcp",
		"ufw allow 443/tcp",
		"ufw --force enable",
	}
	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("Apply calls mismatch:\ngot:  %v\nwant: %v", got, wantCmds)
	}
	for i, want := range wantCmds {
		if applyCalls[i].String() != want {
			t.Fatalf("call %d: got %q, want %q", i, applyCalls[i].String(), want)
		}
	}

	allow22Idx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "ufw allow 22/tcp"
	})
	enableIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "ufw --force enable"
	})
	if allow22Idx == -1 || enableIdx == -1 || allow22Idx >= enableIdx {
		t.Fatalf("expected ufw allow 22/tcp (idx %d) before ufw --force enable (idx %d)", allow22Idx, enableIdx)
	}
}

func TestFirewallBehindProxyOnly22(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "behind-proxy")
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte("Status: inactive\n"),
		ExitCode: 0,
	})
	runner.On("ufw default deny incoming", sys.Result{ExitCode: 0})
	runner.On("ufw default allow outgoing", sys.Result{ExitCode: 0})
	runner.On("ufw allow 22/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw --force enable", sys.Result{ExitCode: 0})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes when ufw is inactive, got none")
	}

	callsBeforeApply := len(runner.Calls)
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"ufw default deny incoming",
		"ufw default allow outgoing",
		"ufw allow 22/tcp",
		"ufw --force enable",
	}
	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("Apply calls mismatch:\ngot:  %v\nwant: %v", got, wantCmds)
	}
	for _, c := range applyCalls {
		if c.String() == "ufw allow 80/tcp" || c.String() == "ufw allow 443/tcp" {
			t.Fatalf("unexpected edge port allowed in behind-proxy mode: %s", c.String())
		}
	}
}

func TestFirewallKeepsExistingRules(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "edge")
	statusOut := `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
8080/tcp                   ALLOW IN    Anywhere
`
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte(statusOut),
		ExitCode: 0,
	})
	runner.On("ufw allow 80/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw allow 443/tcp", sys.Result{ExitCode: 0})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes for missing 80/tcp and 443/tcp, got none")
	}

	callsBeforeApply := len(runner.Calls)
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	for _, c := range runner.Calls {
		s := c.String()
		if strings.Contains(s, "delete") || strings.Contains(s, "reset") {
			t.Fatalf("firewall must never delete or reset rules, saw %q", s)
		}
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"ufw allow 80/tcp",
		"ufw allow 443/tcp",
	}
	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("Apply calls mismatch:\ngot:  %v\nwant: %v", got, wantCmds)
	}
}

func TestFirewallSkip(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(false, "edge")

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	wantSkip := "firewall not managed (CVCTL_FIREWALL_MANAGE=false)"
	if plan.Skip != wantSkip {
		t.Fatalf("got Skip %q, want %q", plan.Skip, wantSkip)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when skipped, got %v", plan.Changes)
	}
	if len(runner.Calls) != 0 {
		t.Fatalf("expected no commands when skipped, got %v", runner.Calls)
	}
}

func TestFirewallUnchanged(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "edge")
	statusOut := `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
22/tcp                     ALLOW IN    Anywhere
80/tcp                     ALLOW IN    Anywhere
443/tcp                    ALLOW IN    Anywhere
`
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte(statusOut),
		ExitCode: 0,
	})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when firewall is already in desired state, got %v", plan.Changes)
	}
}

func TestFirewallCommandFailure(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "behind-proxy")
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte("Status: inactive\n"),
		ExitCode: 0,
	})
	runner.On("ufw default deny incoming", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("ERROR: problem running ufw-init\n"),
	})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_FIREWALL_FAILED error, got nil")
	}
	if got := code(t, err); got != "E_FIREWALL_FAILED" {
		t.Fatalf("got code %q, want E_FIREWALL_FAILED", got)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if !strings.Contains(oe.Message, "ERROR: problem running ufw-init") {
		t.Fatalf("expected stderr in error message, got %q", oe.Message)
	}
	if !strings.Contains(oe.Hint, "ufw default deny incoming") {
		t.Fatalf("expected command in hint, got %q", oe.Hint)
	}
}

func TestFirewallCustomSSHPort2228(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "edge")
	runner.On("sshd -T -C user=root,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("passwordauthentication no\nport 2228\npermitrootlogin no\n"),
		ExitCode: 0,
	})
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte("Status: inactive\n"),
		ExitCode: 0,
	})
	runner.On("ufw default deny incoming", sys.Result{ExitCode: 0})
	runner.On("ufw default allow outgoing", sys.Result{ExitCode: 0})
	runner.On("ufw allow 2228/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw allow 80/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw allow 443/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw --force enable", sys.Result{ExitCode: 0})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	callsBeforeApply := len(runner.Calls)
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	for _, c := range runner.Calls {
		if c.String() == "ufw allow 22/tcp" {
			t.Fatalf("must not allow hardcoded 22/tcp when sshd port is 2228, calls: %v", runner.Calls)
		}
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"ufw default deny incoming",
		"ufw default allow outgoing",
		"ufw allow 2228/tcp",
		"ufw allow 80/tcp",
		"ufw allow 443/tcp",
		"ufw --force enable",
	}
	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("Apply calls mismatch:\ngot:  %v\nwant: %v", got, wantCmds)
	}
	for i, want := range wantCmds {
		if applyCalls[i].String() != want {
			t.Fatalf("call %d: got %q, want %q", i, applyCalls[i].String(), want)
		}
	}

	allowIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "ufw allow 2228/tcp"
	})
	enableIdx := slices.IndexFunc(runner.Calls, func(c sys.Cmd) bool {
		return c.String() == "ufw --force enable"
	})
	if allowIdx == -1 || enableIdx == -1 || allowIdx >= enableIdx {
		t.Fatalf("expected ufw allow 2228/tcp (idx %d) before ufw --force enable (idx %d)", allowIdx, enableIdx)
	}
}

func TestFirewallMultipleSSHPorts(t *testing.T) {
	env, runner, _ := newFirewallTestEnv(true, "behind-proxy")
	runner.On("sshd -T -C user=root,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout:   []byte("port 22\nport 2228\n"),
		ExitCode: 0,
	})
	runner.On("ufw status verbose", sys.Result{
		Stdout:   []byte("Status: inactive\n"),
		ExitCode: 0,
	})
	runner.On("ufw default deny incoming", sys.Result{ExitCode: 0})
	runner.On("ufw default allow outgoing", sys.Result{ExitCode: 0})
	runner.On("ufw allow 22/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw allow 2228/tcp", sys.Result{ExitCode: 0})
	runner.On("ufw --force enable", sys.Result{ExitCode: 0})

	step := FirewallStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}

	callsBeforeApply := len(runner.Calls)
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("unexpected Apply error: %v", err)
	}

	applyCalls := runner.Calls[callsBeforeApply:]
	wantCmds := []string{
		"ufw default deny incoming",
		"ufw default allow outgoing",
		"ufw allow 22/tcp",
		"ufw allow 2228/tcp",
		"ufw --force enable",
	}
	if len(applyCalls) != len(wantCmds) {
		var got []string
		for _, c := range applyCalls {
			got = append(got, c.String())
		}
		t.Fatalf("Apply calls mismatch:\ngot:  %v\nwant: %v", got, wantCmds)
	}
	for i, want := range wantCmds {
		if applyCalls[i].String() != want {
			t.Fatalf("call %d: got %q, want %q", i, applyCalls[i].String(), want)
		}
	}
}

func TestFirewallSSHDTFailureRefusesWithoutUfwCalls(t *testing.T) {
	cases := []struct {
		name string
		res  sys.Result
	}{
		{
			name: "non-zero exit",
			res:  sys.Result{ExitCode: 1, Stderr: []byte("/etc/ssh/sshd_config: bad option\n")},
		},
		{
			name: "no port reported",
			res:  sys.Result{ExitCode: 0, Stdout: []byte("passwordauthentication no\n")},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, runner, _ := newFirewallTestEnv(true, "edge")
			runner.On("sshd -T -C user=root,host=localhost,addr=127.0.0.1", tc.res)

			step := FirewallStep{}
			_, err := step.Check(context.Background(), env)
			if err == nil {
				t.Fatal("expected E_FIREWALL_FAILED error, got nil")
			}
			var oe *output.Error
			if !errors.As(err, &oe) {
				t.Fatalf("expected *output.Error, got %T (%v)", err, err)
			}
			if oe.Code != "E_FIREWALL_FAILED" {
				t.Fatalf("got code %q, want E_FIREWALL_FAILED", oe.Code)
			}
			wantMsg := "cannot determine the ssh port; refusing to enable the firewall"
			if oe.Message != wantMsg {
				t.Fatalf("got message %q, want %q", oe.Message, wantMsg)
			}
			wantHint := "check `sshd -T` output, or set CVCTL_FIREWALL_MANAGE=false"
			if oe.Hint != wantHint {
				t.Fatalf("got hint %q, want %q", oe.Hint, wantHint)
			}
			for _, c := range runner.Calls {
				if c.Name == "ufw" {
					t.Fatalf("expected no ufw calls when sshd -T fails, saw %q", c.String())
				}
			}
		})
	}
}

func TestFirewallUfwStatusParserTable(t *testing.T) {
	activeHeader := `Status: active
Logging: on (low)
Default: deny (incoming), allow (outgoing), disabled (routed)
New profiles: skip

To                         Action      From
--                         ------      ----
`
	cases := []struct {
		name        string
		sshPort     string
		ufwStdout   string
		wantChanges []string
	}{
		{
			name:        "22/tcp ALLOW IN Anywhere is present",
			sshPort:     "22",
			ufwStdout:   activeHeader + "22/tcp                     ALLOW IN    Anywhere\n",
			wantChanges: nil,
		},
		{
			name:        "22/tcp (v6) ALLOW IN Anywhere (v6) alone does not count",
			sshPort:     "22",
			ufwStdout:   activeHeader + "22/tcp (v6)                ALLOW IN    Anywhere (v6)\n",
			wantChanges: []string{"ufw allow 22/tcp"},
		},
		{
			name:        "OpenSSH ALLOW IN Anywhere counts when port is 22",
			sshPort:     "22",
			ufwStdout:   activeHeader + "OpenSSH                    ALLOW IN    Anywhere\n",
			wantChanges: nil,
		},
		{
			name:        "OpenSSH ALLOW IN Anywhere does not count when port is 2228",
			sshPort:     "2228",
			ufwStdout:   activeHeader + "OpenSSH                    ALLOW IN    Anywhere\n",
			wantChanges: []string{"ufw allow 2228/tcp"},
		},
		{
			name:        "22 ALLOW IN Anywhere counts",
			sshPort:     "22",
			ufwStdout:   activeHeader + "22                         ALLOW IN    Anywhere\n",
			wantChanges: nil,
		},
		{
			name:        "22/tcp LIMIT IN Anywhere counts",
			sshPort:     "22",
			ufwStdout:   activeHeader + "22/tcp                     LIMIT IN    Anywhere\n",
			wantChanges: nil,
		},
		{
			name:        "22/tcp ALLOW IN 10.0.0.0/8 restricted source does not count",
			sshPort:     "22",
			ufwStdout:   activeHeader + "22/tcp                     ALLOW IN    10.0.0.0/8\n",
			wantChanges: []string{"ufw allow 22/tcp"},
		},
		{
			name:      "Status: inactive plans defaults, allow and enable",
			sshPort:   "22",
			ufwStdout: "Status: inactive\n",
			wantChanges: []string{
				"ufw default deny incoming",
				"ufw default allow outgoing",
				"ufw allow 22/tcp",
				"ufw --force enable",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, runner, _ := newFirewallTestEnv(true, "behind-proxy")
			runner.On("sshd -T -C user=root,host=localhost,addr=127.0.0.1", sys.Result{
				Stdout:   []byte("port " + tc.sshPort + "\n"),
				ExitCode: 0,
			})
			runner.On("ufw status verbose", sys.Result{
				Stdout:   []byte(tc.ufwStdout),
				ExitCode: 0,
			})

			step := FirewallStep{}
			plan, err := step.Check(context.Background(), env)
			if err != nil {
				t.Fatalf("unexpected Check error: %v", err)
			}
			if !slices.Equal(plan.Changes, tc.wantChanges) {
				t.Fatalf("Changes mismatch:\ngot:  %v\nwant: %v", plan.Changes, tc.wantChanges)
			}
		})
	}
}
