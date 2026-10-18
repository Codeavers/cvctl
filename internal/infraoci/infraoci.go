// Package infraoci publishes, resolves, fetches and extracts the infra
// contract (compose.yml, env.spec.yml, optional prod.env) stored as an OCI
// artifact.
package infraoci

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/remote/transport"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	// ContractType is the artifact type and the config media type.
	ContractType = "application/vnd.cvctl.contract.v1"
	titleKey     = "org.opencontainers.image.title"
	// MaxLayerSize caps each layer so a hostile registry cannot exhaust memory.
	MaxLayerSize = 1 << 20
)

// layerTypes maps each allowed file name to its layer media type.
var layerTypes = map[string]string{
	"compose.yml":  "application/vnd.cvctl.compose.v1+yaml",
	"env.spec.yml": "application/vnd.cvctl.envspec.v1+yaml",
	"prod.env":     "application/vnd.cvctl.prodenv.v1",
}

// fileOrder is the fixed layer order, so equal inputs give equal digests.
var fileOrder = []string{"compose.yml", "env.spec.yml", "prod.env"}

var requiredFiles = []string{"compose.yml", "env.spec.yml"}

var infraOwner = sys.Owner{User: "deployer", Group: "deploy"}

// Auth carries the registry credentials; the token is sent only to TokenHost.
type Auth struct {
	User, Token, TokenHost string
	Insecure               []string
}

// Contract is a fetched contract keyed by file name.
type Contract struct {
	Digest string
	Files  map[string][]byte
}

func fail(code, msg, hint string) *output.Error {
	return &output.Error{Code: code, Message: msg, Hint: hint}
}

func (a Auth) redact(s string) string {
	if a.Token != "" {
		s = strings.ReplaceAll(s, a.Token, "[REDACTED]")
	}
	return strings.ReplaceAll(s, "Authorization", "[REDACTED]")
}

func (a Auth) parse(s string) (name.Reference, []remote.Option, error) {
	var opts []name.Option
	if pre, err := name.ParseReference(s); err == nil && registry.IsInsecure(pre.Context().RegistryStr(), a.Insecure) {
		opts = append(opts, name.Insecure)
	}
	ref, err := name.ParseReference(s, opts...)
	if err != nil {
		return nil, nil, err
	}
	return ref, []remote.Option{
		remote.WithAuth(registry.AuthFor(a.User, a.Token, a.TokenHost, ref.Context().RegistryStr())),
	}, nil
}

// transportMsg describes a registry failure without echoing raw error text.
func (a Auth) transportMsg(err error, what string) string {
	var terr *transport.Error
	if errors.As(err, &terr) {
		return fmt.Sprintf("HTTP %d %s", terr.StatusCode, what)
	}
	return a.redact(fmt.Sprintf("%s: %v", what, err))
}

// Resolve returns the manifest digest of ref:tag.
func Resolve(ctx context.Context, a Auth, ref, tag string) (string, error) {
	s := ref
	if tag != "" {
		s = ref + ":" + tag
	}
	r, opts, err := a.parse(s)
	if err != nil {
		return "", fail("E_INFRA_ARTIFACT", fmt.Sprintf("invalid artifact reference %q", s), "use host/repository with a tag")
	}
	desc, err := remote.Head(r, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return "", fail("E_INFRA_ARTIFACT", a.transportMsg(err, "resolving "+s), "check the artifact reference and registry credentials")
	}
	return desc.Digest.String(), nil
}

