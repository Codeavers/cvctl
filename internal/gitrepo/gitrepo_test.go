package gitrepo_test

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const (
	testDir     = "/srv/deploy/myproject"
	testKeyPath = "/etc/cvctl/keys/deploy.key"
	testSSHCmd  = "GIT_SSH_COMMAND=ssh -i /etc/cvctl/keys/deploy.key -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/etc/cvctl/keys/known_hosts"
)

var defaultExpectedEnv = []string{"LC_ALL=C", testSSHCmd}

func assertEnv(t *testing.T, got, want []string) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("expected env %v, got %v", want, got)
	}
}

func TestCloneArgsAndEnv(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	url := "git@github.com:codeavers/infra.git"
	branch := "main"
	expectedCmd := "git clone --branch main --single-branch -- git@github.com:codeavers/infra.git " + testDir
	run.On(expectedCmd, sys.Result{ExitCode: 0})

	ctx := context.Background()
	err := repo.Clone(ctx, url, branch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	call := run.Calls[0]
	if call.String() != expectedCmd {
		t.Errorf("expected command %q, got %q", expectedCmd, call.String())
	}
	assertEnv(t, call.Env, defaultExpectedEnv)
}

func TestCloneNoKeyPath(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: "",
	}

	url := "file:///tmp/repo.git"
	branch := "main"
	expectedCmd := "git clone --branch main --single-branch -- file:///tmp/repo.git " + testDir
	run.On(expectedCmd, sys.Result{ExitCode: 0})

	ctx := context.Background()
	err := repo.Clone(ctx, url, branch)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	call := run.Calls[0]
	assertEnv(t, call.Env, []string{"LC_ALL=C"})
}

func TestFetchReturnsCommit(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	fetchCmd := "git -C " + testDir + " fetch origin main"
	revParseCmd := "git -C " + testDir + " rev-parse origin/main"
	expectedCommit := "a1b2c3d4e5f67890abcdef1234567890abcdef12"

	run.On(fetchCmd, sys.Result{ExitCode: 0})
	run.On(revParseCmd, sys.Result{ExitCode: 0, Stdout: []byte(expectedCommit + "\n")})

	ctx := context.Background()
	commit, err := repo.Fetch(ctx, "main")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if commit != expectedCommit {
		t.Fatalf("expected commit %s, got %s", expectedCommit, commit)
	}

	if len(run.Calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(run.Calls))
	}
	for i, call := range run.Calls {
		assertEnv(t, call.Env, defaultExpectedEnv)
		_ = i
	}
}

func TestFetchFailure(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	fetchCmd := "git -C " + testDir + " fetch origin main"
	run.On(fetchCmd, sys.Result{
		ExitCode: 128,
		Stderr:   []byte("Permission denied (publickey)"),
	})

	ctx := context.Background()
	_, err := repo.Fetch(ctx, "main")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_GIT_FAILED" {
		t.Errorf("expected code E_GIT_FAILED, got %s", oe.Code)
	}
	if oe.Hint != "check the deploy key is added to the repo with read access" {
		t.Errorf("expected deploy key hint, got %q", oe.Hint)
	}
	if !strings.Contains(oe.Message, "Permission denied (publickey)") {
		t.Errorf("expected stderr in message, got %q", oe.Message)
	}
}

func TestCheckout(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	commit := "a1b2c3d4"
	expectedCmd := "git -C " + testDir + " checkout --detach " + commit + " --"
	run.On(expectedCmd, sys.Result{ExitCode: 0})

	ctx := context.Background()
	err := repo.Checkout(ctx, commit)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	call := run.Calls[0]
	if call.String() != expectedCmd {
		t.Errorf("expected command %q, got %q", expectedCmd, call.String())
	}
	assertEnv(t, call.Env, defaultExpectedEnv)
}

func TestShowSuccess(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	expectedContent := []byte("services:\n  app:\n    image: nginx\n")
	expectedCmd := "git -C " + testDir + " show v1.0.0:docker-compose.yml"
	run.On(expectedCmd, sys.Result{
		ExitCode: 0,
		Stdout:   expectedContent,
	})

	ctx := context.Background()
	data, err := repo.Show(ctx, "v1.0.0", "docker-compose.yml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if string(data) != string(expectedContent) {
		t.Fatalf("expected %q, got %q", string(expectedContent), string(data))
	}
	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	assertEnv(t, run.Calls[0].Env, defaultExpectedEnv)
}

