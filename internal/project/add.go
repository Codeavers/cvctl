package project

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}
var identRe = regexp.MustCompile(`^[a-z0-9_]+$`)

const alphaNum = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

func randomPassword32() (string, error) {
	b := make([]byte, 32)
	var buf [1]byte
	for i := 0; i < 32; {
		if _, err := rand.Read(buf[:]); err != nil {
			return "", err
		}
		if int(buf[0]) >= 256/len(alphaNum)*len(alphaNum) {
			continue
		}
		b[i] = alphaNum[int(buf[0])%len(alphaNum)]
		i++
	}
	return string(b), nil
}

type RepoLike interface {
	Clone(ctx context.Context, url, branch string) error
	LsRemote(ctx context.Context, url string) error
	Fetch(ctx context.Context, branch string) (string, error)
	Show(ctx context.Context, commit, path string) ([]byte, error)
	Checkout(ctx context.Context, commit string) error
}

type Env struct {
	Run          sys.Runner
	FS           sys.FS
	Prompt       hostcfg.Prompter
	Interactive  bool
	Host         *hostcfg.Config
	Secrets      *hostcfg.Secrets
	Catalog      kinds.Catalog
	Deployer     *deploy.Deployer
	DeployRoot   string
	StateRoot    string
	RepoFactory  func(dir, keyPath string) RepoLike
	Resolver     registry.Resolver
	PollInterval time.Duration
}

func (e *Env) deployRoot() string {
	if e.DeployRoot != "" {
		return e.DeployRoot
	}
	return "/srv/deploy"
}

func (e *Env) stateRoot() string {
	if e.StateRoot != "" {
		return e.StateRoot
	}
	return "/var/lib/cvctl"
}

func (e *Env) fs() sys.FS {
	if e.FS != nil {
		return e.FS
	}
	return sys.NewOSFS()
}

func (e *Env) runner() sys.Runner {
	if e.Run != nil {
		return e.Run
	}
	return sys.NewExecRunner()
}

func (e *Env) repo(dir, keyPath string) RepoLike {
	if e.RepoFactory != nil {
		return e.RepoFactory(dir, keyPath)
	}
	return &gitrepo.Repo{
		Run:     e.runner(),
		Dir:     dir,
		KeyPath: keyPath,
	}
}

func (e *Env) catalog() (kinds.Catalog, error) {
	if e.Catalog != nil {
		return e.Catalog, nil
	}
	return kinds.Load()
}

type AddOptions struct {
	Project  string
	Env      string
	Branch   string
	TrackTag string
	Strategy string
	Set      map[string]string
}

type AddResult struct {
	Instance     string
	Deploy       deploy.Result
	DeployKeyPub string
	Decisions    *instance.Decisions
}

func removeAll(fsys sys.FS, p string) error {
	entries, err := fsys.ReadDir(p)
	if err == nil {
		for _, entry := range entries {
			if err := removeAll(fsys, filepath.Join(p, entry.Name())); err != nil {
				return err
			}
		}
	}
	return fsys.Remove(p)
}

func DeriveInstanceName(repoURL, project, env string) string {
	if env == "" {
		env = "dev"
	}
	name := project
	if name == "" {
		trimmed := strings.TrimRight(repoURL, "/")
		trimmed = strings.TrimSuffix(trimmed, ".git")
		idx := strings.LastIndexAny(trimmed, "/:")
		if idx >= 0 {
			name = trimmed[idx+1:]
		} else {
			name = trimmed
		}
		name = strings.TrimSuffix(name, "-infra")
	}
	name = strings.ToLower(name)
	return fmt.Sprintf("%s-%s", name, strings.ToLower(env))
}

