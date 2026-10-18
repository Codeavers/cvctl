package deploy

import (
	"context"
	"errors"
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
// or for an OCI source the contract by digest (local cache, else registry)
// extracted. A revision of the other source kind fails before touching infra/.
func (d *Deployer) checkout(ctx context.Context, inst string, t state.Desired) error {
	instDir := filepath.Join(d.deployRoot(), inst)
	env := d.instanceEnv(instDir)
	isOCI := env["CVCTL_INFRA_SOURCE"] == instance.SourceOCI
	switch {
	case isOCI && t.InfraDigest == "" && t.InfraCommit != "":
		return revMismatch(inst, "git commit "+commitShort(t.InfraCommit), "oci")
	case !isOCI && t.InfraDigest != "":
		return revMismatch(inst, "contract "+RevShort(t), "git")
	}
	if t.InfraDigest != "" {
		return d.installContract(ctx, inst, env["CVCTL_INFRA_REF"], t.InfraDigest)
	}
	if r := d.repo(inst); r != nil {
		return r.Checkout(ctx, t.InfraCommit)
	}
	return nil
}

func revMismatch(inst, rev, source string) error {
	return &output.Error{
		Code:    "E_DEPLOY_INVALID",
		Message: fmt.Sprintf("instance %q records %s but its infra source is %s", inst, rev, source),
		Hint:    "deploy the instance from its current source (cvctl deploy) instead of restoring a revision of the other source",
	}
}

// Cache is the contract cache of inst.
func (d *Deployer) Cache(inst string) *ContractCache {
	return NewContractCache(d.fs(), d.stateRoot(), inst)
}

// pruneContracts keeps the cached contracts state references plus the most
// recent ones; a failure only leaves extra files behind.
func (d *Deployer) pruneContracts(inst string, st *state.State) {
	var keep []string
	for _, t := range []*state.Desired{st.Current, st.Previous} {
		if t != nil && t.InfraDigest != "" {
			keep = append(keep, t.InfraDigest)
		}
	}
	_ = d.Cache(inst).Prune(keep...)
}

func (d *Deployer) tokenHost() string {
	if d.Host == nil {
		return ""
	}
	return d.Host.TokenHost()
}

func (d *Deployer) installContract(ctx context.Context, inst, ref, digest string) error {
	instDir := filepath.Join(d.deployRoot(), inst)
	cat, err := d.catalog()
	if err != nil {
		return err
	}
	cache := d.Cache(inst)
	if c, ok := cache.Get(digest); ok {
		if _, err := ValidateContract(c, cat); err == nil {
			_ = cache.Put(c)
			return infraoci.Extract(d.fs(), instDir, c)
		}
	}
	if d.Contracts == nil {
		return &output.Error{
			Code:    "E_DEPLOY_MISCONFIGURED",
			Message: "deployer misconfigured: no contract fetcher for an OCI source",
			Hint:    "deployer has no contract fetcher",
		}
	}
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
		var oe *output.Error
		if errors.As(err, &oe) && strings.Contains(oe.Message, "HTTP 404") {
			e2 := *oe
			e2.Message += fmt.Sprintf("; %s is not in the local cache and the registry no longer has it under %s: it may belong to a previous CVCTL_INFRA_REF", RevShort(state.Desired{InfraDigest: digest}), ref)
			err = &e2
		}
		return RegistryHint(d.tokenHost(), ref, err)
	}
	// An invalid contract never reaches infra/: the restore paths rely on
	// infra/ still holding the last installed revision.
	if _, verr := ValidateContract(c, cat); verr != nil {
		return &invalidContractError{err: verr}
	}
	_ = cache.Put(c)
	return infraoci.Extract(d.fs(), instDir, c)
}

// invalidContractError marks a fetched contract that failed validation, a
// permanent failure for that desired state.
type invalidContractError struct{ err error }

func (e *invalidContractError) Error() string { return e.err.Error() }
func (e *invalidContractError) Unwrap() error { return e.err }

func (d *Deployer) instanceEnv(instDir string) map[string]string {
	raw, err := d.fs().ReadFile(filepath.Join(instDir, "cvctl.env"))
	if err != nil {
		return map[string]string{}
	}
	m, err := godotenv.Unmarshal(string(raw))
	if err != nil {
		return map[string]string{}
	}
	return m
}

