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

func capsEnv(rel string) (*Env, *systest.Runner, *systest.FS) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	seedOS(fs, rel)
	return &Env{Run: r, FS: fs, EUID: 0, Cfg: &hostcfg.Config{FirewallManage: true, EdgeMode: "edge"}}, r, fs
}

func dockerPresent(r *systest.Runner) {
	r.On("docker info", sys.Result{})
	r.On("docker compose version --short", sys.Result{Stdout: []byte("2.29.1\n")})
}

func TestPackagesNoAptHostsWithDockerPlanNothing(t *testing.T) {
	for name, rel := range map[string]string{"arch": arch, "fedora": fedora, "unknown": ""} {
		env, r, _ := capsEnv(rel)
		dockerPresent(r)
		plan, err := PackagesStep{}.Check(context.Background(), env)
		if err != nil || len(plan.Changes) != 0 {
			t.Fatalf("%s: plan=%+v err=%v", name, plan, err)
		}
		for _, c := range r.Calls {
			if c.Name == "dpkg-query" || c.Name == "apt-get" {
				t.Fatalf("%s: unexpected %s", name, c)
			}
		}
	}
}

func TestPackagesDockerMissingWithoutRepo(t *testing.T) {
	env, _, _ := capsEnv(arch)
	_, err := PackagesStep{}.Check(context.Background(), env)
	if c := code(t, err); c != "E_DOCKER_MISSING" {
		t.Fatal(c)
	}
}

func TestPackagesDebianUsesDebianRepo(t *testing.T) {
	env, r, fs := capsEnv(debian12)
	markAllInstalled(r)
	r.On("install -m 0755 -d /etc/apt/keyrings", sys.Result{})
	r.On("curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc", sys.Result{})
	r.On("dpkg --print-architecture", sys.Result{Stdout: []byte("amd64\n")})
	r.On("curl -fsSI https://download.docker.com/linux/debian/dists/bookworm/Release", sys.Result{})
	r.On("apt-get update", sys.Result{})
	r.On("apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin", sys.Result{})
	plan, err := PackagesStep{}.Check(context.Background(), env)
	if err != nil || len(plan.Changes) == 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	if _, err := (PackagesStep{}).Apply(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	b, _ := fs.ReadFile("/etc/apt/sources.list.d/docker.list")
	want := "deb [arch=amd64 signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian bookworm stable\n"
	if string(b) != want {
		t.Fatalf("got %q", b)
	}
}

func TestPackagesMintUsesUbuntuCodename(t *testing.T) {
	env, r, fs := capsEnv(mint)
	markAllInstalled(r)
	r.On("install -m 0755 -d /etc/apt/keyrings", sys.Result{})
	r.On("curl -fsSL https://download.docker.com/linux/ubuntu/gpg -o /etc/apt/keyrings/docker.asc", sys.Result{})
	r.On("dpkg --print-architecture", sys.Result{Stdout: []byte("amd64\n")})
	r.On("curl -fsSI https://download.docker.com/linux/ubuntu/dists/noble/Release", sys.Result{})
	r.On("apt-get update", sys.Result{})
	r.On("apt-get install -y docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin", sys.Result{})
	plan, err := PackagesStep{}.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (PackagesStep{}).Apply(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	b, _ := fs.ReadFile("/etc/apt/sources.list.d/docker.list")
	if !strings.Contains(string(b), "linux/ubuntu noble stable") {
		t.Fatalf("got %q", b)
	}
}

func TestPackagesUbuntu2204Accepted(t *testing.T) {
	env, r, _ := capsEnv("ID=ubuntu\nVERSION_ID=\"22.04\"\nVERSION_CODENAME=jammy\n")
	markAllInstalled(r)
	plan, err := PackagesStep{}.Check(context.Background(), env)
	if err != nil || len(plan.Changes) == 0 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestPackagesAptHostInstallsToolPackages(t *testing.T) {
	for _, rel := range []string{debian12, mint, ubuntu2404} {
		env, r, _ := capsEnv(rel)
		dockerPresent(r)
		plan, err := PackagesStep{}.Check(context.Background(), env)
		if err != nil || len(plan.Changes) != 4 {
			t.Fatalf("%q: plan=%+v err=%v", rel, plan, err)
		}
	}
}

func TestToolStepsSkipOnNonAptWithoutTools(t *testing.T) {
	for name, rel := range map[string]string{"arch": arch, "fedora": fedora} {
		env, _, _ := capsEnv(rel)
		for _, s := range []Step{Fail2banStep{}, FirewallStep{}, UnattendedStep{}} {
			plan, err := s.Check(context.Background(), env)
			if err != nil || plan.Skip == "" || len(plan.Changes) != 0 {
				t.Fatalf("%s/%s: plan=%+v err=%v", name, s.Name(), plan, err)
			}
		}
	}
	env, _, _ := capsEnv(fedora)
	plan, _ := FirewallStep{}.Check(context.Background(), env)
	if !strings.Contains(plan.Skip, "ufw not installed") {
		t.Fatalf("skip %q", plan.Skip)
	}
	plan, _ = UnattendedStep{}.Check(context.Background(), env)
	if plan.Skip != "automatic updates not managed on fedora" {
		t.Fatalf("skip %q", plan.Skip)
	}
}

func TestToolStepsRunOnNonAptWhenToolPresent(t *testing.T) {
	env, r, _ := capsEnv(arch)
	r.On("sh -c command -v fail2ban-client", sys.Result{Stdout: []byte("/usr/bin/fail2ban-client\n")})
	r.On("sh -c command -v ufw", sys.Result{Stdout: []byte("/usr/sbin/ufw\n")})
	r.On("sshd -T -C user=root,host=localhost,addr=127.0.0.1", sys.Result{Stdout: []byte("port 22\n")})
	r.On("ufw status verbose", sys.Result{Stdout: []byte("Status: inactive\n")})
	for _, s := range []Step{Fail2banStep{}, FirewallStep{}} {
		plan, err := s.Check(context.Background(), env)
		if err != nil || plan.Skip != "" || len(plan.Changes) == 0 {
			t.Fatalf("%s: plan=%+v err=%v", s.Name(), plan, err)
		}
	}
}

func TestSSHUnitSelection(t *testing.T) {
	for _, tc := range []struct {
		catExit int
		want    string
	}{{0, "ssh"}, {1, "sshd"}} {
		env, r, _ := capsEnv(ubuntu2404)
		r.On("systemctl cat ssh.service", sys.Result{ExitCode: tc.catExit})
		if got, err := sshUnit(context.Background(), env); err != nil || got != tc.want {
			t.Fatalf("exit %d: got %q want %q", tc.catExit, got, tc.want)
		}
	}
}

func TestSSHApplyUsesSshdUnit(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	runner.On("systemctl cat ssh.service", sys.Result{ExitCode: 1})
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout: []byte("passwordauthentication yes\npermitrootlogin yes\n"),
	})
	runner.On("sshd -t", sys.Result{})
	runner.On("systemd-run --unit cvctl-ssh-revert --on-active=300 /bin/sh -c rm -f /etc/ssh/sshd_config.d/00-cvctl.conf && systemctl try-reload-or-restart sshd", sys.Result{})
	runner.On("systemctl try-reload-or-restart sshd", sys.Result{})

	plan, err := SSHStep{}.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout: []byte("passwordauthentication no\npermitrootlogin no\n"),
	})
	if _, err := (SSHStep{}).Apply(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(runner.Calls, func(c sys.Cmd) bool { return c.String() == "systemctl try-reload-or-restart sshd" }) {
		t.Fatalf("sshd reload not called: %v", runner.Calls)
	}
	for _, c := range runner.Calls {
		if c.String() == "systemctl try-reload-or-restart ssh" {
			t.Fatal("ssh unit used on an sshd host")
		}
	}
}