// Fetch pulls ref@digest and validates it against the contract format.
func Fetch(ctx context.Context, a Auth, ref, digest string) (*Contract, error) {
	r, opts, err := a.parse(ref + "@" + digest)
	if err != nil {
		return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("invalid artifact reference %q", ref+"@"+digest), "use host/repository and a sha256 digest")
	}
	desc, err := remote.Get(r, append(opts, remote.WithContext(ctx))...)
	if err != nil {
		return nil, fail("E_INFRA_ARTIFACT", a.transportMsg(err, "pulling "+ref+"@"+digest), "check the artifact reference and registry credentials")
	}
	if desc.MediaType != types.OCIManifestSchema1 {
		return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("artifact manifest has media type %q, want %q", desc.MediaType, types.OCIManifestSchema1), "publish it with `cvctl publish`")
	}
	img, err := desc.Image()
	if err != nil {
		return nil, fail("E_INFRA_ARTIFACT", a.transportMsg(err, "reading artifact"), "check the artifact reference")
	}
	m, err := img.Manifest()
	if err != nil {
		return nil, fail("E_INFRA_ARTIFACT", a.transportMsg(err, "reading manifest"), "check the artifact reference")
	}
	if m.MediaType != types.OCIManifestSchema1 {
		return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("artifact manifest has media type %q, want %q", m.MediaType, types.OCIManifestSchema1), "publish it with `cvctl publish`")
	}
	if string(m.Config.MediaType) != ContractType {
		return nil, fail("E_INFRA_ARTIFACT", "artifact is not a cvctl contract", "publish it with `cvctl publish`")
	}

	c := &Contract{Digest: digest, Files: map[string][]byte{}}
	for _, d := range m.Layers {
		file := d.Annotations[titleKey]
		want, ok := layerTypes[file]
		if !ok || strings.Contains(file, "/") || strings.Contains(file, "..") {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("unexpected layer %q", file), "allowed layers: compose.yml, env.spec.yml, prod.env")
		}
		if _, dup := c.Files[file]; dup {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("duplicate layer %q", file), "publish each file once")
		}
		if string(d.MediaType) != want {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("layer %q has media type %q, want %q", file, d.MediaType, want), "republish the artifact")
		}
		if d.Size > MaxLayerSize {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("layer %q exceeds 1 MiB", file), "keep contract files under 1 MiB")
		}
		l, err := img.LayerByDigest(d.Digest)
		if err != nil {
			return nil, fail("E_INFRA_ARTIFACT", a.transportMsg(err, "locating layer "+file), "republish the artifact")
		}
		rc, err := l.Compressed()
		if err != nil {
			return nil, fail("E_INFRA_ARTIFACT", a.transportMsg(err, "pulling layer "+file), "check registry connectivity")
		}
		data, err := io.ReadAll(io.LimitReader(rc, MaxLayerSize+1))
		_ = rc.Close()
		if err != nil {
			return nil, fail("E_INFRA_ARTIFACT", a.transportMsg(err, "reading layer "+file), "check registry connectivity")
		}
		if len(data) > MaxLayerSize {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("layer %q exceeds 1 MiB", file), "keep contract files under 1 MiB")
		}
		sum := sha256.Sum256(data)
		if "sha256:"+hex.EncodeToString(sum[:]) != d.Digest.String() {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("layer %q does not match its digest", file), "republish the artifact")
		}
		c.Files[file] = data
	}
	for _, req := range requiredFiles {
		if _, ok := c.Files[req]; !ok {
			return nil, fail("E_INFRA_ARTIFACT", fmt.Sprintf("missing layer %q", req), "compose.yml and env.spec.yml are required")
		}
	}
	return c, nil
}

