// Package deploy resolves desired state and coordinates deployment pipelines.
package deploy

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// RepoAPI defines git operations required to fetch refs and read committed files.
type RepoAPI interface {
	Fetch(ctx context.Context, branch string) (string, error)
	Show(ctx context.Context, commit, path string) ([]byte, error)
}

// Source resolves desired state for an instance deployment. The contract used
// for registry checks and digest resolution is read from the fetched commit.
type Source interface {
	Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error)
}

// Deps encapsulates external dependencies needed to construct a Source.
type Deps struct {
	Repo     RepoAPI
	Resolver registry.Resolver
	Allowed  []string
	Catalog  kinds.Catalog
	// OCI authenticates contract artifact pulls of OCI sources.
	OCI infraoci.Auth
	// Cache holds the instance's validated contracts; nil disables it.
	Cache *ContractCache
}

var (
	DefaultDeployRoot = "/srv/deploy"
	DefaultStateRoot  = "/var/lib/cvctl"
)

// SourceDeps builds Deps for an instance using the host configuration, secrets, and repository checkout.
func SourceDeps(inst string, run sys.Runner, host *hostcfg.Config, secrets *hostcfg.Secrets) Deps {
	return SourceDepsWithRoots(inst, run, host, secrets, DefaultDeployRoot, DefaultStateRoot, nil)
}

// SourceDepsWithRoots builds Deps allowing custom deployRoot, stateRoot, and filesystem.
func SourceDepsWithRoots(inst string, run sys.Runner, host *hostcfg.Config, secrets *hostcfg.Secrets, deployRoot, stateRoot string, fsys sys.FS) Deps {
	if deployRoot == "" {
		deployRoot = DefaultDeployRoot
	}
	if stateRoot == "" {
		stateRoot = DefaultStateRoot
	}
	keyPath := filepath.Join(stateRoot, inst, "deploy_key")
	exists := false
	if fsys != nil {
		if fi, err := fsys.Stat(keyPath); err == nil && !fi.IsDir {
			exists = true
		}
	} else {
		if fi, err := os.Stat(keyPath); err == nil && !fi.IsDir() {
			exists = true
		}
	}
	if !exists {
		keyPath = ""
	}
	repo := &gitrepo.Repo{
		Run:     run,
		Dir:     filepath.Join(deployRoot, inst, "infra"),
		KeyPath: keyPath,
	}

	var user string
	if host != nil {
		user = host.RegistryUser
	}
	var token string
	if secrets != nil {
		token, _ = secrets.Get("registry_token")
	}
	resolver := registry.New(user, token, host.TokenHost(), registry.InsecureHostsFromEnv())

	return Deps{
		Repo:     repo,
		Resolver: resolver,
		Allowed:  AllowedRegistries(host),
		OCI:      OCIAuth(host, secrets),
		Cache:    NewContractCache(fsys, stateRoot, inst),
	}
}

// AllowedRegistries returns the registry prefixes permitted by the host
// configuration. The result is never nil: an empty list rejects every image.
func AllowedRegistries(host *hostcfg.Config) []string {
	allowed := []string{}
	if host != nil && host.Registries != "" {
		for _, r := range strings.Split(host.Registries, ",") {
			if s := strings.TrimSpace(r); s != "" {
				allowed = append(allowed, s)
			}
		}
	}
	return allowed
}

// contractAt loads the compose contract as committed at commit, so desired
// state never depends on the working tree left by a previous deploy.
func contractAt(ctx context.Context, repo RepoAPI, commit string, d *instance.Decisions, cat kinds.Catalog) (*contract.Project, error) {
	if cat == nil {
		var err error
		if cat, err = kinds.Load(); err != nil {
			return nil, err
		}
	}
	infraPath := "."
	if d != nil && d.InfraPath != "" {
		infraPath = d.InfraPath
	}
	data, err := repo.Show(ctx, commit, path.Join(infraPath, "compose.yml"))
	if err != nil {
		return nil, err
	}
	dir, cleanup, err := sys.MkdirTemp("cvctl-contract-")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	if err := sys.NewOSFS().WriteFile(filepath.Join(dir, "compose.yml"), data, 0o600, sys.Owner{}); err != nil {
		return nil, err
	}
	return contract.Load(dir, cat)
}

// DevSource resolves desired state for dev environments by tracking branch refs and image tags.
type DevSource struct {
	Repo     RepoAPI
	Resolver registry.Resolver
	Allowed  []string
	Catalog  kinds.Catalog
}

