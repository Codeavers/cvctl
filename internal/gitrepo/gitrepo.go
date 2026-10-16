package gitrepo

import (
	"context"
	"fmt"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const gitDeployKeyHint = "check the deploy key is added to the repo with read access"

// Repo provides git operations against a repository checkout using an optional deploy key.
type Repo struct {
	Run     sys.Runner
	Dir     string
	KeyPath string
}

func (r *Repo) env() []string {
	if r.KeyPath == "" {
		return nil
	}
	return []string{
		fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new", r.KeyPath),
	}
}

// Clone performs a single-branch clone into Dir.
func (r *Repo) Clone(ctx context.Context, url, branch string) error {
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"clone", "--branch", branch, "--single-branch", url, r.Dir},
		Env:  r.env(),
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	return nil
}

// Fetch fetches the given branch from origin and returns the resolved commit SHA.
func (r *Repo) Fetch(ctx context.Context, branch string) (string, error) {
	fetchCmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "fetch", "origin", branch},
		Env:  r.env(),
	}
	res, err := r.Run.Run(ctx, fetchCmd)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr("E_GIT_FAILED", fetchCmd, res, err, gitDeployKeyHint)
	}

	revCmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "rev-parse", "origin/" + branch},
		Env:  r.env(),
	}
	res, err = r.Run.Run(ctx, revCmd)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr("E_GIT_FAILED", revCmd, res, err, gitDeployKeyHint)
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// Checkout checks out a specific commit in detached HEAD state.
func (r *Repo) Checkout(ctx context.Context, commit string) error {
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "checkout", "--detach", commit},
		Env:  r.env(),
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	return nil
}

// Show reads a file content from a specific commit in the repository.
// If the path does not exist in the commit, it returns an E_GIT_NO_FILE error.
func (r *Repo) Show(ctx context.Context, commit, path string) ([]byte, error) {
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "show", fmt.Sprintf("%s:%s", commit, path)},
		Env:  r.env(),
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil {
		return nil, sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	if res.ExitCode != 0 {
		stderr := string(res.Stderr)
		if strings.Contains(stderr, "does not exist") || strings.Contains(stderr, "exists on disk, but not in") {
			return nil, &output.Error{
				Code:    "E_GIT_NO_FILE",
				Message: fmt.Sprintf("%s:%s does not exist in repo", commit, path),
				Hint:    "verify the file exists at this commit",
			}
		}
		return nil, sys.CmdErr("E_GIT_FAILED", cmd, res, nil, gitDeployKeyHint)
	}
	return res.Stdout, nil
}

// IsAncestor checks if commit is an ancestor of origin/<branch>.
// Exit code 0 returns true; exit code 1 returns false; other exit codes return E_GIT_FAILED.
func (r *Repo) IsAncestor(ctx context.Context, commit, branch string) (bool, error) {
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "merge-base", "--is-ancestor", commit, "origin/" + branch},
		Env:  r.env(),
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil {
		return false, sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, sys.CmdErr("E_GIT_FAILED", cmd, res, nil, gitDeployKeyHint)
	}
}

// LsRemote checks accessibility of the remote repository URL via git ls-remote.
func (r *Repo) LsRemote(ctx context.Context, url string) error {
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"ls-remote", url, "HEAD"},
		Env:  r.env(),
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	return nil
}
