package gitrepo

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	gitDeployKeyHint      = "check the deploy key is added to the repo with read access"
	forbiddenKeyPathChars = "'\"$\\`;" + "&|<>()"
)

// EUID is the function used to query the effective user ID, injectable for testing.
var EUID = os.Geteuid

var (
	urlCredsRe = regexp.MustCompile(`(?i)(https?://)(?:[^@/\s]+@)`)
	// ssh user names are not secrets; only a password is masked
	sshPassRe = regexp.MustCompile(`(?i)(ssh://[^:@/\s]+):[^@/\s]*@`)
)

// RedactURL masks credentials in HTTP/HTTPS URLs and passwords in ssh:// URLs,
// whatever the case of the scheme.
func RedactURL(s string) string {
	s = urlCredsRe.ReplaceAllString(s, "${1}***@")
	return sshPassRe.ReplaceAllString(s, "${1}:***@")
}

// Repo provides git operations against a repository checkout using an optional deploy key.
type Repo struct {
	Run     sys.Runner
	Dir     string
	KeyPath string
}

func (r *Repo) gitCmd(args []string, env []string) sys.Cmd {
	if EUID() == 0 {
		return sys.Cmd{
			Name: "setpriv",
			Args: append([]string{"--reuid=deployer", "--regid=deploy", "--init-groups", "--", "git"}, args...),
			Env:  env,
		}
	}
	return sys.Cmd{
		Name: "git",
		Args: args,
		Env:  env,
	}
}

func (r *Repo) cmdErr(cmd sys.Cmd, res sys.Result, err error) *output.Error {
	oe := sys.CmdErr("E_GIT_FAILED", cmd, res, err, gitDeployKeyHint)
	oe.Message = RedactURL(oe.Message)
	return oe
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
	envs := []string{"LC_ALL=C"}
	if EUID() == 0 {
		envs = append(envs, "HOME=/srv/deploy")
	}
	if r.KeyPath == "" {
		return envs, nil
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
	envs = append(envs, sshCmd)
	return envs, nil
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
	cmd := r.gitCmd([]string{"clone", "--branch", branch, "--single-branch", "--", url, r.Dir}, env)
	res, err := r.Run.Run(ctx, cmd)
	if err != nil {
		return r.cmdErr(cmd, res, err)
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
		return r.cmdErr(cmd, res, nil)
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
	fetchCmd := r.gitCmd([]string{"-C", r.Dir, "fetch", "origin", branch}, env)
	res, err := r.Run.Run(ctx, fetchCmd)
	if err != nil || res.ExitCode != 0 {
		return "", r.cmdErr(fetchCmd, res, err)
	}

	revCmd := r.gitCmd([]string{"-C", r.Dir, "rev-parse", "origin/" + branch}, env)
	res, err = r.Run.Run(ctx, revCmd)
	if err != nil || res.ExitCode != 0 {
		return "", r.cmdErr(revCmd, res, err)
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
	cmd := r.gitCmd([]string{"-C", r.Dir, "checkout", "--detach", commit, "--"}, env)
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return r.cmdErr(cmd, res, err)
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
	cmd := r.gitCmd([]string{"-C", r.Dir, "show", fmt.Sprintf("%s:%s", commit, path)}, env)
	res, err := r.Run.Run(ctx, cmd)
	if err != nil {
		return nil, r.cmdErr(cmd, res, err)
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
		return nil, r.cmdErr(cmd, res, nil)
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
	cmd := r.gitCmd([]string{"-C", r.Dir, "merge-base", "--is-ancestor", commit, "origin/" + branch}, env)
	res, err := r.Run.Run(ctx, cmd)
	if err != nil {
		return false, r.cmdErr(cmd, res, err)
	}
	switch res.ExitCode {
	case 0:
		return true, nil
	case 1:
		return false, nil
	default:
		return false, r.cmdErr(cmd, res, nil)
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
	cmd := r.gitCmd([]string{"ls-remote", "--", url, "HEAD"}, env)
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return r.cmdErr(cmd, res, err)
	}
	return nil
}