func Add(ctx context.Context, e *Env, repoURL string, opts AddOptions) (*AddResult, error) {
	if opts.Strategy == "blue-green" {
		return nil, &output.Error{
			Code:    "E_STRATEGY_UNSUPPORTED",
			Message: "blue-green strategy is not supported yet",
			Hint:    "use recreate strategy",
		}
	}
	if opts.Strategy == "" {
		opts.Strategy = "recreate"
	}
	if opts.Strategy != "recreate" {
		return nil, &output.Error{
			Code:    "E_STRATEGY_UNSUPPORTED",
			Message: fmt.Sprintf("strategy %q is not supported", opts.Strategy),
			Hint:    "use recreate strategy",
		}
	}

	envName := opts.Env
	if envName == "" {
		envName = "dev"
	}
	inst := DeriveInstanceName(repoURL, opts.Project, envName)
	instDir := filepath.Join(e.deployRoot(), inst)

	if _, err := e.fs().Stat(instDir); err == nil {
		return nil, &output.Error{
			Code:    "E_PROJECT_EXISTS",
			Message: fmt.Sprintf("instance %q already exists", inst),
			Hint:    fmt.Sprintf("cvctl project update %s", inst),
		}
	}

	isFileRepo := strings.HasPrefix(repoURL, "file://")
	if isFileRepo && os.Getenv("CVCTL_ALLOW_FILE_REPOS") != "1" {
		return nil, &output.Error{
			Code:    "E_PROJECT_FILE_REPO",
			Message: fmt.Sprintf("file:// repo %q is not allowed without CVCTL_ALLOW_FILE_REPOS=1", repoURL),
			Hint:    "export CVCTL_ALLOW_FILE_REPOS=1 or use a git/ssh URL",
		}
	}

	keyPath := ""
	pubKey := ""
	if !isFileRepo {
		keyDir := filepath.Join(e.stateRoot(), inst)
		keyPath = filepath.Join(keyDir, "deploy_key")
		pubPath := keyPath + ".pub"

		if existingPub, err := e.fs().ReadFile(pubPath); err == nil {
			pubKey = string(existingPub)
		} else {
			priv, pub, err := gitrepo.NewDeployKey("cvctl-" + inst)
			if err != nil {
				return nil, err
			}
			if err := e.fs().MkdirAll(keyDir, 0o700, deployOwner); err != nil {
				return nil, err
			}
			if err := e.fs().WriteFile(keyPath, priv, 0o600, deployOwner); err != nil {
				return nil, err
			}
			if err := e.fs().WriteFile(pubPath, pub, 0o644, deployOwner); err != nil {
				return nil, err
			}
			pubKey = string(pub)
		}

		repoForCheck := e.repo(filepath.Join(instDir, "infra"), keyPath)
		if !e.Interactive {
			if err := repoForCheck.LsRemote(ctx, repoURL); err != nil {
				return nil, &output.Error{
					Code:    "E_DEPLOY_KEY_PENDING",
					Message: "deploy key is not yet authorized on remote repo",
					Hint:    fmt.Sprintf("add this key as a read-only deploy key on the repo:\n%s", strings.TrimSpace(pubKey)),
				}
			}
		} else {
			if err := repoForCheck.LsRemote(ctx, repoURL); err != nil {
				cleanRepo := strings.TrimSuffix(repoURL, ".git")
				if e.Prompt != nil {
					ans, err := e.Prompt.Ask(hostcfg.Question{
						Key:    "deploy_key_ready",
						Prompt: fmt.Sprintf("Add it as a read-only deploy key at %s/settings/keys, then press enter", cleanRepo),
						Help:   strings.TrimSpace(pubKey),
					}, "")
					if err != nil {
						return nil, err
					}
					if strings.TrimSpace(strings.ToLower(ans)) == "abort" {
						return nil, &output.Error{
							Code:    "E_DEPLOY_KEY_ABORTED",
							Message: "deploy key authorization aborted",
							Hint:    "add the deploy key to the repo before re-running",
						}
					}
				}
				interval := e.PollInterval
				if interval == 0 {
					interval = 5 * time.Second
				}
				for {
					if err := repoForCheck.LsRemote(ctx, repoURL); err == nil {
						break
					}
					select {
					case <-ctx.Done():
						return nil, ctx.Err()
					case <-time.After(interval):
					}
				}
			}
		}
	}

	cleanupInst := func() {
		_ = removeAll(e.fs(), instDir)
	}

	if err := e.fs().MkdirAll(instDir, 0o2775, deployOwner); err != nil {
		return nil, err
	}

	branch := opts.Branch
	if branch == "" {
		if envName == "prod" {
			branch = "main"
		} else {
			branch = "develop"
		}
	}

	infraDir := filepath.Join(instDir, "infra")
	repo := e.repo(infraDir, keyPath)
	if err := repo.Clone(ctx, repoURL, branch); err != nil {
		cleanupInst()
		return nil, err
	}

	cat, err := e.catalog()
	if err != nil {
		cleanupInst()
		return nil, err
	}

	p, err := contract.Load(infraDir, cat)
	if err != nil {
		cleanupInst()
		return nil, err
	}

	specPath := filepath.Join(infraDir, "env.spec.yml")
	var spec *envspec.Spec
	var extra []output.Issue
	if _, statErr := e.fs().Stat(specPath); statErr != nil {
		extra = append(extra, output.Issue{
			Code:    "E_CONTRACT_NO_ENVSPEC",
			Message: "env.spec.yml not found",
			Hint:    "create env.spec.yml next to compose.yml declaring every variable the compose file references",
		})
	} else if spec, err = envspec.Load(specPath); err != nil {
		var oe *output.Error
		if !errors.As(err, &oe) {
			cleanupInst()
			return nil, err
		}
		extra = append(extra, output.Issue{Code: oe.Code, Message: oe.Message, Hint: oe.Hint})
	}

	issues := append(contract.Validate(p, cat, spec), extra...)
	output.SortIssues(issues)
	if len(issues) > 0 {
		cleanupInst()
		return nil, &output.IssuesError{Issues: issues}
	}

	trackTag := opts.TrackTag
	if trackTag == "" && envName == "dev" {
		trackTag = "develop"
	}

	d := &instance.Decisions{
		Instance:    inst,
		Env:         envName,
		InfraBranch: branch,
		TrackTag:    trackTag,
		InfraPath:   ".",
		Strategy:    opts.Strategy,
		Services:    make(map[string]instance.ServiceDecision),
		Domains:     make(map[string]string),
		Digests:     make(map[string]string),
	}

	for _, ds := range p.Data {
		mode := "local"
		if ds.ExternalAllowed && e.Prompt != nil {
			ans, err := e.Prompt.Ask(hostcfg.Question{
				Key:     "data_" + ds.Name + "_mode",
				Prompt:  fmt.Sprintf("Mode for data service %s (local|external)", ds.Name),
				Options: []string{"local", "external"},
			}, "local")
			if err != nil {
				cleanupInst()
				return nil, err
			}
			if ans != "" {
				mode = ans
			}
		}

		if mode == "local" {
			user := strings.ToLower(contract.Normalize(ds.Name))
			user = strings.ReplaceAll(user, "-", "_")
			user = strings.ReplaceAll(user, ".", "_")
			if !identRe.MatchString(user) {
				cleanupInst()
				return nil, &output.Error{
					Code:    "E_DECISION_INVALID",
					Message: fmt.Sprintf("service %s normalizes to invalid identifier %q", ds.Name, user),
					Hint:    "use characters a-z, 0-9 and _",
				}
			}
			pass, err := randomPassword32()
			if err != nil {
				cleanupInst()
				return nil, err
			}
			d.Services[ds.Name] = instance.ServiceDecision{
				Mode:    "local",
				Version: ds.Version,
				Creds: kinds.Credentials{
					User:     user,
					DB:       user,
					Password: pass,
				},
			}
		} else {
			extMap := make(map[string]string)
			k := cat[ds.Kind]
			for _, field := range k.External {
				fieldLower := strings.ToLower(field)
				isSec := strings.Contains(fieldLower, "password") ||
					strings.Contains(fieldLower, "secret") ||
					strings.Contains(fieldLower, "key") ||
					strings.Contains(fieldLower, "token") ||
					strings.Contains(fieldLower, "url")
				val := ""
				if e.Prompt != nil {
					ans, err := e.Prompt.Ask(hostcfg.Question{
						Key:    "data_" + ds.Name + "_ext_" + field,
						Prompt: fmt.Sprintf("%s external %s", ds.Name, field),
						Secret: isSec,
					}, "")
					if err != nil {
						cleanupInst()
						return nil, err
					}
					val = ans
				}
				extMap[field] = val
			}
			d.Services[ds.Name] = instance.ServiceDecision{
				Mode:     "external",
				External: extMap,
			}
		}
	}

	for _, a := range p.Apps {
		if a.ExposePort > 0 {
			domain := ""
			if e.Prompt != nil {
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:    "domain_" + a.Name,
					Prompt: fmt.Sprintf("Domain for %s", a.Name),
				}, "")
				if err != nil {
					cleanupInst()
					return nil, err
				}
				domain = ans
			}
			d.Domains[a.Name] = domain
		}
	}

	appEnvValues := make(map[string]string)
	for k, v := range opts.Set {
		appEnvValues[k] = v
	}

	if spec != nil {
		names := make([]string, 0, len(spec.Vars))
		for n := range spec.Vars {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, name := range names {
			v := spec.Vars[name]
			if v.Source == "prompt" {
				if _, ok := appEnvValues[name]; ok {
					continue
				}
				def := ""
				if v.Default != nil {
					def = *v.Default
				}
				if e.Prompt != nil {
					ans, err := e.Prompt.Ask(hostcfg.Question{
						Key:    "env_" + name,
						Prompt: fmt.Sprintf("Value for %s", name),
						Secret: v.Secret,
					}, def)
					if err != nil {
						cleanupInst()
						return nil, err
					}
					appEnvValues[name] = ans
				} else if def != "" {
					appEnvValues[name] = def
				}
			}
		}
	}

	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	appEnvPath := filepath.Join(instDir, "app.env")

	if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(d.Entries()), 0o640, deployOwner); err != nil {
		cleanupInst()
		return nil, err
	}
	if err := e.fs().WriteFile(appEnvPath, instance.FormatEnv(appEnvValues), 0o640, deployOwner); err != nil {
		cleanupInst()
		return nil, err
	}

	var deployRes deploy.Result
	if e.Deployer != nil {
		if e.Deployer.Repo == nil {
			e.Deployer.Repo = repo
		}
		var allowed []string
		if e.Host != nil && e.Host.Registries != "" {
			for _, r := range strings.Split(e.Host.Registries, ",") {
				if s := strings.TrimSpace(r); s != "" {
					allowed = append(allowed, s)
				}
			}
		}
		src := deploy.SourceFor(d, deploy.Deps{
			Repo:     repo,
			Resolver: e.Resolver,
			Allowed:  allowed,
		})
		desired, err := src.Desired(ctx, d, p)
		if err != nil {
			return nil, err
		}
		deployRes, err = e.Deployer.Deploy(ctx, d.Instance, desired, deploy.Options{Actor: "cli:project-add"})
		if err != nil {
			return nil, err
		}
	} else {
		deployRes = deploy.Result{Status: state.StatusOK}
	}

	return &AddResult{
		Instance:     d.Instance,
		Deploy:       deployRes,
		DeployKeyPub: pubKey,
		Decisions:    d,
	}, nil
}

