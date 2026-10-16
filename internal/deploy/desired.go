// Package deploy resolves desired state and coordinates deployment pipelines.
package deploy

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// RepoAPI defines git operations required to fetch refs and read committed files.
type RepoAPI interface {
	Fetch(ctx context.Context, branch string) (string, error)
	Show(ctx context.Context, commit, path string) ([]byte, error)
}

// Source resolves desired state for an instance deployment.
type Source interface {
	Desired(ctx context.Context, d *instance.Decisions, p *contract.Project) (state.Desired, error)
}

// Deps encapsulates external dependencies needed to construct a Source.
type Deps struct {
	Repo     RepoAPI
	Resolver registry.Resolver
	Allowed  []string
}

// DevSource resolves desired state for dev environments by tracking branch refs and image tags.
type DevSource struct {
	Repo     RepoAPI
	Resolver registry.Resolver
	Allowed  []string
}

// Desired resolves tag digests for dev environments.
func (s *DevSource) Desired(ctx context.Context, d *instance.Decisions, p *contract.Project) (state.Desired, error) {
	var apps []contract.AppService
	if p != nil {
		apps = p.Apps
	}

	// Allowed registries must be validated before making network calls to resolve digests.
	for _, a := range apps {
		if err := registry.Allowed(a.Image, s.Allowed); err != nil {
			return state.Desired{}, err
		}
	}

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
}

// Desired reads pinned digests from deploy/prod.env at the fetched commit.
func (s *ProdSource) Desired(ctx context.Context, d *instance.Decisions, p *contract.Project) (state.Desired, error) {
	var apps []contract.AppService
	if p != nil {
		apps = p.Apps
	}

	for _, a := range apps {
		if err := registry.Allowed(a.Image, s.Allowed); err != nil {
			return state.Desired{}, err
		}
	}

	branch := "main"
	if d != nil && d.InfraBranch != "" {
		branch = d.InfraBranch
	}

	commit, err := s.Repo.Fetch(ctx, branch)
	if err != nil {
		return state.Desired{}, err
	}

	data, err := s.Repo.Show(ctx, commit, "deploy/prod.env")
	if err != nil {
		return state.Desired{}, err
	}

	envMap, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return state.Desired{}, &output.Error{
			Code:    "E_PROD_DIGEST_INVALID",
			Message: fmt.Sprintf("cannot parse deploy/prod.env at %s: %v", commitShort(commit), err),
			Hint:    "check deploy/prod.env syntax in the infra repo",
		}
	}

	commit7 := commitShort(commit)
	digests := make(map[string]string, len(apps))
	for _, a := range apps {
		key := "CVCTL_DIGEST_" + contract.Normalize(a.Name)
		val, ok := envMap[key]
		if !ok || strings.TrimSpace(val) == "" {
			return state.Desired{}, &output.Error{
				Code:    "E_PROD_DIGEST_MISSING",
				Message: fmt.Sprintf("service %q missing digest in deploy/prod.env at %s", a.Name, commit7),
				Hint:    fmt.Sprintf("add %s=sha256:... to deploy/prod.env in the infra repo", key),
			}
		}
		val = strings.TrimSpace(val)
		if !digestRe.MatchString(val) {
			return state.Desired{}, &output.Error{
				Code:    "E_PROD_DIGEST_INVALID",
				Message: fmt.Sprintf("invalid digest for key %s: %q does not match sha256:<64 hex>", key, val),
				Hint:    fmt.Sprintf("set %s to sha256:<64 hex> in deploy/prod.env", key),
			}
		}
		digests[a.Name] = val
	}

	return state.Desired{
		InfraCommit: commit,
		Digests:     digests,
	}, nil
}

// SourceFor selects the appropriate Source for decisions d.
func SourceFor(d *instance.Decisions, deps Deps) Source {
	if d == nil {
		return invalidSource{env: ""}
	}
	switch d.Env {
	case "dev":
		return &DevSource{
			Repo:     deps.Repo,
			Resolver: deps.Resolver,
			Allowed:  deps.Allowed,
		}
	case "prod":
		return &ProdSource{
			Repo:    deps.Repo,
			Allowed: deps.Allowed,
		}
	default:
		return invalidSource{env: d.Env}
	}
}

type invalidSource struct {
	env string
}

func (s invalidSource) Desired(ctx context.Context, d *instance.Decisions, p *contract.Project) (state.Desired, error) {
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
