package host

import (
	"context"
	"fmt"
	"strconv"
	"strings"

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

func checkDockerSatisfied(ctx context.Context, e *Env) bool {
	res, err := e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"compose", "version", "--short"}})
	if err != nil || res.ExitCode != 0 || !isComposeSatisfied(string(res.Stdout)) {
		return false
	}
	res, err = e.Run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"info"}})
	if err != nil || res.ExitCode != 0 {
		return false
	}
	return true
}

func checkPackageInstalled(ctx context.Context, e *Env, pkg string) bool {
	res, err := e.Run.Run(ctx, sys.Cmd{
		Name: "dpkg-query",
		Args: []string{"-W", "-f='${Status}'", pkg},
	})
	if err != nil || res.ExitCode != 0 {
		return false
	}
	return strings.TrimSpace(string(res.Stdout)) == "install ok installed"
}

func (PackagesStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var changes []string
	if !checkDockerSatisfied(ctx, e) {
		changes = append(changes, "install docker")
	}
	for _, pkg := range defaultOtherPackages {
		if !checkPackageInstalled(ctx, e, pkg) {
			changes = append(changes, "install package "+pkg)
		}
	}
	return Plan{Changes: changes}, nil
}

func (PackagesStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	var installDocker bool
	var missingPkgs []string

	for _, c := range p.Changes {
		switch {
		case c == "install docker":
			installDocker = true
		case strings.HasPrefix(c, "install package "):
			missingPkgs = append(missingPkgs, strings.TrimPrefix(c, "install package "))
		}
	}

	if installDocker {
		if err := runCmd(ctx, e, sys.Cmd{
			Name: "install",
			Args: []string{"-m", "0755", "-d", "/etc/apt/keyrings"},
		}); err != nil {
			return "", err
		}

		if err := runCmd(ctx, e, sys.Cmd{
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
			return "", fmt.Errorf("dpkg --print-architecture failed (exit %d): %s", res.ExitCode, strings.TrimSpace(string(res.Stderr)))
		}
		arch := strings.TrimSpace(string(res.Stdout))

		dockerListContent := fmt.Sprintf("deb [arch=%s signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/ubuntu noble stable\n", arch)
		_ = e.FS.MkdirAll("/etc/apt/sources.list.d", 0o755, sys.Owner{User: "root", Group: "root"})
		if err := e.FS.WriteFile("/etc/apt/sources.list.d/docker.list", []byte(dockerListContent), 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
			return "", err
		}

		if err := runCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: []string{"update"},
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}

		if err := runCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: []string{"install", "-y", "docker-ce", "docker-ce-cli", "containerd.io", "docker-buildx-plugin", "docker-compose-plugin"},
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}
	}

	if len(missingPkgs) > 0 {
		args := append([]string{"install", "-y"}, missingPkgs...)
		if err := runCmd(ctx, e, sys.Cmd{
			Name: "apt-get",
			Args: args,
			Env:  []string{"DEBIAN_FRONTEND=noninteractive"},
		}); err != nil {
			return "", err
		}
	}

	return "", nil
}

func runCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return fmt.Errorf("%s failed (exit %d): %s", c.String(), res.ExitCode, strings.TrimSpace(string(res.Stderr)))
	}
	return nil
}
