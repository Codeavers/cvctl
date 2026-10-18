package deploy

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

// ContractFetcher pulls the contract artifact ref@digest.
type ContractFetcher func(ctx context.Context, ref, digest string) (*infraoci.Contract, error)

// OCIAuth builds the contract registry credentials from the host config; the
// token goes only to the host's token host.
func OCIAuth(host *hostcfg.Config, secrets *hostcfg.Secrets) infraoci.Auth {
	a := infraoci.Auth{Insecure: registry.InsecureHostsFromEnv()}
	if host != nil {
		a.User = host.RegistryUser
		a.TokenHost = host.TokenHost()
	}
	if secrets != nil {
		a.Token, _ = secrets.Get("registry_token")
	}
	return a
}

// OCIFetcher returns a ContractFetcher using a.
func OCIFetcher(a infraoci.Auth) ContractFetcher {
	return func(ctx context.Context, ref, digest string) (*infraoci.Contract, error) {
		return infraoci.Fetch(ctx, a, ref, digest)
	}
}

// RevShort names the infra revision of a desired state: the short commit, or
// the short contract digest for an OCI source.
func RevShort(t state.Desired) string {
	if t.InfraDigest != "" {
		h := strings.TrimPrefix(t.InfraDigest, "sha256:")
		if len(h) > 12 {
			h = h[:12]
		}
		return "sha256:" + h
	}
	return commitShort(t.InfraCommit)
}

func revWord(t state.Desired) string {
	if t.InfraDigest != "" {
		return "contract"
	}
	return "commit"
}

// hasInfra reports whether t names an infra revision that can be restored.
func hasInfra(t *state.Desired) bool {
	return t != nil && (t.InfraCommit != "" || t.InfraDigest != "")
}

// checkout puts the infra revision of t into <inst>/infra: a git checkout,
// or for an OCI source the artifact fetched by digest and extracted.
func (d *Deployer) checkout(ctx context.Context, inst string, t state.Desired) error {
	if t.InfraDigest != "" {
		return d.installContract(ctx, inst, t.InfraDigest)
	}
	if r := d.repo(inst); r != nil {
		return r.Checkout(ctx, t.InfraCommit)
	}
	return nil
}

func (d *Deployer) installContract(ctx context.Context, inst, digest string) error {
	if d.Contracts == nil {
		return &output.Error{
			Code:    "E_DEPLOY_MISCONFIGURED",
			Message: "deployer misconfigured: no contract fetcher for an OCI source",
			Hint:    "deployer has no contract fetcher",
		}
	}
	instDir := filepath.Join(d.deployRoot(), inst)
	ref := d.infraRef(instDir)
	if ref == "" {
		return &output.Error{
			Code:    "E_INFRA_ARTIFACT",
			Message: fmt.Sprintf("instance %q has no CVCTL_INFRA_REF", inst),
			Hint:    "set CVCTL_INFRA_REF in cvctl.env to host/repository",
		}
	}
	timeout := hostcfg.DefaultTimeoutFetch
	if d.Host != nil {
		timeout = d.Host.TimeoutFetch()
	}
	fctx, cancel := withStepTimeout(ctx, timeout, "contract fetch")
	defer cancel()
	c, err := d.Contracts(fctx, ref, digest)
	if err != nil {
		if terr := TimeoutError(fctx); terr != nil {
			return terr
		}
		return err
	}
	return infraoci.Extract(d.fs(), instDir, c)
}

func (d *Deployer) infraRef(instDir string) string {
	raw, err := d.fs().ReadFile(filepath.Join(instDir, "cvctl.env"))
	if err != nil {
		return ""
	}
	m, err := godotenv.Unmarshal(string(raw))
	if err != nil {
		return ""
	}
	return m["CVCTL_INFRA_REF"]
}

// OCIDevSource tracks <InfraRef>:<track tag> for the contract and the same tag
// for the app images.
type OCIDevSource struct {
	Auth     infraoci.Auth
	Resolver registry.Resolver
	Allowed  []string
	Catalog  kinds.Catalog
}