// Desired resolves tag digests for dev environments.
func (s *DevSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	branch := "develop"
	tag := "develop"
	if d != nil {
		if d.InfraBranch != "" {
			branch = d.InfraBranch
		}
		if d.TrackTag != "" {
			tag = d.TrackTag
		}
	}

	commit, err := s.Repo.Fetch(ctx, branch)
	if err != nil {
		return state.Desired{}, err
	}

	p, err := contractAt(ctx, s.Repo, commit, d, s.Catalog)
	if err != nil {
		return state.Desired{}, err
	}
	apps := p.Apps

	// Allowed registries must be validated before making network calls to resolve digests.
	for _, a := range apps {
		if err := registry.Allowed(a.Image, s.Allowed); err != nil {
			return state.Desired{}, err
		}
	}

	digests := make(map[string]string, len(apps))
	for _, a := range apps {
		digest, err := s.Resolver.Digest(ctx, a.Image, tag)
		if err != nil {
			return state.Desired{}, err
		}
		digests[a.Name] = digest
	}

	return state.Desired{
		InfraCommit: commit,
		Digests:     digests,
	}, nil
}

// ProdSource resolves desired state for production environments using pinned digests in deploy/prod.env.
type ProdSource struct {
	Repo    RepoAPI
	Allowed []string
	Catalog kinds.Catalog
}

// Desired reads pinned digests from deploy/prod.env at the fetched commit.
func (s *ProdSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	branch := "main"
	if d != nil && d.InfraBranch != "" {
		branch = d.InfraBranch
	}

	commit, err := s.Repo.Fetch(ctx, branch)
	if err != nil {
		return state.Desired{}, err
	}

	p, err := contractAt(ctx, s.Repo, commit, d, s.Catalog)
	if err != nil {
		return state.Desired{}, err
	}
	apps := p.Apps

	for _, a := range apps {
		if err := registry.Allowed(a.Image, s.Allowed); err != nil {
			return state.Desired{}, err
		}
	}

	data, err := s.Repo.Show(ctx, commit, "deploy/prod.env")
	if err != nil {
		return state.Desired{}, err
	}
	digests, err := prodDigests(data, p, "deploy/prod.env at "+commitShort(commit), "deploy/prod.env")
	if err != nil {
		return state.Desired{}, err
	}

	return state.Desired{
		InfraCommit: commit,
		Digests:     digests,
	}, nil
}

// prodDigests reads CVCTL_DIGEST_<N> for every app of p from a prod.env
// file; where names it in errors and file in hints.
func prodDigests(data []byte, p *contract.Project, where, file string) (map[string]string, error) {
	envMap, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, &output.Error{
			Code:    "E_PROD_DIGEST_INVALID",
			Message: fmt.Sprintf("cannot parse %s: %v", where, err),
			Hint:    fmt.Sprintf("check %s syntax in the infra repo", file),
		}
	}
	digests := make(map[string]string, len(p.Apps))
	for _, a := range p.Apps {
		key := "CVCTL_DIGEST_" + contract.Normalize(a.Name)
		val, ok := envMap[key]
		if !ok || strings.TrimSpace(val) == "" {
			return nil, &output.Error{
				Code:    "E_PROD_DIGEST_MISSING",
				Message: fmt.Sprintf("service %q missing digest in %s", a.Name, where),
				Hint:    fmt.Sprintf("add %s=sha256:... to %s in the infra repo", key, file),
			}
		}
		val = strings.TrimSpace(val)
		if !digestRe.MatchString(val) {
			return nil, &output.Error{
				Code:    "E_PROD_DIGEST_INVALID",
				Message: fmt.Sprintf("invalid digest for key %s: %q does not match sha256:<64 hex>", key, val),
				Hint:    fmt.Sprintf("set %s to sha256:<64 hex> in %s", key, file),
			}
		}
		digests[a.Name] = val
	}
	return digests, nil
}

// SourceFor selects the appropriate Source for decisions d.
func SourceFor(d *instance.Decisions, deps Deps) Source {
	if d == nil {
		return invalidSource{env: ""}
	}
	if d.IsOCI() {
		switch d.Env {
		case "dev":
			return &OCIDevSource{Auth: deps.OCI, Resolver: deps.Resolver, Allowed: deps.Allowed, Catalog: deps.Catalog, Cache: deps.Cache}
		case "prod":
			return &OCIProdSource{Auth: deps.OCI, Allowed: deps.Allowed, Catalog: deps.Catalog, Cache: deps.Cache}
		default:
			return invalidSource{env: d.Env}
		}
	}
	switch d.Env {
	case "dev":
		return &DevSource{
			Repo:     deps.Repo,
			Resolver: deps.Resolver,
			Allowed:  deps.Allowed,
			Catalog:  deps.Catalog,
		}
	case "prod":
		return &ProdSource{
			Repo:    deps.Repo,
			Allowed: deps.Allowed,
			Catalog: deps.Catalog,
		}
	default:
		return invalidSource{env: d.Env}
	}
}

type invalidSource struct {
	env string
}

func (s invalidSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	env := s.env
	if env == "" && d != nil {
		env = d.Env
	}
	return state.Desired{}, &output.Error{
		Code:    "E_DECISION_INVALID",
		Message: fmt.Sprintf("env %q is not valid", env),
		Hint:    "use dev or prod",
	}
}

func commitShort(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return commit
}