func TestShowMissingFile(t *testing.T) {
	tests := []struct {
		name    string
		stderr  string
		commit  string
		path    string
		wantCod string
	}{
		{
			name:    "does not exist in commit",
			stderr:  "fatal: path 'nonexistent.yml' does not exist in 'HEAD'",
			commit:  "HEAD",
			path:    "nonexistent.yml",
			wantCod: "E_GIT_NO_FILE",
		},
		{
			name:    "exists on disk but not in commit",
			stderr:  "fatal: Path 'local-only.yml' exists on disk, but not in 'commit123'",
			commit:  "commit123",
			path:    "local-only.yml",
			wantCod: "E_GIT_NO_FILE",
		},
		{
			name:    "other git failure",
			stderr:  "fatal: bad object commit999",
			commit:  "commit999",
			path:    "app.yml",
			wantCod: "E_GIT_FAILED",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := &systest.Runner{}
			repo := &gitrepo.Repo{
				Run:     run,
				Dir:     testDir,
				KeyPath: testKeyPath,
			}

			cmd := "git -C " + testDir + " show " + tc.commit + ":" + tc.path
			run.On(cmd, sys.Result{
				ExitCode: 128,
				Stderr:   []byte(tc.stderr),
			})

			ctx := context.Background()
			_, err := repo.Show(ctx, tc.commit, tc.path)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}

			var oe *output.Error
			if !errors.As(err, &oe) {
				t.Fatalf("expected *output.Error, got %T: %v", err, err)
			}
			if oe.Code != tc.wantCod {
				t.Errorf("expected code %s, got %s", tc.wantCod, oe.Code)
			}
			if tc.wantCod == "E_GIT_NO_FILE" {
				if !strings.Contains(oe.Message, tc.commit) || !strings.Contains(oe.Message, tc.path) {
					t.Errorf("expected message to name commit %q and path %q, got %q", tc.commit, tc.path, oe.Message)
				}
			}
		})
	}
}

func TestIsAncestor(t *testing.T) {
	tests := []struct {
		name       string
		exitCode   int
		stderr     string
		wantResult bool
		wantErr    bool
	}{
		{
			name:       "is ancestor (exit 0)",
			exitCode:   0,
			wantResult: true,
			wantErr:    false,
		},
		{
			name:       "not an ancestor (exit 1)",
			exitCode:   1,
			wantResult: false,
			wantErr:    false,
		},
		{
			name:       "git error (exit 128)",
			exitCode:   128,
			stderr:     "fatal: Not a valid object name badcommit",
			wantResult: false,
			wantErr:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			run := &systest.Runner{}
			repo := &gitrepo.Repo{
				Run:     run,
				Dir:     testDir,
				KeyPath: testKeyPath,
			}

			commit := "abcdef1"
			branch := "main"
			expectedCmd := "git -C " + testDir + " merge-base --is-ancestor " + commit + " origin/" + branch
			run.On(expectedCmd, sys.Result{
				ExitCode: tc.exitCode,
				Stderr:   []byte(tc.stderr),
			})

			ctx := context.Background()
			result, err := repo.IsAncestor(ctx, commit, branch)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil")
				}
				var oe *output.Error
				if !errors.As(err, &oe) {
					t.Fatalf("expected *output.Error, got %T: %v", err, err)
				}
				if oe.Code != "E_GIT_FAILED" {
					t.Errorf("expected code E_GIT_FAILED, got %s", oe.Code)
				}
			} else {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if result != tc.wantResult {
					t.Errorf("expected %v, got %v", tc.wantResult, result)
				}
			}
		})
	}
}

func TestLsRemote(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	url := "git@github.com:codeavers/infra.git"
	expectedCmd := "git ls-remote -- " + url + " HEAD"
	run.On(expectedCmd, sys.Result{
		ExitCode: 0,
		Stdout:   []byte("a1b2c3d4e5f6\tHEAD\n"),
	})

	ctx := context.Background()
	err := repo.LsRemote(ctx, url)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	call := run.Calls[0]
	if call.String() != expectedCmd {
		t.Errorf("expected command %q, got %q", expectedCmd, call.String())
	}
	assertEnv(t, call.Env, defaultExpectedEnv)
}

func TestLsRemoteFailure(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	url := "git@github.com:codeavers/infra.git"
	expectedCmd := "git ls-remote -- " + url + " HEAD"
	run.On(expectedCmd, sys.Result{
		ExitCode: 128,
		Stderr:   []byte("Permission denied (publickey)"),
	})

	ctx := context.Background()
	err := repo.LsRemote(ctx, url)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_GIT_FAILED" {
		t.Errorf("expected code E_GIT_FAILED, got %s", oe.Code)
	}
	if oe.Hint != "check the deploy key is added to the repo with read access" {
		t.Errorf("expected hint 'check the deploy key is added to the repo with read access', got %q", oe.Hint)
	}
}

func assertBadRef(t *testing.T, err error, run *systest.Runner) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_GIT_BAD_REF" {
		t.Errorf("expected code E_GIT_BAD_REF, got %s", oe.Code)
	}
	if oe.Hint != "refs and URLs cannot start with '-'" {
		t.Errorf("expected hint \"refs and URLs cannot start with '-'\", got %q", oe.Hint)
	}
	if len(run.Calls) != 0 {
		t.Errorf("expected no runner calls, got %d", len(run.Calls))
	}
}

