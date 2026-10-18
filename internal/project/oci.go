package project

import (
	"context"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"

	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/output"
)

const (
	ociScheme = "oci://"
	keySource = "CVCTL_INFRA_SOURCE"
	keyRef    = "CVCTL_INFRA_REF"
)

func isOCIURL(u string) bool { return strings.HasPrefix(u, ociScheme) }

func ociRefErr(u string) error {
	return &output.Error{
		Code:    "E_PROJECT_REPO_URL",
		Message: fmt.Sprintf("invalid artifact reference %q", u),
		Hint:    "use oci://host/repository without a tag or digest; the tag is the instance's track tag",
	}
}

// validateOCIRef accepts oci://host/repository: the tag comes from the
// instance's track tag, so a tag or digest in the reference is rejected.
func validateOCIRef(u string) error {
	ref := strings.TrimPrefix(u, ociScheme)
	if ref == "" || strings.HasPrefix(ref, "-") || strings.Contains(ref, "@") || !strings.Contains(ref, "/") {
		return ociRefErr(u)
	}
	if last := ref[strings.LastIndex(ref, "/")+1:]; strings.Contains(last, ":") {
		return ociRefErr(u)
	}
	if _, err := name.NewRepository(ref); err != nil {
		return ociRefErr(u)
	}
	return nil
}

// ociTrackTag is the tag an OCI instance follows: CVCTL_TRACK_TAG, else
// develop for dev and main for prod.
func ociTrackTag(env, track string) string {
	if track != "" {
		return track
	}
	if env == "prod" {
		return "main"
	}
	return "develop"
}

// installOCI resolves ref:tag, fetches the contract by digest and extracts it
// into <instDir>/infra. It returns the contract digest.
func installOCI(ctx context.Context, e *Env, instDir, ref, tag string) (string, error) {
	timeout := hostcfg.DefaultTimeoutFetch
	if e.Host != nil {
		timeout = e.Host.TimeoutFetch()
	}
	fctx, cancel := deploy.WithStepTimeout(ctx, timeout, "contract fetch")
	defer cancel()
	a := deploy.OCIAuth(e.Host, e.Secrets)
	fail := func(err error) (string, error) {
		if terr := deploy.TimeoutError(fctx); terr != nil {
			return "", terr
		}
		return "", deploy.RegistryHint(a.TokenHost, ref, err)
	}
	digest, err := infraoci.Resolve(fctx, a, ref, tag)
	if err != nil {
		return fail(err)
	}
	cache := deploy.NewContractCache(e.fs(), e.stateRoot(), filepath.Base(instDir))
	c, ok := cache.Get(digest)
	if !ok {
		if c, err = infraoci.Fetch(fctx, a, ref, digest); err != nil {
			return fail(err)
		}
		cat, err := e.catalog()
		if err != nil {
			return "", err
		}
		// Only a valid contract is cached; an invalid one is still extracted so
		// the contract flow reports its issues.
		if _, verr := deploy.ValidateContract(c, cat); verr == nil {
			_ = cache.Put(c)
		}
	}
	return digest, infraoci.Extract(e.fs(), instDir, c)
}

var infraPathRe = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)

// validateInfraPath accepts a relative directory inside the infra checkout.
func validateInfraPath(p string) error {
	bad := !infraPathRe.MatchString(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") || path.Clean(p) != p
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			bad = true
		}
	}
	if bad {
		return &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_INVALID",
			Message: fmt.Sprintf("invalid --infra-path %q", p),
			Hint:    "use a relative directory inside the infra repo, e.g. deploy/infra",
		}}
	}
	return nil
}

// contractDir is the directory holding compose.yml inside infraDir.
func contractDir(infraDir, infraPath string) string {
	if infraPath == "" || infraPath == "." {
		return infraDir
	}
	return filepath.Join(infraDir, infraPath)
}