// Extract writes the contract as regular files into <instDir>/infra, replacing
// the previous directory atomically and never following symlinks.
func Extract(fsys sys.FS, instDir string, c *Contract) error {
	infra := path.Join(instDir, "infra")
	prev := infra + ".prev"
	hadInfra := false
	if fi, err := fsys.Lstat(infra); err == nil {
		if fi.IsSymlink {
			return fail("E_INFRA_ARTIFACT", infra+" is a symlink", "remove the symlink and retry")
		}
		if !fi.IsDir {
			return fail("E_INFRA_ARTIFACT", infra+" is not a directory", "remove it and retry")
		}
		hadInfra = true
	} else if errors.Is(err, fs.ErrNotExist) {
		// A crash between the two renames leaves only infra.prev.
		if _, perr := fsys.Lstat(prev); perr == nil {
			if rerr := fsys.Rename(prev, infra); rerr != nil {
				return fail("E_INFRA_ARTIFACT", fmt.Sprintf("cannot restore %s: %v", infra, rerr), "move infra.prev back to infra by hand")
			}
			hadInfra = true
		}
	}
	if ents, err := fsys.ReadDir(instDir); err == nil {
		for _, e := range ents {
			if strings.HasPrefix(e.Name(), ".infra-") {
				_ = fsys.RemoveAll(path.Join(instDir, e.Name()))
			}
		}
	}

	for file := range c.Files {
		if _, ok := layerTypes[file]; !ok {
			return fail("E_INFRA_ARTIFACT", fmt.Sprintf("unexpected file %q", file), "allowed files: compose.yml, env.spec.yml, prod.env")
		}
	}
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fail("E_INFRA_ARTIFACT", "cannot create a temporary directory name", "retry")
	}
	tmp := path.Join(instDir, ".infra-"+hex.EncodeToString(rnd[:]))
	if err := fsys.Mkdir(tmp, 0o750, infraOwner); err != nil {
		return fail("E_INFRA_ARTIFACT", fmt.Sprintf("cannot create %s: %v", tmp, err), "check ownership of the instance directory")
	}
	for _, file := range fileOrder {
		data, ok := c.Files[file]
		if !ok {
			continue
		}
		if err := fsys.WriteFile(path.Join(tmp, file), data, 0o640, infraOwner); err != nil {
			_ = fsys.RemoveAll(tmp)
			return fail("E_INFRA_ARTIFACT", fmt.Sprintf("cannot write %s: %v", file, err), "check ownership of the instance directory")
		}
	}

	if hadInfra {
		_ = fsys.RemoveAll(prev)
		if err := fsys.Rename(infra, prev); err != nil {
			_ = fsys.RemoveAll(tmp)
			return fail("E_INFRA_ARTIFACT", fmt.Sprintf("cannot replace %s: %v", infra, err), "check ownership of the instance directory")
		}
	}
	if err := fsys.Rename(tmp, infra); err != nil {
		if hadInfra {
			_ = fsys.Rename(prev, infra)
		}
		_ = fsys.RemoveAll(tmp)
		return fail("E_INFRA_ARTIFACT", fmt.Sprintf("cannot install %s: %v", infra, err), "check ownership of the instance directory")
	}
	if hadInfra {
		_ = fsys.RemoveAll(prev)
	}
	return nil
}

// Publish pushes files as a contract artifact to ref (host/repo:tag) and
// returns the manifest digest.
func Publish(ctx context.Context, a Auth, ref string, files map[string][]byte) (string, error) {
	const hint = "see the contract format in the documentation"
	for file, data := range files {
		if _, ok := layerTypes[file]; !ok {
			return "", fail("E_PUBLISH", fmt.Sprintf("unexpected file %q", file), "allowed files: compose.yml, env.spec.yml, prod.env")
		}
		if len(data) > MaxLayerSize {
			return "", fail("E_PUBLISH", fmt.Sprintf("%s exceeds 1 MiB", file), "keep contract files under 1 MiB")
		}
	}
	for _, req := range requiredFiles {
		if _, ok := files[req]; !ok {
			return "", fail("E_PUBLISH", fmt.Sprintf("missing %s", req), hint)
		}
	}

	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(ContractType))
	for _, file := range fileOrder {
		data, ok := files[file]
		if !ok {
			continue
		}
		var err error
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:       static.NewLayer(data, types.MediaType(layerTypes[file])),
			Annotations: map[string]string{titleKey: file},
		})
		if err != nil {
			return "", fail("E_PUBLISH", fmt.Sprintf("cannot build artifact: %v", err), hint)
		}
	}

	r, opts, err := a.parse(ref)
	if err != nil {
		return "", fail("E_PUBLISH", fmt.Sprintf("invalid artifact reference %q", ref), "use host/repository:tag")
	}
	if err := remote.Write(r, img, append(opts, remote.WithContext(ctx))...); err != nil {
		return "", fail("E_PUBLISH", a.transportMsg(err, "pushing "+ref), "check the registry credentials (needs write:packages)")
	}
	h, err := img.Digest()
	if err != nil {
		return "", fail("E_PUBLISH", fmt.Sprintf("cannot compute digest: %v", err), hint)
	}
	return h.String(), nil
}
