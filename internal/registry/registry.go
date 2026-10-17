package registry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// Allowed verifies that image belongs to at least one prefix in allowed.
// Tag and digest suffixes are stripped before checking. Comparison is case-insensitive.
// An empty allowed list returns E_REGISTRY_NOT_ALLOWED.
func Allowed(image string, allowed []string) error {
	const hint = "set CVCTL_REGISTRIES in /etc/cvctl/host.env"
	if len(allowed) == 0 {
		return &output.Error{
			Code:    "E_REGISTRY_NOT_ALLOWED",
			Message: fmt.Sprintf("image %q is not in allowed registries", image),
			Hint:    hint,
		}
	}

	clean := strings.ToLower(stripTagAndDigest(image))
	for _, prefix := range allowed {
		p := strings.ToLower(strings.TrimRight(strings.TrimSpace(prefix), "/"))
		if p == "" {
			continue
		}
		if clean == p || strings.HasPrefix(clean, p+"/") {
			return nil
		}
	}

	return &output.Error{
		Code:    "E_REGISTRY_NOT_ALLOWED",
		Message: fmt.Sprintf("image %q is not in allowed registries", image),
		Hint:    hint,
	}
}

func stripTagAndDigest(image string) string {
	if idx := strings.Index(image, "@"); idx != -1 {
		image = image[:idx]
	}
	slashIdx := strings.LastIndex(image, "/")
	colonIdx := strings.LastIndex(image, ":")
	if colonIdx > slashIdx {
		image = image[:colonIdx]
	}
	return image
}

// Resolver resolves image tags to immutable digests.
type Resolver interface {
	Digest(ctx context.Context, image, tag string) (string, error)
}

type resolver struct {
	user          string
	token         string
	insecureHosts []string
}

// New returns a Resolver using go-containerregistry.
func New(user, token string, insecureHosts []string) Resolver {
	return &resolver{
		user:          user,
		token:         token,
		insecureHosts: insecureHosts,
	}
}

func (r *resolver) Digest(ctx context.Context, image, tag string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	refStr := image
	if tag != "" {
		refStr = fmt.Sprintf("%s:%s", image, tag)
	}

	var nameOpts []name.Option
	preRef, err := name.ParseReference(refStr)
	var regHost string
	if err == nil {
		regHost = preRef.Context().RegistryStr()
		if isInsecure(regHost, r.insecureHosts) {
			nameOpts = append(nameOpts, name.Insecure)
		}
	} else if isInsecure(imageHost(refStr), r.insecureHosts) {
		nameOpts = append(nameOpts, name.Insecure)
	}

	ref, err := name.ParseReference(refStr, nameOpts...)
	if err != nil {
		host := regHost
		if host == "" {
			host = imageHost(refStr)
		}
		return "", r.sanitizeError(&output.Error{
			Code:    "E_REGISTRY_RESOLVE",
			Message: fmt.Sprintf("cannot reach registry %s", host),
			Hint:    "check image and tag syntax",
		})
	}
	regHost = ref.Context().RegistryStr()

	var remoteOpts []remote.Option
	remoteOpts = append(remoteOpts, remote.WithContext(ctx))
	if r.token != "" {
		u := r.user
		if u == "" {
			u = "cvctl"
		}
		remoteOpts = append(remoteOpts, remote.WithAuth(&authn.Basic{
			Username: u,
			Password: r.token,
		}))
	} else {
		remoteOpts = append(remoteOpts, remote.WithAuth(authn.Anonymous))
	}

	desc, err := remote.Head(ref, remoteOpts...)
	if err != nil {
		var terr *transport.Error
		if errors.As(err, &terr) {
			msg := fmt.Sprintf("HTTP %d resolving %s:%s", terr.StatusCode, image, tag)
			if tag == "" {
				msg = fmt.Sprintf("HTTP %d resolving %s", terr.StatusCode, image)
			}
			hint := "check registry connectivity, image name, and credentials"
			if terr.StatusCode == 401 || terr.StatusCode == 403 {
				hint = "check the registry token (needs read:packages)"
			} else if terr.StatusCode == 404 {
				hint = "verify image name and tag exist in registry"
			}
			return "", r.sanitizeError(&output.Error{
				Code:    "E_REGISTRY_RESOLVE",
				Message: msg,
				Hint:    hint,
			})
		}
		return "", r.sanitizeError(&output.Error{
			Code:    "E_REGISTRY_RESOLVE",
			Message: fmt.Sprintf("cannot reach registry %s", regHost),
			Hint:    "check registry connectivity and credentials",
		})
	}

	return desc.Digest.String(), nil
}

