package infraoci_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrreg "github.com/google/go-containerregistry/pkg/registry"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/static"
	"github.com/google/go-containerregistry/pkg/v1/types"

	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

const (
	titleKey    = "org.opencontainers.image.title"
	composeType = "application/vnd.cvctl.compose.v1+yaml"
	specType    = "application/vnd.cvctl.envspec.v1+yaml"
)

func newRegistry(t *testing.T, wrap func(http.Handler) http.Handler) (string, infraoci.Auth) {
	t.Helper()
	var h http.Handler = ggcrreg.New(ggcrreg.Logger(log.New(io.Discard, "", 0)))
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return host, infraoci.Auth{User: "u", Token: "tok", TokenHost: host, Insecure: []string{host}}
}

func goodFiles() map[string][]byte {
	return map[string][]byte{
		"compose.yml":  []byte("services: {}\n"),
		"env.spec.yml": []byte("vars: []\n"),
		"prod.env":     []byte("A=1\n"),
	}
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

type layer struct {
	title, mt string
	data      []byte
}

// push writes an arbitrary artifact so rejection cases can be exercised.
func push(t *testing.T, a infraoci.Auth, host, tag, cfgType string, layers []layer) string {
	t.Helper()
	img := mutate.MediaType(empty.Image, types.OCIManifestSchema1)
	img = mutate.ConfigMediaType(img, types.MediaType(cfgType))
	for _, l := range layers {
		var err error
		img, err = mutate.Append(img, mutate.Addendum{
			Layer:       static.NewLayer(l.data, types.MediaType(l.mt)),
			Annotations: map[string]string{titleKey: l.title},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	ref, err := name.ParseReference(host+"/infra/bad:"+tag, name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, _ := img.Digest()
	return d.String()
}

func TestPublishResolveFetchRoundTrip(t *testing.T) {
	host, a := newRegistry(t, nil)
	ctx := context.Background()
	ref := host + "/infra/app"
	files := goodFiles()
	pub, err := infraoci.Publish(ctx, a, ref+":v1", files)
	if err != nil {
		t.Fatal(err)
	}
	res, err := infraoci.Resolve(ctx, a, ref, "v1")
	if err != nil || res != pub {
		t.Fatalf("resolve %q %v, published %q", res, err, pub)
	}
	c, err := infraoci.Fetch(ctx, a, ref, pub)
	if err != nil {
		t.Fatal(err)
	}
	if c.Digest != pub || len(c.Files) != 3 {
		t.Fatalf("contract: %+v", c)
	}
	for k, v := range files {
		if !bytes.Equal(c.Files[k], v) {
			t.Fatalf("%s differs", k)
		}
	}
	again, err := infraoci.Publish(ctx, a, ref+":v2", files)
	if err != nil || again != pub {
		t.Fatalf("publish not deterministic: %q vs %q (%v)", again, pub, err)
	}
}

func TestPublishOptionalProdEnvAndRejections(t *testing.T) {
	host, a := newRegistry(t, nil)
	ctx := context.Background()
	ref := host + "/infra/app:v1"
	files := goodFiles()
	delete(files, "prod.env")
	d, err := infraoci.Publish(ctx, a, ref, files)
	if err != nil {
		t.Fatal(err)
	}
	c, err := infraoci.Fetch(ctx, a, host+"/infra/app", d)
	if err != nil || len(c.Files) != 2 {
		t.Fatalf("%v %+v", err, c)
	}

	bad := goodFiles()
	delete(bad, "env.spec.yml")
	_, err = infraoci.Publish(ctx, a, ref, bad)
	wantCode(t, err, "E_PUBLISH")
	bad = goodFiles()
	bad["../x"] = []byte("x")
	_, err = infraoci.Publish(ctx, a, ref, bad)
	wantCode(t, err, "E_PUBLISH")
	bad = goodFiles()
	bad["compose.yml"] = make([]byte, infraoci.MaxLayerSize+1)
	_, err = infraoci.Publish(ctx, a, ref, bad)
	wantCode(t, err, "E_PUBLISH")
}

func TestFetchRejections(t *testing.T) {
	host, a := newRegistry(t, nil)
	ctx := context.Background()
	compose := layer{"compose.yml", composeType, []byte("a")}
	spec := layer{"env.spec.yml", specType, []byte("b")}
	big := layer{"compose.yml", composeType, make([]byte, infraoci.MaxLayerSize+1)}
	cases := map[string]struct {
		cfg    string
		layers []layer
	}{
		"wrong config type":  {"application/vnd.oci.image.config.v1+json", []layer{compose, spec}},
		"wrong layer type":   {infraoci.ContractType, []layer{{"compose.yml", "text/plain", []byte("a")}, spec}},
		"unknown title":      {infraoci.ContractType, []layer{compose, spec, {"other.yml", composeType, []byte("c")}}},
		"traversal title":    {infraoci.ContractType, []layer{compose, spec, {"../compose.yml", composeType, []byte("c")}}},
		"duplicate":          {infraoci.ContractType, []layer{compose, spec, {"compose.yml", composeType, []byte("z")}}},
		"missing env spec":   {infraoci.ContractType, []layer{compose}},
		"missing compose":    {infraoci.ContractType, []layer{spec}},
		"oversized layer":    {infraoci.ContractType, []layer{big, spec}},
		"title swapped type": {infraoci.ContractType, []layer{{"compose.yml", specType, []byte("a")}, spec}},
	}
	i := 0
	for n, tc := range cases {
		i++
		d := push(t, a, host, "t"+string(rune('a'+i)), tc.cfg, tc.layers)
		_, err := infraoci.Fetch(ctx, a, host+"/infra/bad", d)
		if err == nil {
			t.Errorf("%s: accepted", n)
			continue
		}
		wantCode(t, err, "E_INFRA_ARTIFACT")
	}
}

func TestFetchRejectsTamperedLayer(t *testing.T) {
	var tamper bool
	host, a := newRegistry(t, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if tamper && r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/blobs/") {
				_, _ = w.Write([]byte("tampered"))
				return
			}
			h.ServeHTTP(w, r)
		})
	})
	ctx := context.Background()
	d, err := infraoci.Publish(ctx, a, host+"/infra/app:v1", goodFiles())
	if err != nil {
		t.Fatal(err)
	}
	tamper = true
	_, err = infraoci.Fetch(ctx, a, host+"/infra/app", d)
	wantCode(t, err, "E_INFRA_ARTIFACT")
}

func TestExtractWritesAndReplaces(t *testing.T) {
	fsys := systest.NewFS()
	c := &infraoci.Contract{Files: goodFiles()}
	if err := infraoci.Extract(fsys, "/srv/deploy/i", c); err != nil {
		t.Fatal(err)
	}
	fi, err := fsys.Stat("/srv/deploy/i/infra/compose.yml")
	if err != nil || fi.Mode != 0o640 || fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("%+v %v", fi, err)
	}
	c2 := &infraoci.Contract{Files: map[string][]byte{"compose.yml": []byte("new"), "env.spec.yml": []byte("s")}}
	if err := infraoci.Extract(fsys, "/srv/deploy/i", c2); err != nil {
		t.Fatal(err)
	}
	if b, _ := fsys.ReadFile("/srv/deploy/i/infra/compose.yml"); string(b) != "new" {
		t.Fatalf("not replaced: %q", b)
	}
	for p := range fsys.Files() {
		if strings.Contains(p, ".infra-") || strings.Contains(p, "infra.prev") || strings.HasSuffix(p, "infra/prod.env") {
			t.Fatalf("leftover %s", p)
		}
	}
}