// OCIProdSource tracks <InfraRef>:<track tag> and takes the app digests from
// the artifact's prod.env.
type OCIProdSource struct {
	Auth    infraoci.Auth
	Allowed []string
	Catalog kinds.Catalog
}

func ociTag(d *instance.Decisions, def string) string {
	if d != nil && d.TrackTag != "" {
		return d.TrackTag
	}
	return def
}

// ociContract resolves ref:tag, fetches it by digest and loads its contract
// (allowlist, compose load and validation) from a temp extraction.
func ociContract(ctx context.Context, a infraoci.Auth, d *instance.Decisions, tag string, cat kinds.Catalog) (*infraoci.Contract, *contract.Project, error) {
	if d == nil || d.InfraRef == "" {
		return nil, nil, &output.Error{
			Code:    "E_DECISION_MISSING",
			Message: "oci infra source has no artifact reference",
			Hint:    "set CVCTL_INFRA_REF to host/repository",
		}
	}
	if cat == nil {
		var err error
		if cat, err = kinds.Load(); err != nil {
			return nil, nil, err
		}
	}
	digest, err := infraoci.Resolve(ctx, a, d.InfraRef, tag)
	if err != nil {
		return nil, nil, err
	}
	c, err := infraoci.Fetch(ctx, a, d.InfraRef, digest)
	if err != nil {
		return nil, nil, err
	}
	dir, cleanup, err := sys.MkdirTemp("cvctl-contract-")
	if err != nil {
		return nil, nil, err
	}
	defer cleanup()
	fsys := sys.NewOSFS()
	for _, f := range []string{"compose.yml", "env.spec.yml"} {
		if err := fsys.WriteFile(filepath.Join(dir, f), c.Files[f], 0o600, sys.Owner{}); err != nil {
			return nil, nil, err
		}
	}
	p, err := contract.Load(dir, cat)
	if err != nil {
		return nil, nil, instance.WrapLoadErr(err)
	}
	spec, err := envspec.Load(filepath.Join(dir, "env.spec.yml"))
	if err != nil {
		return nil, nil, err
	}
	if is := contract.Validate(p, cat, spec); len(is) > 0 {
		return nil, nil, instance.WrapLoadErr(&output.IssuesError{Issues: is})
	}
	return c, p, nil
}

// Desired resolves the contract digest and the app image digests for the track tag.
func (s *OCIDevSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	tag := ociTag(d, "develop")
	c, p, err := ociContract(ctx, s.Auth, d, tag, s.Catalog)
	if err != nil {
		return state.Desired{}, err
	}
	for _, a := range p.Apps {
		if err := registry.Allowed(a.Image, s.Allowed); err != nil {
			return state.Desired{}, err
		}
	}
	digests := make(map[string]string, len(p.Apps))
	for _, a := range p.Apps {
		dg, err := s.Resolver.Digest(ctx, a.Image, tag)
		if err != nil {
			return state.Desired{}, err
		}
		digests[a.Name] = dg
	}
	return state.Desired{InfraDigest: c.Digest, Digests: digests}, nil
}

// Desired resolves the contract digest and reads the pinned app digests from prod.env.
func (s *OCIProdSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	c, p, err := ociContract(ctx, s.Auth, d, ociTag(d, "main"), s.Catalog)
	if err != nil {
		return state.Desired{}, err
	}
	for _, a := range p.Apps {
		if err := registry.Allowed(a.Image, s.Allowed); err != nil {
			return state.Desired{}, err
		}
	}
	data, ok := c.Files["prod.env"]
	if !ok {
		return state.Desired{}, &output.Error{
			Code:    "E_INFRA_ARTIFACT",
			Message: fmt.Sprintf("contract %s has no prod.env layer", RevShort(state.Desired{InfraDigest: c.Digest})),
			Hint:    "publish prod.env with the pinned CVCTL_DIGEST_* values for a prod instance",
		}
	}
	digests, err := prodDigests(data, p, "prod.env of contract "+RevShort(state.Desired{InfraDigest: c.Digest}), "prod.env")
	if err != nil {
		return state.Desired{}, err
	}
	return state.Desired{InfraDigest: c.Digest, Digests: digests}, nil
}