func Update(ctx context.Context, e *Env, inst string) (*AddResult, error) {
	instDir := filepath.Join(e.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")

	if _, err := e.fs().Stat(cvctlEnvPath); err != nil {
		return nil, &output.Error{
			Code:    "E_PROJECT_NOT_FOUND",
			Message: fmt.Sprintf("instance %q not found", inst),
			Hint:    fmt.Sprintf("run cvctl project add <repo-url> --name %s", inst),
		}
	}

	keyPath := filepath.Join(e.stateRoot(), inst, "deploy_key")
	pubKey := ""
	if pub, err := e.fs().ReadFile(keyPath + ".pub"); err == nil {
		pubKey = string(pub)
	}
	if _, err := e.fs().Stat(keyPath); err != nil {
		keyPath = ""
	}

	infraDir := filepath.Join(instDir, "infra")
	repo := e.repo(infraDir, keyPath)

	cat, err := e.catalog()
	if err != nil {
		return nil, err
	}

	p, err := contract.Load(infraDir, cat)
	if err != nil {
		return nil, err
	}

	specPath := filepath.Join(infraDir, "env.spec.yml")
	var spec *envspec.Spec
	var extra []output.Issue
	if _, statErr := e.fs().Stat(specPath); statErr != nil {
		extra = append(extra, output.Issue{
			Code:    "E_CONTRACT_NO_ENVSPEC",
			Message: "env.spec.yml not found",
			Hint:    "create env.spec.yml next to compose.yml declaring every variable the compose file references",
		})
	} else if spec, err = envspec.Load(specPath); err != nil {
		var oe *output.Error
		if !errors.As(err, &oe) {
			return nil, err
		}
		extra = append(extra, output.Issue{Code: oe.Code, Message: oe.Message, Hint: oe.Hint})
	}

	issues := append(contract.Validate(p, cat, spec), extra...)
	output.SortIssues(issues)
	if len(issues) > 0 {
		return nil, &output.IssuesError{Issues: issues}
	}

	d, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return nil, err
	}

	appEnvPath := filepath.Join(instDir, "app.env")
	appEnvValues := make(map[string]string)
	if data, err := e.fs().ReadFile(appEnvPath); err == nil {
		if parsed, pErr := godotenv.Unmarshal(string(data)); pErr == nil {
			appEnvValues = parsed
		}
	}

	for _, ds := range p.Data {
		sd, ok := d.Services[ds.Name]
		if !ok || sd.Mode == "" {
			mode := "local"
			if ds.ExternalAllowed && e.Prompt != nil {
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:     "data_" + ds.Name + "_mode",
					Prompt:  fmt.Sprintf("Mode for data service %s (local|external)", ds.Name),
					Options: []string{"local", "external"},
				}, "local")
				if err != nil {
					return nil, err
				}
				if ans != "" {
					mode = ans
				}
			}

			if mode == "local" {
				user := strings.ToLower(contract.Normalize(ds.Name))
				user = strings.ReplaceAll(user, "-", "_")
				user = strings.ReplaceAll(user, ".", "_")
				if !identRe.MatchString(user) {
					return nil, &output.Error{
						Code:    "E_DECISION_INVALID",
						Message: fmt.Sprintf("service %s normalizes to invalid identifier %q", ds.Name, user),
						Hint:    "use characters a-z, 0-9 and _",
					}
				}
				pass, err := randomPassword32()
				if err != nil {
					return nil, err
				}
				d.Services[ds.Name] = instance.ServiceDecision{
					Mode:    "local",
					Version: ds.Version,
					Creds: kinds.Credentials{
						User:     user,
						DB:       user,
						Password: pass,
					},
				}
			} else {
				extMap := make(map[string]string)
				k := cat[ds.Kind]
				for _, field := range k.External {
					fieldLower := strings.ToLower(field)
					isSec := strings.Contains(fieldLower, "password") ||
						strings.Contains(fieldLower, "secret") ||
						strings.Contains(fieldLower, "key") ||
						strings.Contains(fieldLower, "token") ||
						strings.Contains(fieldLower, "url")
					val := ""
					if e.Prompt != nil {
						ans, err := e.Prompt.Ask(hostcfg.Question{
							Key:    "data_" + ds.Name + "_ext_" + field,
							Prompt: fmt.Sprintf("%s external %s", ds.Name, field),
							Secret: isSec,
						}, "")
						if err != nil {
							return nil, err
						}
						val = ans
					}
					extMap[field] = val
				}
				d.Services[ds.Name] = instance.ServiceDecision{
					Mode:     "external",
					External: extMap,
				}
			}
		}
	}

	for _, a := range p.Apps {
		if a.ExposePort > 0 && d.Domains[a.Name] == "" {
			domain := ""
			if e.Prompt != nil {
				ans, err := e.Prompt.Ask(hostcfg.Question{
					Key:    "domain_" + a.Name,
					Prompt: fmt.Sprintf("Domain for %s", a.Name),
				}, "")
				if err != nil {
					return nil, err
				}
				domain = ans
			}
			d.Domains[a.Name] = domain
		}
	}

	if spec != nil {
		names := make([]string, 0, len(spec.Vars))
		for n := range spec.Vars {
			names = append(names, n)
		}
		sort.Strings(names)

		for _, name := range names {
			v := spec.Vars[name]
			if v.Source == "prompt" {
				if _, ok := appEnvValues[name]; ok {
					continue
				}
				def := ""
				if v.Default != nil {
					def = *v.Default
				}
				if e.Prompt != nil {
					ans, err := e.Prompt.Ask(hostcfg.Question{
						Key:    "env_" + name,
						Prompt: fmt.Sprintf("Value for %s", name),
						Secret: v.Secret,
					}, def)
					if err != nil {
						return nil, err
					}
					appEnvValues[name] = ans
				} else if def != "" {
					appEnvValues[name] = def
				}
			}
		}
	}

	if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(d.Entries()), 0o640, deployOwner); err != nil {
		return nil, err
	}
	if err := e.fs().WriteFile(appEnvPath, instance.FormatEnv(appEnvValues), 0o640, deployOwner); err != nil {
		return nil, err
	}

	var deployRes deploy.Result
	if e.Deployer != nil {
		if e.Deployer.Repo == nil {
			e.Deployer.Repo = repo
		}
		var allowed []string
		if e.Host != nil && e.Host.Registries != "" {
			for _, r := range strings.Split(e.Host.Registries, ",") {
				if s := strings.TrimSpace(r); s != "" {
					allowed = append(allowed, s)
				}
			}
		}
		src := deploy.SourceFor(d, deploy.Deps{
			Repo:     repo,
			Resolver: e.Resolver,
			Allowed:  allowed,
		})
		desired, err := src.Desired(ctx, d, p)
		if err != nil {
			return nil, err
		}
		deployRes, err = e.Deployer.Deploy(ctx, d.Instance, desired, deploy.Options{Actor: "cli:project-update"})
		if err != nil {
			return nil, err
		}
	} else {
		deployRes = deploy.Result{Status: state.StatusOK}
	}

	return &AddResult{
		Instance:     d.Instance,
		Deploy:       deployRes,
		DeployKeyPub: pubKey,
		Decisions:    d,
	}, nil
}