type renameFailFS struct {
	*systest.FS
	failNew bool
}

func (f *renameFailFS) Rename(o, n string) error {
	if f.failNew && strings.Contains(o, ".infra-") {
		return errors.New("boom")
	}
	return f.FS.Rename(o, n)
}

func TestExtractRestoresOnFailure(t *testing.T) {
	base := systest.NewFS()
	_ = base.MkdirAll("/srv/deploy/i/infra", 0o750, sys.Owner{})
	_ = base.WriteFile("/srv/deploy/i/infra/compose.yml", []byte("old"), 0o640, sys.Owner{})
	fsys := &renameFailFS{FS: base, failNew: true}
	err := infraoci.Extract(fsys, "/srv/deploy/i", &infraoci.Contract{Files: goodFiles()})
	wantCode(t, err, "E_INFRA_ARTIFACT")
	if b, _ := base.ReadFile("/srv/deploy/i/infra/compose.yml"); string(b) != "old" {
		t.Fatalf("previous infra not restored: %q", b)
	}
	for p := range base.Files() {
		if strings.Contains(p, ".infra-") || strings.Contains(p, "infra.prev") {
			t.Fatalf("leftover %s", p)
		}
	}
}

func TestExtractRefusesSymlinkAndBadNames(t *testing.T) {
	fsys := systest.NewFS()
	_ = fsys.MkdirAll("/srv/deploy/i/infra", 0o750, sys.Owner{})
	fsys.MarkSymlink("/srv/deploy/i/infra")
	wantCode(t, infraoci.Extract(fsys, "/srv/deploy/i", &infraoci.Contract{Files: goodFiles()}), "E_INFRA_ARTIFACT")

	fresh := systest.NewFS()
	err := infraoci.Extract(fresh, "/srv/deploy/i", &infraoci.Contract{Files: map[string][]byte{"../evil": []byte("x")}})
	wantCode(t, err, "E_INFRA_ARTIFACT")
}

func TestTokenOnlyToTokenHost(t *testing.T) {
	var seenA, seenB []string
	rec := func(dst *[]string) func(http.Handler) http.Handler {
		return func(h http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				*dst = append(*dst, r.Header.Get("Authorization"))
				h.ServeHTTP(w, r)
			})
		}
	}
	hostA, a := newRegistry(t, rec(&seenA))
	hostB, _ := newRegistry(t, rec(&seenB))
	a.Insecure = append(a.Insecure, hostB)
	ctx := context.Background()
	if _, err := infraoci.Publish(ctx, a, hostA+"/infra/app:v1", goodFiles()); err != nil {
		t.Fatal(err)
	}
	if _, err := infraoci.Publish(ctx, a, hostB+"/infra/app:v1", goodFiles()); err != nil {
		t.Fatal(err)
	}
	has := func(v []string) bool {
		for _, s := range v {
			if s != "" {
				return true
			}
		}
		return false
	}
	if !has(seenA) {
		t.Fatal("token host received no credentials")
	}
	if has(seenB) {
		t.Fatalf("credentials leaked to other host: %v", seenB)
	}
}
