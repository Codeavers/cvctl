package deploy

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
)

type fakeRepo struct {
	fetchFunc func(ctx context.Context, branch string) (string, error)
	showFunc  func(ctx context.Context, commit, path string) ([]byte, error)
	fetched   []string
	showed    [][2]string
}

func (f *fakeRepo) Fetch(ctx context.Context, branch string) (string, error) {
	f.fetched = append(f.fetched, branch)
	if f.fetchFunc != nil {
		return f.fetchFunc(ctx, branch)
	}
	return "0123456789abcdef", nil
}

func (f *fakeRepo) Show(ctx context.Context, commit, path string) ([]byte, error) {
	f.showed = append(f.showed, [2]string{commit, path})
	if f.showFunc != nil {
		return f.showFunc(ctx, commit, path)
	}
	return nil, nil
}

type fakeResolver struct {
	digestFunc func(ctx context.Context, image, tag string) (string, error)
	calls      [][2]string
}

func (f *fakeResolver) Digest(ctx context.Context, image, tag string) (string, error) {
	f.calls = append(f.calls, [2]string{image, tag})
	if f.digestFunc != nil {
		return f.digestFunc(ctx, image, tag)
	}
	return "sha256:0000000000000000000000000000000000000000000000000000000000000000", nil
}

func TestDevResolvesTags(t *testing.T) {
	repo := &fakeRepo{
		fetchFunc: func(ctx context.Context, branch string) (string, error) {
			if branch != "feature-branch" {
				t.Fatalf("expected branch feature-branch, got %s", branch)
			}
			return "commit1234567890abcdef", nil
		},
	}
	resolver := &fakeResolver{
		digestFunc: func(ctx context.Context, image, tag string) (string, error) {
			switch image {
			case "ghcr.io/codeavers/api":
				return "sha256:1111111111111111111111111111111111111111111111111111111111111111", nil
			case "ghcr.io/codeavers/web":
				return "sha256:2222222222222222222222222222222222222222222222222222222222222222", nil
			default:
				t.Fatalf("unexpected image %s", image)
				return "", nil
			}
		},
	}

	src := &DevSource{
		Repo:     repo,
		Resolver: resolver,
		Allowed:  []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{
		Env:         "dev",
		InfraBranch: "feature-branch",
		TrackTag:    "edge",
	}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
			{Name: "web", Image: "ghcr.io/codeavers/web"},
		},
	}

	desired, err := src.Desired(context.Background(), d, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if desired.InfraCommit != "commit1234567890abcdef" {
		t.Fatalf("expected commit commit1234567890abcdef, got %s", desired.InfraCommit)
	}
	if len(desired.Digests) != 2 {
		t.Fatalf("expected 2 digests, got %d", len(desired.Digests))
	}
	if desired.Digests["api"] != "sha256:1111111111111111111111111111111111111111111111111111111111111111" {
		t.Fatalf("unexpected api digest: %s", desired.Digests["api"])
	}
	if desired.Digests["web"] != "sha256:2222222222222222222222222222222222222222222222222222222222222222" {
		t.Fatalf("unexpected web digest: %s", desired.Digests["web"])
	}

	if len(resolver.calls) != 2 {
		t.Fatalf("expected 2 resolver calls, got %d", len(resolver.calls))
	}
	if resolver.calls[0][1] != "edge" || resolver.calls[1][1] != "edge" {
		t.Fatalf("expected tag edge in resolver calls, got %v", resolver.calls)
	}
}

func TestDevRejectsDisallowedRegistry(t *testing.T) {
	repo := &fakeRepo{}
	resolver := &fakeResolver{}

	src := &DevSource{
		Repo:     repo,
		Resolver: resolver,
		Allowed:  []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{
		Env:      "dev",
		TrackTag: "develop",
	}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "docker.io/untrusted/api"},
		},
	}

	_, err := src.Desired(context.Background(), d, p)
	if err == nil {
		t.Fatal("expected error for disallowed registry, got nil")
	}

	var outErr *output.Error
	if !errors.As(err, &outErr) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_REGISTRY_NOT_ALLOWED" {
		t.Fatalf("expected code E_REGISTRY_NOT_ALLOWED, got %s", outErr.Code)
	}

	if len(resolver.calls) != 0 {
		t.Fatalf("expected 0 resolver calls before resolving, got %d", len(resolver.calls))
	}
}

