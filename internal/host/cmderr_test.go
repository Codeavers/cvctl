package host

import (
	"errors"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
)

func TestCmdErrFormat(t *testing.T) {
	t.Run("exit non-zero with stderr", func(t *testing.T) {
		cmd := sys.Cmd{Name: "systemctl", Args: []string{"restart", "docker"}}
		res := sys.Result{ExitCode: 1, Stderr: []byte("failed to start unit\n")}
		err := CmdErr("E_DOCKER_RESTART_FAILED", cmd, res, nil, "check systemctl status docker")
		if err.Code != "E_DOCKER_RESTART_FAILED" {
			t.Errorf("got code %q, want E_DOCKER_RESTART_FAILED", err.Code)
		}
		wantMsg := "systemctl restart docker failed (exit 1): failed to start unit"
		if err.Message != wantMsg {
			t.Errorf("got message %q, want %q", err.Message, wantMsg)
		}
		if err.Hint != "check systemctl status docker" {
			t.Errorf("got hint %q, want 'check systemctl status docker'", err.Hint)
		}
	})

	t.Run("exit non-zero without stderr", func(t *testing.T) {
		cmd := sys.Cmd{Name: "docker", Args: []string{"ps", "-q"}}
		res := sys.Result{ExitCode: 2}
		err := CmdErr("E_DOCKER_PS_FAILED", cmd, res, nil, "systemctl status docker")
		if err.Code != "E_DOCKER_PS_FAILED" {
			t.Errorf("got code %q, want E_DOCKER_PS_FAILED", err.Code)
		}
		wantMsg := "docker ps -q failed (exit 2)"
		if err.Message != wantMsg {
			t.Errorf("got message %q, want %q", err.Message, wantMsg)
		}
		if err.Hint != "systemctl status docker" {
			t.Errorf("got hint %q, want 'systemctl status docker'", err.Hint)
		}
	})

	t.Run("exit non-zero with empty stderr and non-empty stdout", func(t *testing.T) {
		cmd := sys.Cmd{Name: "sshd", Args: []string{"-t"}}
		res := sys.Result{ExitCode: 1, Stdout: []byte("configuration test failed\n")}
		err := CmdErr("E_SSH_CONFIG", cmd, res, nil, "fix sshd_config")
		if err.Code != "E_SSH_CONFIG" {
			t.Errorf("got code %q, want E_SSH_CONFIG", err.Code)
		}
		wantMsg := "sshd -t failed (exit 1): configuration test failed"
		if err.Message != wantMsg {
			t.Errorf("got message %q, want %q", err.Message, wantMsg)
		}
		if err.Hint != "fix sshd_config" {
			t.Errorf("got hint %q, want 'fix sshd_config'", err.Hint)
		}
	})

	t.Run("failed to start runErr", func(t *testing.T) {
		cmd := sys.Cmd{Name: "ufw", Args: []string{"status"}}
		runErr := errors.New("exec: executable file not found in $PATH")
		err := CmdErr("E_FIREWALL_FAILED", cmd, sys.Result{}, runErr, "install ufw")
		if err.Code != "E_FIREWALL_FAILED" {
			t.Errorf("got code %q, want E_FIREWALL_FAILED", err.Code)
		}
		wantMsg := "ufw status: exec: executable file not found in $PATH"
		if err.Message != wantMsg {
			t.Errorf("got message %q, want %q", err.Message, wantMsg)
		}
		if err.Hint != "install ufw" {
			t.Errorf("got hint %q, want 'install ufw'", err.Hint)
		}
	})

	t.Run("command with env vars not in message", func(t *testing.T) {
		cmd := sys.Cmd{
			Name: "apt-get",
			Args: []string{"update"},
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}
		res := sys.Result{ExitCode: 100, Stderr: []byte("  repository not found\n")}
		err := CmdErr("E_APT_FAILED", cmd, res, nil, "check apt sources")
		wantMsg := "apt-get update failed (exit 100): repository not found"
		if err.Message != wantMsg {
			t.Errorf("got message %q, want %q", err.Message, wantMsg)
		}
	})
}
