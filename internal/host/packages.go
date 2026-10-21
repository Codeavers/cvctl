package host

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

var defaultOtherPackages = []string{"fail2ban", "ufw", "unattended-upgrades", "curl"}

type PackagesStep struct{}

func (PackagesStep) Name() string { return "packages" }

func parseSemver(s string) (major, minor, patch int, ok bool) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "v")
	if idx := strings.IndexAny(s, "-+"); idx != -1 {
		s = s[:idx]
	}
	parts := strings.Split(s, ".")
	if len(parts) < 2 {
		return 0, 0, 0, false
	}
	var err error
	major, err = strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, 0, false
	}
	if len(parts) >= 3 {
		patch, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, 0, 0, false
		}
	}
	return major, minor, patch, true
}

func isComposeSatisfied(versionStr string) bool {
	maj, min, patch, ok := parseSemver(versionStr)
	if !ok {
		return false
	}
	if maj > 2 {
		return true
	}
	if maj == 2 {
		if min > 24 {
			return true
		}
		if min == 24 && patch >= 0 {
			return true
		}
	}
	return false
}

func checkPackageInstalled(ctx context.Context, e *Env, pkg string) bool {
	res, err := e.Run.Run(ctx, sys.Cmd{
		Name: "dpkg-query",
		Args: []string{"-W", "-f=${Status}", pkg},
	})
	if err != nil || res.ExitCode != 0 {
		return false
	}
	return strings.TrimSpace(string(res.Stdout)) == "install ok installed"
}

func (PackagesStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var changes []string
	var actions []Action

	resInfo, errInfo := e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"info"}, Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}})
	if errInfo == nil && resInfo.ExitCode == 0 {
		resComp, errComp := e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"compose", "version", "--short"}, Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}})
		if errComp == nil && resComp.ExitCode == 0 && isComposeSatisfied(string(resComp.Stdout)) {
			// Compose is installed and version is >= 2.24.0.
		} else {
			return Plan{}, &output.Error{
				Code:    "E_DOCKER_COMPOSE_OLD",
				Message: "docker is running but docker compose is missing or older than 2.24",
				Hint:    "install the compose plugin: apt-get install docker-compose-v2 (or docker-compose-plugin from Docker's repo)",
			}
		}
	} else {
		resComp, errComp := e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"compose", "version", "--short"}, Env: []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}})
		if errComp == nil && resComp.ExitCode == 0 && isComposeSatisfied(string(resComp.Stdout)) {
			return Plan{}, &output.Error{
				Code:    "E_DOCKER_DAEMON_DOWN",
				Message: "docker is installed but the daemon is not responding",
				Hint:    "systemctl start docker",
			}
		}
		// Install docker-ce only when docker is not runnable at all.
		if errInfo == nil && resInfo.ExitCode != 127 {
			return Plan{}, &output.Error{
				Code:    "E_DOCKER_DAEMON_DOWN",
				Message: "docker is installed but the daemon is not responding",
				Hint:    "systemctl start docker",
			}
		}
		changes = append(changes, "install docker")
		actions = append(actions, Action{Kind: "install", Target: "docker"})
	}

	for _, pkg := range defaultOtherPackages {
		if !checkPackageInstalled(ctx, e, pkg) {
			changes = append(changes, "install package "+pkg)
			actions = append(actions, Action{Kind: "install", Target: "package", Arg: pkg})
		}
	}
	return Plan{Changes: changes, Actions: actions}, nil
}

func (PackagesStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	var installDocker bool
	var missingPkgs []string

	for _, a := range p.Actions {
		switch {
		case a.Kind == "install" && a.Target == "docker":
			installDocker = true
		case a.Kind == "install" && a.Target == "package":
			missingPkgs = append(missingPkgs, a.Arg)
		}
	}

	// Base packages must be installed before adding the Docker repo because Docker repo setup requires curl.
	if len(missingPkgs) > 0 {
		if err := runAptCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: []string{"update"},
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}
		args := append([]string{"install", "-y"}, missingPkgs...)
		if err := runAptCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: args,
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}
	}

	if installDocker {
		if err := runRepoCmd(ctx, e, sys.Cmd{
			Name: "install",
			Args: []string{"-m", "0755", "-d", "/etc/apt/keyrings"},
		}); err != nil {
			return "", err
		}

		if err := runRepoCmd(ctx, e, sys.Cmd{
			Name: "curl",
			Args: []string{"-fsSL", "https://download.docker.com/linux/ubuntu/gpg", "-o", "/etc/apt/keyrings/docker.asc"},
		}); err != nil {
			return "", err
		}

		res, err := e.Run.Run(ctx, sys.Cmd{
			Name: "dpkg",
			Args: []string{"--print-architecture"},
		})
		if err != nil {
			return "", err
		}
		if res.ExitCode != 0 {
			dpkgCmd := sys.Cmd{Name: "dpkg", Args: []string{"--print-architecture"}}
			return "", sys.CmdErr("E_DOCKER_REPO", dpkgCmd, res, nil, "check network access to download.docker.com")
		}
		arch := strings.TrimSpace(string(res.Stdout))
		codename, err := ubuntuCodename(e.FS)
		if err != nil {
			return "", err
		}

		dockerListContent := fmt.Sprintf("deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu %s stable\n", arch, codename)
		if err := e.FS.MkdirAll("/etc/apt/sources.list.d", 0o755, sys.Owner{User: "root", Group: "root"}); err != nil {
			return "", err
		}
		if err := e.FS.WriteFile("/etc/apt/sources.list.d/docker.list", []byte(dockerListContent), 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
			return "", err
		}

		if err := runAptCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: []string{"update"},
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}

		if err := runAptCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: []string{"install", "-y", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"},
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}
	}

	return "", nil
}

func runAptCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return sys.CmdErr("E_APT_FAILED", c, res, nil, "check apt sources and network, then re-run cvctl init")
	}
	return nil
}

func runRepoCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return sys.CmdErr("E_DOCKER_REPO", c, res, nil, "check network access to download.docker.com")
	}
	return nil
}
