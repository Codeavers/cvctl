package gitrepo

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	gitDeployKeyHint      = "check the deploy key is added to the repo with read access"
	forbiddenKeyPathChars = "'\"$\\`;" + "&|<>()"
)

// Repo provides git operations against a repository checkout using an optional deploy key.
type Repo struct {
	Run     sys.Runner
	Dir     string
	KeyPath string
}

func validateRef(s string) error {
	if strings.HasPrefix(s, "-") {
		return &output.Error{
			Code:    "E_GIT_BAD_REF",
			Message: fmt.Sprintf("invalid reference or URL %q: cannot start with '-'", s),
			Hint:    "refs and URLs cannot start with '-'",
		}
	}
	return nil
}

func (r *Repo) env() ([]string, error) {
	if r.KeyPath == "" {
		return []string{"LC_ALL=C"}, nil
	}
	if strings.ContainsFunc(r.KeyPath, unicode.IsSpace) || strings.ContainsAny(r.KeyPath, forbiddenKeyPathChars) {
		return nil, &output.Error{
			Code:    "E_GIT_BAD_KEYPATH",
			Message: fmt.Sprintf("invalid deploy key path %q: contains whitespace or shell characters", r.KeyPath),
			Hint:    "deploy key path cannot contain whitespace or shell characters",
		}
	}
	knownHosts := filepath.Join(filepath.Dir(r.KeyPath), "known_hosts")
	sshCmd := fmt.Sprintf("GIT_SSH_COMMAND=ssh -i %s -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=%s", r.KeyPath, knownHosts)
	return []string{
		"LC_ALL=C",
		sshCmd,
	}, nil
}

// Clone performs a single-branch clone into Dir.
func (r *Repo) Clone(ctx context.Context, url, branch string) error {
	if err := validateRef(url); err != nil {
		return err
	}
	if err := validateRef(branch); err != nil {
		return err
	}
	env, err := r.env()
	if err != nil {
		return err
	}
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"clone", "--branch", branch, "--single-branch", "--", url, r.Dir},
		Env:  env,
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil {
		return sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	if res.ExitCode != 0 {
		stderr := string(res.Stderr)
		if strings.Contains(stderr, "already exists and is not an empty directory") {
			return &output.Error{
				Code:    "E_GIT_DIR_NOT_EMPTY",
				Message: fmt.Sprintf("destination directory %q already exists and is not an empty directory", r.Dir),
				Hint:    "remove it or run cvctl project update",
			}
		}
		return sys.CmdErr("E_GIT_FAILED", cmd, res, nil, gitDeployKeyHint)
	}
	return nil
}

// Fetch fetches the given branch from origin and returns the resolved commit SHA.
func (r *Repo) Fetch(ctx context.Context, branch string) (string, error) {
	if err := validateRef(branch); err != nil {
		return "", err
	}
	env, err := r.env()
	if err != nil {
		return "", err
	}
	fetchCmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "fetch", "origin", branch},
		Env:  env,
	}
	res, err := r.Run.Run(ctx, fetchCmd)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr("E_GIT_FAILED", fetchCmd, res, err, gitDeployKeyHint)
	}

	revCmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "rev-parse", "origin/" + branch},
		Env:  env,
	}
	res, err = r.Run.Run(ctx, revCmd)
	if err != nil || res.ExitCode != 0 {
		return "", sys.CmdErr("E_GIT_FAILED", revCmd, res, err, gitDeployKeyHint)
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

// Checkout checks out a specific commit in detached HEAD state.
func (r *Repo) Checkout(ctx context.Context, commit string) error {
	if err := validateRef(commit); err != nil {
		return err
	}
	env, err := r.env()
	if err != nil {
		return err
	}
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "checkout", "--detach", commit, "--"},
		Env:  env,
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
	if err := validateRef(commit); err != nil {
		return nil, err
	}
	env, err := r.env()
	if err != nil {
		return nil, err
	}
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "show", fmt.Sprintf("%s:%s", commit, path)},
		Env:  env,
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
	if err := validateRef(commit); err != nil {
		return false, err
	}
	if err := validateRef(branch); err != nil {
		return false, err
	}
	env, err := r.env()
	if err != nil {
		return false, err
	}
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"-C", r.Dir, "merge-base", "--is-ancestor", commit, "origin/" + branch},
		Env:  env,
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
	if err := validateRef(url); err != nil {
		return err
	}
	env, err := r.env()
	if err != nil {
		return err
	}
	cmd := sys.Cmd{
		Name: "git",
		Args: []string{"ls-remote", "--", url, "HEAD"},
		Env:  env,
	}
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	}
	return nil
}
