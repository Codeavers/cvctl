package host

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/sys"
)

var (
	deployerOwner   = sys.Owner{User: "deployer", Group: "deploy"}
	rootDeployOwner = sys.Owner{User: "root", Group: "deploy"}
)

type dirSpec struct {
	path  string
	mode  os.FileMode
	owner sys.Owner
}

// /srv/deploy must be 2750 (not group-writable) so sshd StrictModes accepts deployer's authorized_keys.
var accountDirs = []dirSpec{
	{"/srv/deploy", 0o2750, deployerOwner},
	{"/etc/cvctl", 0o750, rootDeployOwner},
	{"/etc/cvctl/secrets", 0o750, rootDeployOwner},
	{"/etc/traefik/dynamic", 0o2775, deployerOwner},
	{"/var/lib/cvctl", 0o750, deployerOwner},
}

func accountFiles() []string {
	files := make([]string, 0, 1+len(hostcfg.SecretNames))
	files = append(files, "/etc/cvctl/host.env")
	for _, name := range hostcfg.SecretNames {
		files = append(files, path.Join("/etc/cvctl/secrets", name))
	}
	return files
}

func hasGroups(stdout []byte, want ...string) bool {
	groups := strings.Fields(string(stdout))
	for _, w := range want {
		if !slices.Contains(groups, w) {
			return false
		}
	}
	return true
}

type AccountsStep struct{}

func (AccountsStep) Name() string { return "accounts" }

func (AccountsStep) Check(ctx context.Context, e *Env) (Plan, error) {
	var changes []string
	var actions []Action

	res, err := e.Run.Run(ctx, sys.Cmd{Name: "getent", Args: []string{"group", "deploy"}})
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 {
		changes = append(changes, "create group deploy")
		actions = append(actions, Action{Kind: "groupadd", Target: "deploy"})
	}

	res, err = e.Run.Run(ctx, sys.Cmd{Name: "getent", Args: []string{"passwd", "deployer"}})
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 {
		changes = append(changes, "create user deployer")
		actions = append(actions, Action{Kind: "useradd", Target: "deployer"})
	}

	res, err = e.Run.Run(ctx, sys.Cmd{Name: "id", Args: []string{"-nG", "deployer"}})
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 || !hasGroups(res.Stdout, "docker", "deploy") {
		changes = append(changes, "add deployer to docker,deploy")
		actions = append(actions, Action{Kind: "usermod", Target: "deployer", Arg: "docker,deploy"})
	}

	if e.Cfg != nil && e.Cfg.AdminUser != "" {
		admin := e.Cfg.AdminUser
		res, err = e.Run.Run(ctx, sys.Cmd{Name: "getent", Args: []string{"passwd", admin}})
		if err != nil {
			return Plan{}, err
		}
		if res.ExitCode == 0 {
			res, err = e.Run.Run(ctx, sys.Cmd{Name: "id", Args: []string{"-nG", admin}})
			if err != nil {
				return Plan{}, err
			}
			if res.ExitCode != 0 || !hasGroups(res.Stdout, "deploy") {
				changes = append(changes, "add "+admin+" to deploy")
				actions = append(actions, Action{Kind: "usermod", Target: admin, Arg: "deploy"})
			}
		}
	}

	for _, d := range accountDirs {
		fi, err := e.FS.Stat(d.path)
		if errors.Is(err, fs.ErrNotExist) {
			changes = append(changes, "directory "+d.path)
			actions = append(actions, Action{Kind: "mkdir", Target: d.path})
			continue
		}
		if err != nil {
			return Plan{}, err
		}
		if !fi.IsDir || fi.Mode != d.mode || fi.User != d.owner.User || fi.Group != d.owner.Group {
			changes = append(changes, "directory "+d.path)
			actions = append(actions, Action{Kind: "mkdir", Target: d.path})
		}
	}

	for _, f := range accountFiles() {
		fi, err := e.FS.Stat(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return Plan{}, err
		}
		if fi.IsDir || fi.Mode != 0o640 || fi.User != rootDeployOwner.User || fi.Group != rootDeployOwner.Group {
			changes = append(changes, "permissions "+f)
			actions = append(actions, Action{Kind: "chmod", Target: f})
		}
	}

	return Plan{Changes: changes, Actions: actions}, nil
}

func (AccountsStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	for _, a := range p.Actions {
		switch a.Kind {
		case "groupadd":
			if err := runAccountsCmd(ctx, e, sys.Cmd{
				Name: "groupadd",
				Args: []string{a.Target},
			}); err != nil {
				return "", err
			}
		case "useradd":
			if a.Target == "deployer" {
				if err := runAccountsCmd(ctx, e, sys.Cmd{
					Name: "useradd",
					Args: []string{"--system", "--home-dir", "/srv/deploy", "--no-create-home", "--shell", "/bin/bash", "deployer"},
				}); err != nil {
					return "", err
				}
			}
		case "usermod":
			if err := runAccountsCmd(ctx, e, sys.Cmd{
				Name: "usermod",
				Args: []string{"-aG", a.Arg, a.Target},
			}); err != nil {
				return "", err
			}
		case "mkdir":
			for _, d := range accountDirs {
				if d.path == a.Target {
					if err := e.FS.MkdirAll(d.path, d.mode, d.owner); err != nil {
						return "", err
					}
					break
				}
			}
		case "chmod":
			data, err := e.FS.ReadFile(a.Target)
			if err != nil {
				return "", err
			}
			if err := e.FS.WriteFile(a.Target, data, 0o640, rootDeployOwner); err != nil {
				return "", err
			}
		}
	}

	return "", nil
}

func runAccountsCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		return sys.CmdErr("E_ACCOUNTS_FAILED", c, res, nil, fmt.Sprintf("check `%s` output and system user/group database", c.String()))
	}
	return nil
}