func TestProdReadsPinnedDigests(t *testing.T) {
	const commitSHA = "a1b2c3d4e5f67890abcdef1234567890abcdef12"
	prodEnvContent := `
# Production digests
CVCTL_DIGEST_API=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
CVCTL_DIGEST_WEB_FRONTEND=sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
CVCTL_DIGEST_EXTRA_UNKNOWN=sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc
UNRELATED_VAR=ignored
`

	repo := &fakeRepo{
		fetchFunc: func(ctx context.Context, branch string) (string, error) {
			if branch != "main" {
				t.Fatalf("expected branch main, got %s", branch)
			}
			return commitSHA, nil
		},
		showFunc: func(ctx context.Context, commit, path string) ([]byte, error) {
			if commit != commitSHA {
				t.Fatalf("expected commit %s, got %s", commitSHA, commit)
			}
			if path != "deploy/prod.env" {
				t.Fatalf("expected path deploy/prod.env, got %s", path)
			}
			return []byte(prodEnvContent), nil
		},
	}

	src := &ProdSource{
		Repo:    repo,
		Allowed: []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{
		Env:         "prod",
		InfraBranch: "main",
	}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
			{Name: "web-frontend", Image: "ghcr.io/codeavers/web-frontend"},
		},
	}

	desired, err := src.Desired(context.Background(), d, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if desired.InfraCommit != commitSHA {
		t.Fatalf("expected commit %s, got %s", commitSHA, desired.InfraCommit)
	}
	if len(desired.Digests) != 2 {
		t.Fatalf("expected 2 digests (extra unknown ignored), got %d", len(desired.Digests))
	}
	if desired.Digests["api"] != "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("unexpected api digest: %s", desired.Digests["api"])
	}
	if desired.Digests["web-frontend"] != "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" {
		t.Fatalf("unexpected web-frontend digest: %s", desired.Digests["web-frontend"])
	}
	if _, ok := desired.Digests["EXTRA_UNKNOWN"]; ok {
		t.Fatal("expected extra keys to be ignored")
	}
}

func TestProdMissingDigest(t *testing.T) {
	const commitSHA = "c1a2b3c4d5e6f7890abcdef"
	prodEnvContent := `
CVCTL_DIGEST_API=sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
`

	repo := &fakeRepo{
		fetchFunc: func(ctx context.Context, branch string) (string, error) {
			return commitSHA, nil
		},
		showFunc: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(prodEnvContent), nil
		},
	}

	src := &ProdSource{
		Repo:    repo,
		Allowed: []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{
		Env: "prod",
	}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
			{Name: "worker-service", Image: "ghcr.io/codeavers/worker"},
		},
	}

	_, err := src.Desired(context.Background(), d, p)
	if err == nil {
		t.Fatal("expected error for missing digest, got nil")
	}

	var outErr *output.Error
	if !errors.As(err, &outErr) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_PROD_DIGEST_MISSING" {
		t.Fatalf("expected code E_PROD_DIGEST_MISSING, got %s", outErr.Code)
	}
	if !strings.Contains(outErr.Message, "worker-service") {
		t.Fatalf("expected message to name service worker-service, got: %s", outErr.Message)
	}
	if !strings.Contains(outErr.Message, "deploy/prod.env at c1a2b3c") {
		t.Fatalf("expected message to contain 'deploy/prod.env at c1a2b3c', got: %s", outErr.Message)
	}
	if !strings.Contains(outErr.Hint, "add CVCTL_DIGEST_WORKER_SERVICE=sha256:... to deploy/prod.env in the infra repo") {
		t.Fatalf("expected hint with normalized name, got: %s", outErr.Hint)
	}
}

func TestProdIgnoresTags(t *testing.T) {
	const commitSHA = "9876543210fedcba"
	prodEnvContent := `
CVCTL_DIGEST_API=sha256:4444444444444444444444444444444444444444444444444444444444444444
`

	repo := &fakeRepo{
		fetchFunc: func(ctx context.Context, branch string) (string, error) {
			return commitSHA, nil
		},
		showFunc: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(prodEnvContent), nil
		},
	}

	src := &ProdSource{
		Repo:    repo,
		Allowed: []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{
		Env:      "prod",
		TrackTag: "v2.0.0-beta",
	}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
		},
	}

	desired, err := src.Desired(context.Background(), d, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if desired.Digests["api"] != "sha256:4444444444444444444444444444444444444444444444444444444444444444" {
		t.Fatalf("unexpected api digest: %s", desired.Digests["api"])
	}
}