func TestArgumentInjectionRejectsDash(t *testing.T) {
	ctx := context.Background()

	t.Run("Clone bad URL", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		err := repo.Clone(ctx, "-u", "main")
		assertBadRef(t, err, run)
	})

	t.Run("Clone bad branch", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		err := repo.Clone(ctx, "git@github.com:foo/bar.git", "--branch-option")
		assertBadRef(t, err, run)
	})

	t.Run("Fetch bad branch", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		_, err := repo.Fetch(ctx, "-v")
		assertBadRef(t, err, run)
	})

	t.Run("Checkout bad commit", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		err := repo.Checkout(ctx, "--detach")
		assertBadRef(t, err, run)
	})

	t.Run("Show bad commit", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		_, err := repo.Show(ctx, "-badcommit", "file.txt")
		assertBadRef(t, err, run)
	})

	t.Run("IsAncestor bad commit", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		_, err := repo.IsAncestor(ctx, "-badcommit", "main")
		assertBadRef(t, err, run)
	})

	t.Run("IsAncestor bad branch", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		_, err := repo.IsAncestor(ctx, "abcdef1", "-badbranch")
		assertBadRef(t, err, run)
	})

	t.Run("LsRemote bad URL", func(t *testing.T) {
		run := &systest.Runner{}
		repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: testKeyPath}
		err := repo.LsRemote(ctx, "--upload-pack=echo pwn")
		assertBadRef(t, err, run)
	})
}

func TestRejectBadKeyPath(t *testing.T) {
	ctx := context.Background()
	badPaths := []struct {
		name string
		path string
	}{
		{"space", "/etc/cvctl/keys/deploy key"},
		{"tab", "/etc/cvctl/keys/deploy\tkey"},
		{"newline", "/etc/cvctl/keys/deploy\nkey"},
		{"single quote", "/etc/cvctl/keys/deploy'key"},
		{"double quote", "/etc/cvctl/keys/deploy\"key"},
		{"dollar", "/etc/cvctl/keys/$key"},
		{"backslash", "/etc/cvctl/keys/deploy\\key"},
		{"backtick", "/etc/cvctl/keys/`key`"},
		{"semicolon", "/etc/cvctl/keys/key;rm"},
		{"ampersand", "/etc/cvctl/keys/key&echo"},
		{"pipe", "/etc/cvctl/keys/key|cat"},
		{"less than", "/etc/cvctl/keys/key<input"},
		{"greater than", "/etc/cvctl/keys/key>output"},
		{"open paren", "/etc/cvctl/keys/key(1)"},
		{"close paren", "/etc/cvctl/keys/key)1("},
	}

	for _, tc := range badPaths {
		t.Run(tc.name, func(t *testing.T) {
			run := &systest.Runner{}
			repo := &gitrepo.Repo{Run: run, Dir: testDir, KeyPath: tc.path}

			err := repo.Clone(ctx, "git@github.com:codeavers/infra.git", "main")
			if err == nil {
				t.Fatalf("expected error for path %q, got nil", tc.path)
			}
			var oe *output.Error
			if !errors.As(err, &oe) {
				t.Fatalf("expected *output.Error, got %T: %v", err, err)
			}
			if oe.Code != "E_GIT_BAD_KEYPATH" {
				t.Errorf("expected code E_GIT_BAD_KEYPATH, got %s", oe.Code)
			}
			if len(run.Calls) != 0 {
				t.Errorf("expected no runner calls, got %d", len(run.Calls))
			}
		})
	}
}

func TestSSHCommandEnvExact(t *testing.T) {
	keyPath := "/var/lib/cvctl/instances/proj-dev/deploy.key"
	expectedKnownHosts := filepath.Join("/var/lib/cvctl/instances/proj-dev", "known_hosts")
	expectedSSHCmd := "GIT_SSH_COMMAND=ssh -i " + keyPath + " -o IdentitiesOnly=yes -o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=" + expectedKnownHosts

	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: keyPath,
	}

	expectedCmd := "git ls-remote -- git@github.com:codeavers/infra.git HEAD"
	run.On(expectedCmd, sys.Result{ExitCode: 0})

	err := repo.LsRemote(context.Background(), "git@github.com:codeavers/infra.git")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	assertEnv(t, run.Calls[0].Env, []string{"LC_ALL=C", expectedSSHCmd})
}

func TestCloneDirNotEmpty(t *testing.T) {
	run := &systest.Runner{}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     testDir,
		KeyPath: testKeyPath,
	}

	url := "git@github.com:codeavers/infra.git"
	branch := "main"
	expectedCmd := "git clone --branch main --single-branch -- git@github.com:codeavers/infra.git " + testDir
	run.On(expectedCmd, sys.Result{
		ExitCode: 128,
		Stderr:   []byte("fatal: destination path '" + testDir + "' already exists and is not an empty directory.\n"),
	})

	ctx := context.Background()
	err := repo.Clone(ctx, url, branch)
	if err == nil {
		t.Fatalf("expected error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_GIT_DIR_NOT_EMPTY" {
		t.Errorf("expected code E_GIT_DIR_NOT_EMPTY, got %s", oe.Code)
	}
	if !strings.Contains(oe.Message, testDir) {
		t.Errorf("expected message to name dir %q, got %q", testDir, oe.Message)
	}
	if oe.Hint != "remove it or run cvctl project update" {
		t.Errorf("expected hint 'remove it or run cvctl project update', got %q", oe.Hint)
	}
}