// RegistryHint adds to an E_INFRA_ARTIFACT error that ref's host gets no
// token when it differs from the token host.
func RegistryHint(tokenHost, ref string, err error) error {
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_INFRA_ARTIFACT" {
		return err
	}
	host := strings.SplitN(ref, "/", 2)[0]
	if tokenHost == "" || registry.SameHost(host, tokenHost) {
		return err
	}
	e2 := *oe
	e2.Hint = strings.TrimSpace(e2.Hint + "; the registry token is sent only to " + tokenHost + " (CVCTL_REGISTRY_HOST); " + host + " is accessed anonymously")
	return &e2
}

// ValidateContract loads c (allowlist, compose load, env spec) from a temp
// directory and validates it.
func ValidateContract(c *infraoci.Contract, cat kinds.Catalog) (*contract.Project, error) {
	if cat == nil {
		var err error
		if cat, err = kinds.Load(); err != nil {
			return nil, err
		}
	}
	dir, cleanup, err := sys.MkdirTemp("cvctl-contract-")
	if err != nil {
		return nil, err
	}
	defer cleanup()
	fsys := sys.NewOSFS()
	for _, f := range []string{"compose.yml", "env.spec.yml"} {
		if err := fsys.WriteFile(filepath.Join(dir, f), c.Files[f], 0o600, sys.Owner{}); err != nil {
			return nil, err
		}
	}
	p, err := contract.Load(dir, cat)
	if err != nil {
		return nil, instance.WrapLoadErr(err)
	}
	spec, err := envspec.Load(filepath.Join(dir, "env.spec.yml"))
	if err != nil {
		return nil, err
	}
	if is := contract.Validate(p, cat, spec); len(is) > 0 {
		return nil, instance.WrapLoadErr(&output.IssuesError{Issues: is})
	}
	return p, nil
}

// OCIDevSource tracks <InfraRef>:<track tag> for the contract and the same tag
// for the app images.
type OCIDevSource struct {
	Auth     infraoci.Auth
	Resolver registry.Resolver
	Allowed  []string
	Catalog  kinds.Catalog
	Cache    *ContractCache
}

// OCIProdSource tracks <InfraRef>:<track tag> and takes the app digests from
// the artifact's prod.env.
type OCIProdSource struct {
	Auth    infraoci.Auth
	Allowed []string
	Catalog kinds.Catalog
	Cache   *ContractCache
}

func ociTag(d *instance.Decisions, def string) string {
	if d != nil && d.TrackTag != "" {
		return d.TrackTag
	}
	return def
}

// ociContract resolves ref:tag and loads and validates that contract, from
// the cache when it already holds the digest, else fetched and cached.
func ociContract(ctx context.Context, a infraoci.Auth, cache *ContractCache, d *instance.Decisions, tag string, cat kinds.Catalog) (*infraoci.Contract, *contract.Project, error) {
	if d == nil || d.InfraRef == "" {
		return nil, nil, &output.Error{
			Code:    "E_DECISION_MISSING",
			Message: "oci infra source has no artifact reference",
			Hint:    "set CVCTL_INFRA_REF to host/repository",
		}
	}
	digest, err := infraoci.Resolve(ctx, a, d.InfraRef, tag)
	if err != nil {
		return nil, nil, RegistryHint(a.TokenHost, d.InfraRef, err)
	}
	if c, ok := cache.Get(digest); ok {
		if p, err := ValidateContract(c, cat); err == nil {
			return c, p, nil
		}
	}
	c, err := infraoci.Fetch(ctx, a, d.InfraRef, digest)
	if err != nil {
		return nil, nil, RegistryHint(a.TokenHost, d.InfraRef, err)
	}
	p, err := ValidateContract(c, cat)
	if err != nil {
		return nil, nil, err
	}
	_ = cache.Put(c)
	return c, p, nil
}

// Desired resolves the contract digest and the app image digests for the track tag.
func (s *OCIDevSource) Desired(ctx context.Context, d *instance.Decisions) (state.Desired, error) {
	tag := ociTag(d, "develop")
	c, p, err := ociContract(ctx, s.Auth, s.Cache, d, tag, s.Catalog)
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
	c, p, err := ociContract(ctx, s.Auth, s.Cache, d, ociTag(d, "main"), s.Catalog)
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