func (r *resolver) sanitizeError(err *output.Error) *output.Error {
	if r.token != "" {
		err.Message = strings.ReplaceAll(err.Message, r.token, "[REDACTED]")
		err.Hint = strings.ReplaceAll(err.Hint, r.token, "[REDACTED]")
	}
	err.Message = strings.ReplaceAll(err.Message, "Authorization", "[REDACTED]")
	err.Hint = strings.ReplaceAll(err.Hint, "Authorization", "[REDACTED]")
	return err
}

func imageHost(refStr string) string {
	parts := strings.SplitN(refStr, "/", 2)
	if len(parts) > 1 && (strings.Contains(parts[0], ".") || strings.Contains(parts[0], ":") || parts[0] == "localhost") {
		return parts[0]
	}
	return "index.docker.io"
}

func isInsecure(host string, insecureHosts []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	host = strings.TrimPrefix(host, "http://")
	host = strings.TrimPrefix(host, "https://")
	for _, h := range insecureHosts {
		h = strings.ToLower(strings.TrimSpace(h))
		h = strings.TrimPrefix(h, "http://")
		h = strings.TrimPrefix(h, "https://")
		if h == host {
			return true
		}
	}
	return false
}

// Geteuid is the effective-uid lookup, replaceable in tests.
var Geteuid = os.Geteuid

const dockerConfigPath = "/var/lib/cvctl/docker/config.json"

var dockerConfigOwner = sys.Owner{User: "deployer", Group: "deploy"}

// Login runs `docker login <host> -u <user> --password-stdin` passing the token on Stdin.
func Login(ctx context.Context, run sys.Runner, host, user, token string) error {
	return LoginWithFS(ctx, run, nil, host, user, token)
}

// LoginWithFS is Login plus, when running as root with a non-nil fsys, handing
// the resulting docker config to deployer:deploy with mode 0600 so the
// deploy pipeline (which runs as deployer) can pull with these credentials.
func LoginWithFS(ctx context.Context, run sys.Runner, fsys sys.FS, host, user, token string) error {
	if user == "" {
		user = "cvctl"
	}
	cmd := sys.Cmd{
		Name:  "docker",
		Args:  []string{"login", host, "-u", user, "--password-stdin"},
		Stdin: []byte(token),
		Env:   []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"},
	}
	res, err := run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return sys.CmdErr("E_REGISTRY_LOGIN", cmd, res, err, "check the registry token (needs read:packages)")
	}
	if fsys != nil && Geteuid() == 0 {
		data, rErr := fsys.ReadFile(dockerConfigPath)
		if rErr == nil {
			// WriteFile rewrites atomically with the requested mode and owner.
			rErr = fsys.WriteFile(dockerConfigPath, data, 0o600, dockerConfigOwner)
		}
		if rErr != nil && !errors.Is(rErr, fs.ErrNotExist) {
			return &output.Error{
				Code:    "E_REGISTRY_LOGIN",
				Message: fmt.Sprintf("login succeeded but cannot hand %s to deployer: %v", dockerConfigPath, rErr),
				Hint:    "check ownership of /var/lib/cvctl/docker",
			}
		}
	}
	return nil
}

// InsecureHostsFromEnv parses the comma-separated CVCTL_INSECURE_REGISTRIES environment variable.
func InsecureHostsFromEnv() []string {
	val := os.Getenv("CVCTL_INSECURE_REGISTRIES")
	if val == "" {
		return nil
	}
	var hosts []string
	for _, part := range strings.Split(val, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			hosts = append(hosts, part)
		}
	}
	return hosts
}
