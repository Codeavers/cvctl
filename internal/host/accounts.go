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
	"github.com/codeavers/cvctl/internal/output"
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

	res, err := e.Run.Run(ctx, sys.Cmd{Name: "getent", Args: []string{"group", "deploy"}})
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 {
		changes = append(changes, "create group deploy")
	}

	res, err = e.Run.Run(ctx, sys.Cmd{Name: "getent", Args: []string{"passwd", "deployer"}})
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 {
		changes = append(changes, "create user deployer")
	}

	res, err = e.Run.Run(ctx, sys.Cmd{Name: "id", Args: []string{"-nG", "deployer"}})
	if err != nil {
		return Plan{}, err
	}
	if res.ExitCode != 0 || !hasGroups(res.Stdout, "docker", "deploy") {
		changes = append(changes, "add deployer to docker,deploy")
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
			}
		}
	}

	for _, d := range accountDirs {
		fi, err := e.FS.Stat(d.path)
		if errors.Is(err, fs.ErrNotExist) {
			changes = append(changes, "directory "+d.path)
			continue
		}
		if err != nil {
			return Plan{}, err
		}
		if !fi.IsDir || fi.Mode != d.mode || fi.User != d.owner.User || fi.Group != d.owner.Group {
			changes = append(changes, "directory "+d.path)
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
		}
	}

	return Plan{Changes: changes}, nil
}

func (AccountsStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	changeSet := make(map[string]bool, len(p.Changes))
	for _, c := range p.Changes {
		changeSet[c] = true
	}

	if changeSet["create group deploy"] {
		if err := runAccountsCmd(ctx, e, sys.Cmd{
			Name: "groupadd",
			Args: []string{"deploy"},
		}); err != nil {
			return "", err
		}
	}

	if changeSet["create user deployer"] {
		if err := runAccountsCmd(ctx, e, sys.Cmd{
			Name: "useradd",
			Args: []string{"--system", "--home-dir", "/srv/deploy", "--no-create-home", "--shell", "/bin/bash", "deployer"},
		}); err != nil {
			return "", err
		}
	}

	if changeSet["add deployer to docker,deploy"] {
		if err := runAccountsCmd(ctx, e, sys.Cmd{
			Name: "usermod",
			Args: []string{"-aG", "docker,deploy", "deployer"},
		}); err != nil {
			return "", err
		}
	}

	if e.Cfg != nil && e.Cfg.AdminUser != "" && changeSet["add "+e.Cfg.AdminUser+" to deploy"] {
		if err := runAccountsCmd(ctx, e, sys.Cmd{
			Name: "usermod",
			Args: []string{"-aG", "deploy", e.Cfg.AdminUser},
		}); err != nil {
			return "", err
		}
	}

	for _, d := range accountDirs {
		if changeSet["directory "+d.path] {
			if err := e.FS.MkdirAll(d.path, d.mode, d.owner); err != nil {
				return "", err
			}
		}
	}

	for _, f := range accountFiles() {
		if changeSet["permissions "+f] {
			data, err := e.FS.ReadFile(f)
			if err != nil {
				return "", err
			}
			if err := e.FS.WriteFile(f, data, 0o640, rootDeployOwner); err != nil {
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
		stderr := strings.TrimSpace(string(res.Stderr))
		msg := fmt.Sprintf("%s failed (exit %d)", c.String(), res.ExitCode)
		if stderr != "" {
			msg += ": " + stderr
		}
		return &output.Error{
			Code:    "E_ACCOUNTS_FAILED",
			Message: msg,
			Hint:    fmt.Sprintf("check `%s` output and system user/group database", c.String()),
		}
	}
	return nil
}