func TestProdDigestInvalid(t *testing.T) {
	prodEnvContent := `
CVCTL_DIGEST_API=not-a-valid-sha256
`
	repo := &fakeRepo{
		showFunc: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte(prodEnvContent), nil
		},
	}

	src := &ProdSource{
		Repo:    repo,
		Allowed: []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{Env: "prod"}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
		},
	}

	_, err := src.Desired(context.Background(), d, p)
	if err == nil {
		t.Fatal("expected error for invalid digest, got nil")
	}

	var outErr *output.Error
	if !errors.As(err, &outErr) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_PROD_DIGEST_INVALID" {
		t.Fatalf("expected code E_PROD_DIGEST_INVALID, got %s", outErr.Code)
	}
	if !strings.Contains(outErr.Message, "CVCTL_DIGEST_API") {
		t.Fatalf("expected message to name key CVCTL_DIGEST_API, got: %s", outErr.Message)
	}
}

func TestProdRejectsDisallowedRegistry(t *testing.T) {
	src := &ProdSource{
		Repo:    &fakeRepo{},
		Allowed: []string{"ghcr.io/codeavers"},
	}
	d := &instance.Decisions{Env: "prod"}
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "docker.io/untrusted/api"},
		},
	}

	_, err := src.Desired(context.Background(), d, p)
	if err == nil {
		t.Fatal("expected error for disallowed registry in prod, got nil")
	}
	var outErr *output.Error
	if !errors.As(err, &outErr) || outErr.Code != "E_REGISTRY_NOT_ALLOWED" {
		t.Fatalf("expected E_REGISTRY_NOT_ALLOWED, got: %v", err)
	}
}

func TestDevDefaults(t *testing.T) {
	repo := &fakeRepo{}
	resolver := &fakeResolver{}
	src := &DevSource{
		Repo:     repo,
		Resolver: resolver,
		Allowed:  []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{Env: "dev"} // InfraBranch and TrackTag empty
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
		},
	}

	_, err := src.Desired(context.Background(), d, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(repo.fetched) != 1 || repo.fetched[0] != "develop" {
		t.Fatalf("expected default branch 'develop', got %v", repo.fetched)
	}
	if len(resolver.calls) != 1 || resolver.calls[0][1] != "develop" {
		t.Fatalf("expected default tag 'develop', got %v", resolver.calls)
	}
}

func TestProdDefaults(t *testing.T) {
	repo := &fakeRepo{
		showFunc: func(ctx context.Context, commit, path string) ([]byte, error) {
			return []byte("CVCTL_DIGEST_API=sha256:0000000000000000000000000000000000000000000000000000000000000000"), nil
		},
	}
	src := &ProdSource{
		Repo:    repo,
		Allowed: []string{"ghcr.io/codeavers"},
	}

	d := &instance.Decisions{Env: "prod"} // InfraBranch empty
	p := &contract.Project{
		Apps: []contract.AppService{
			{Name: "api", Image: "ghcr.io/codeavers/api"},
		},
	}

	_, err := src.Desired(context.Background(), d, p)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(repo.fetched) != 1 || repo.fetched[0] != "main" {
		t.Fatalf("expected default branch 'main', got %v", repo.fetched)
	}
}

func TestSourceFor(t *testing.T) {
	repo := &fakeRepo{}
	resolver := &fakeResolver{}
	deps := Deps{
		Repo:     repo,
		Resolver: resolver,
		Allowed:  []string{"ghcr.io/codeavers"},
	}

	devSrc := SourceFor(&instance.Decisions{Env: "dev"}, deps)
	if _, ok := devSrc.(*DevSource); !ok {
		t.Fatalf("expected *DevSource, got %T", devSrc)
	}

	prodSrc := SourceFor(&instance.Decisions{Env: "prod"}, deps)
	if _, ok := prodSrc.(*ProdSource); !ok {
		t.Fatalf("expected *ProdSource, got %T", prodSrc)
	}

	unknownSrc := SourceFor(&instance.Decisions{Env: "staging"}, deps)
	_, err := unknownSrc.Desired(context.Background(), &instance.Decisions{Env: "staging"}, nil)
	if err == nil {
		t.Fatal("expected error for unknown env, got nil")
	}
	var outErr *output.Error
	if !errors.As(err, &outErr) || outErr.Code != "E_DECISION_INVALID" {
		t.Fatalf("expected E_DECISION_INVALID, got %v", err)
	}

	nilSrc := SourceFor(nil, deps)
	_, err = nilSrc.Desired(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("expected error for nil decisions, got nil")
	}
	if !errors.As(err, &outErr) || outErr.Code != "E_DECISION_INVALID" {
		t.Fatalf("expected E_DECISION_INVALID, got %v", err)
	}
}
