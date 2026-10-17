package cli

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/term"

	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/project"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}

type opsOpts struct {
	NonInteractive bool
	In             io.Reader
	Out            io.Writer
}

type OpsEnv struct {
	FS          sys.FS
	Run         sys.Runner
	Store       *state.Store
	Deployer    *deploy.Deployer
	Prompt      hostcfg.Prompter
	Interactive bool
	Host        *hostcfg.Config
	Secrets     *hostcfg.Secrets
	Catalog     kinds.Catalog
	DeployRoot  string
	StateRoot   string
	Resolver    registry.Resolver
	RepoFactory func(dir, keyPath string) project.RepoLike
}

func (e *OpsEnv) deployRoot() string {
	if e.DeployRoot != "" {
		return e.DeployRoot
	}
	return "/srv/deploy"
}

func (e *OpsEnv) stateRoot() string {
	if e.StateRoot != "" {
		return e.StateRoot
	}
	return "/var/lib/cvctl"
}

func (e *OpsEnv) fs() sys.FS {
	if e.FS != nil {
		return e.FS
	}
	return sys.NewOSFS()
}

func (e *OpsEnv) runner() sys.Runner {
	if e.Run != nil {
		return e.Run
	}
	return sys.NewExecRunner()
}

func (e *OpsEnv) catalog() (kinds.Catalog, error) {
	if e.Catalog != nil {
		return e.Catalog, nil
	}
	return kinds.Load()
}

func (e *OpsEnv) repo(dir, keyPath string) project.RepoLike {
	if e.RepoFactory != nil {
		return e.RepoFactory(dir, keyPath)
	}
	return &gitrepo.Repo{
		Run:     e.runner(),
		Dir:     dir,
		KeyPath: keyPath,
	}
}

func instanceNotFound(inst string) *output.Error {
	return &output.Error{
		Code:    "E_INSTANCE_NOT_FOUND",
		Message: fmt.Sprintf("instance %q not found", inst),
		Hint:    "check the instance name with cvctl status",
	}
}

func (e *OpsEnv) checkInstance(inst string) error {
	if err := project.ValidateInstanceName(e.deployRoot(), e.stateRoot(), inst); err != nil {
		return instanceNotFound(inst)
	}
	cvctlEnvPath := filepath.Join(e.deployRoot(), inst, "cvctl.env")
	if _, err := e.fs().Stat(cvctlEnvPath); err != nil {
		return instanceNotFound(inst)
	}
	return nil
}

var newOpsEnv = func(o opsOpts) (*OpsEnv, error) {
	run := sys.NewExecRunner()
	fsys := sys.NewOSFS()
	interactive := !o.NonInteractive && term.IsTerminal(int(os.Stdin.Fd()))

	cfg, secrets, err := hostcfg.Load(fsys, hostCfgDir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cat, err := kinds.Load()
	if err != nil {
		return nil, err
	}

	var prompter hostcfg.Prompter
	if interactive {
		prompter = hostcfg.NewHuhPrompter(o.In, o.Out)
	}

	var regResolver registry.Resolver
	if cfg != nil {
		token, _ := secrets.Get("registry_token")
		regResolver = registry.New(cfg.RegistryUser, token, registry.InsecureHostsFromEnv())
	}

	store := state.NewStore(fsys, "/var/lib/cvctl")
	deployer := &deploy.Deployer{
		DeployRoot: "/srv/deploy",
		TraefikDir: "/etc/traefik/dynamic",
		FS:         fsys,
		Store:      store,
		Host:       cfg,
		Catalog:    cat,
	}
	deploy.Wire(deployer, run)

	return &OpsEnv{
		FS:          fsys,
		Run:         run,
		Store:       store,
		Deployer:    deployer,
		Prompt:      prompter,
		Interactive: interactive,
		Host:        cfg,
		Secrets:     secrets,
		Catalog:     cat,
		DeployRoot:  "/srv/deploy",
		StateRoot:   "/var/lib/cvctl",
		Resolver:    regResolver,
	}, nil
}