func TestSSHUnitRunnerErrorAbortsBeforeTimer(t *testing.T) {
	env, runner, fs := newSSHTestEnv("andres", true, false)
	boom := errors.New("systemctl exploded")
	runner.OnError("systemctl cat ssh.service", boom)
	seedAdminWithKey(runner, fs, "andres", "/home/andres", "1000", "andres", testEd25519Key)
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout: []byte("passwordauthentication yes\npermitrootlogin yes\n"),
	})
	runner.On("sshd -t", sys.Result{})
	// Check also reads the unit? It must not; only Apply does.
	plan, err := SSHStep{}.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	runner.On("sshd -T -C user=andres,host=localhost,addr=127.0.0.1", sys.Result{
		Stdout: []byte("passwordauthentication no\npermitrootlogin no\n"),
	})
	_, err = SSHStep{}.Apply(context.Background(), env, plan)
	if !errors.Is(err, boom) {
		t.Fatalf("got %v", err)
	}
	if _, serr := fs.Stat(sshDropInPath); serr == nil {
		t.Fatal("drop-in not removed")
	}
	for _, c := range runner.Calls {
		if c.Name == "systemd-run" {
			t.Fatal("revert timer armed")
		}
	}
}

func TestMalformedOSReleaseErrorCode(t *testing.T) {
	env, r, _ := capsEnv("ID=\"unterminated\n")
	dockerPresent(r)
	for _, s := range []Step{PackagesStep{}, Fail2banStep{}, FirewallStep{}, UnattendedStep{}} {
		_, err := s.Check(context.Background(), env)
		if c := code(t, err); c != "E_HOST_OS_RELEASE" {
			t.Fatalf("%s: %s", s.Name(), c)
		}
	}
}

func TestComposeOldHint(t *testing.T) {
	for rel, want := range map[string]string{
		ubuntu2404: "apt-get install docker-compose-v2",
		arch:       "with your distribution's packages",
	} {
		env, r, _ := capsEnv(rel)
		r.On("docker info", sys.Result{})
		r.On("docker compose version --short", sys.Result{Stdout: []byte("2.20.0\n")})
		_, err := PackagesStep{}.Check(context.Background(), env)
		if c := code(t, err); c != "E_DOCKER_COMPOSE_OLD" {
			t.Fatal(c)
		}
		var oe *output.Error
		errors.As(err, &oe)
		if !strings.Contains(oe.Hint, want) {
			t.Fatalf("%s: hint %q", rel, oe.Hint)
		}
	}
}
