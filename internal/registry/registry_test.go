package registry_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-containerregistry/pkg/name"
	ggcrregistry "github.com/google/go-containerregistry/pkg/registry"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/remote"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestAllowedPrefixBoundary(t *testing.T) {
	allowed := []string{"ghcr.io/codeavers"}

	tests := []struct {
		name    string
		image   string
		allowed []string
		wantErr bool
	}{
		{
			name:    "exact prefix match",
			image:   "ghcr.io/codeavers",
			allowed: allowed,
			wantErr: false,
		},
		{
			name:    "sub-path image match",
			image:   "ghcr.io/codeavers/app:v1",
			allowed: allowed,
			wantErr: false,
		},
		{
			name:    "sub-path with digest",
			image:   "ghcr.io/codeavers/app@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			allowed: allowed,
			wantErr: false,
		},
		{
			name:    "sub-path with tag and digest",
			image:   "ghcr.io/codeavers/app:v1.0.0@sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
			allowed: allowed,
			wantErr: false,
		},
		{
			name:    "evil prefix boundary must fail",
			image:   "ghcr.io/codeavers-evil/x:v1",
			allowed: allowed,
			wantErr: true,
		},
		{
			name:    "different registry must fail",
			image:   "docker.io/codeavers/app:v1",
			allowed: allowed,
			wantErr: true,
		},
		{
			name:    "case insensitive match",
			image:   "GHCR.IO/Codeavers/App:V1",
			allowed: []string{"ghcr.io/codeavers"},
			wantErr: false,
		},
		{
			name:    "empty allowed list must fail",
			image:   "ghcr.io/codeavers/app:v1",
			allowed: nil,
			wantErr: true,
		},
		{
			name:    "empty slice allowed list must fail",
			image:   "ghcr.io/codeavers/app:v1",
			allowed: []string{},
			wantErr: true,
		},
		{
			name:    "registry with port preserves port in match",
			image:   "localhost:5000/app:latest",
			allowed: []string{"localhost:5000/app"},
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := registry.Allowed(tc.image, tc.allowed)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for image %q, got nil", tc.image)
				}
				var oe *output.Error
				if !errors.As(err, &oe) {
					t.Fatalf("expected *output.Error, got %T: %v", err, err)
				}
				if oe.Code != "E_REGISTRY_NOT_ALLOWED" {
					t.Errorf("expected code E_REGISTRY_NOT_ALLOWED, got %s", oe.Code)
				}
				if len(tc.allowed) == 0 && oe.Hint != "set CVCTL_REGISTRIES in /etc/cvctl/host.env" {
					t.Errorf("expected hint 'set CVCTL_REGISTRIES in /etc/cvctl/host.env', got %q", oe.Hint)
				}
			} else if err != nil {
				t.Fatalf("unexpected error for image %q: %v", tc.image, err)
			}
		})
	}
}

func TestDigestResolves(t *testing.T) {
	expectedDigest := "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method == http.MethodHead && r.URL.Path == "/v2/myrepo/app/manifests/v1.0.0" {
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Content-Length", "1024")
			w.Header().Set("Docker-Content-Digest", expectedDigest)
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	host := srv.Listener.Addr().String()
	res := registry.New("cvctl", "test-token", host, []string{host})

	ctx := context.Background()
	digest, err := res.Digest(ctx, host+"/myrepo/app", "v1.0.0")
	if err != nil {
		t.Fatalf("unexpected error resolving digest: %v", err)
	}
	if digest != expectedDigest {
		t.Fatalf("expected digest %s, got %s", expectedDigest, digest)
	}
}

func TestDigestErrorNoToken(t *testing.T) {
	secretToken := "secret-super-token-12345"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	host := srv.Listener.Addr().String()
	res := registry.New("myuser", secretToken, host, []string{host})

	ctx := context.Background()
	_, err := res.Digest(ctx, host+"/myrepo/app", "latest")
	if err == nil {
		t.Fatalf("expected error from unauthorized registry, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_REGISTRY_RESOLVE" {
		t.Errorf("expected code E_REGISTRY_RESOLVE, got %s", oe.Code)
	}
	expectedMsg := "HTTP 401 resolving " + host + "/myrepo/app:latest"
	if oe.Message != expectedMsg {
		t.Errorf("expected message %q, got %q", expectedMsg, oe.Message)
	}
	if strings.Contains(err.Error(), secretToken) {
		t.Fatalf("secret token leaked in err.Error(): %s", err.Error())
	}
	if strings.Contains(oe.Message, secretToken) {
		t.Fatalf("secret token leaked in oe.Message: %s", oe.Message)
	}
	if strings.Contains(oe.Hint, secretToken) {
		t.Fatalf("secret token leaked in oe.Hint: %s", oe.Hint)
	}
	if strings.Contains(strings.ToLower(oe.Message), "authorization") {
		t.Fatalf("Authorization header leaked in message: %s", oe.Message)
	}
}

func TestDigestErrorUnreachableRegistry(t *testing.T) {
	host := "127.0.0.1:1"
	res := registry.New("cvctl", "token", host, []string{host})
	_, err := res.Digest(context.Background(), host+"/repo/app", "v1")
	if err == nil {
		t.Fatalf("expected error for unreachable registry, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_REGISTRY_RESOLVE" {
		t.Errorf("expected code E_REGISTRY_RESOLVE, got %s", oe.Code)
	}
	expectedMsg := "cannot reach registry " + host
	if oe.Message != expectedMsg {
		t.Errorf("expected message %q, got %q", expectedMsg, oe.Message)
	}
}

func TestLoginUsesStdin(t *testing.T) {
	run := &systest.Runner{}
	token := "ghp_secrettoken999"
	run.On("docker login ghcr.io -u deployer --password-stdin", sys.Result{ExitCode: 0})

	ctx := context.Background()
	err := registry.Login(ctx, run, "ghcr.io", "deployer", token)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(run.Calls) != 1 {
		t.Fatalf("expected 1 call, got %d", len(run.Calls))
	}
	call := run.Calls[0]
	if string(call.Stdin) != token {
		t.Errorf("expected Stdin to equal token, got %q", string(call.Stdin))
	}
	for _, arg := range call.Args {
		if strings.Contains(arg, token) {
			t.Errorf("token found in command args: %v", call.Args)
		}
	}
	if strings.Contains(call.String(), token) {
		t.Errorf("token found in Cmd.String(): %s", call.String())
	}
	wantEnv := []string{"DOCKER_CONFIG=/var/lib/cvctl/docker"}
	if !reflect.DeepEqual(call.Env, wantEnv) {
		t.Errorf("expected call.Env to equal %v, got %v", wantEnv, call.Env)
	}
}

func TestLoginFailure(t *testing.T) {
	run := &systest.Runner{}
	token := "bad-token"
	run.On("docker login ghcr.io -u cvctl --password-stdin", sys.Result{
		ExitCode: 1,
		Stderr:   []byte("Error response from daemon: unauthorized: bad token"),
	})

	ctx := context.Background()
	err := registry.Login(ctx, run, "ghcr.io", "", token)
	if err == nil {
		t.Fatalf("expected error on non-zero exit, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if oe.Code != "E_REGISTRY_LOGIN" {
		t.Errorf("expected code E_REGISTRY_LOGIN, got %s", oe.Code)
	}
	if oe.Hint != "check the registry token (needs read:packages)" {
		t.Errorf("expected hint 'check the registry token (needs read:packages)', got %q", oe.Hint)
	}
}

func TestInsecureHostsFromEnv(t *testing.T) {
	t.Setenv("CVCTL_INSECURE_REGISTRIES", "localhost:5000, 127.0.0.1:5000, , reg.local ")
	hosts := registry.InsecureHostsFromEnv()
	expected := []string{"localhost:5000", "127.0.0.1:5000", "reg.local"}
	if len(hosts) != len(expected) {
		t.Fatalf("expected %d hosts, got %d: %v", len(expected), len(hosts), hosts)
	}
	for i, h := range expected {
		if hosts[i] != h {
			t.Errorf("at index %d: expected %s, got %s", i, h, hosts[i])
		}
	}

	t.Setenv("CVCTL_INSECURE_REGISTRIES", "")
	if hosts := registry.InsecureHostsFromEnv(); len(hosts) != 0 {
		t.Errorf("expected empty for empty env var, got %v", hosts)
	}
}

func TestLoginWithFSFixesConfigOwnershipAsRoot(t *testing.T) {
	old := registry.Geteuid
	t.Cleanup(func() { registry.Geteuid = old })
	registry.Geteuid = func() int { return 0 }

	run := &systest.Runner{}
	run.On("docker login ghcr.io -u cvctl --password-stdin", sys.Result{ExitCode: 0})
	fsys := systest.NewFS()
	const cfg = "/var/lib/cvctl/docker/config.json"
	if err := fsys.WriteFile(cfg, []byte(`{"auths":{}}`), 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.LoginWithFS(context.Background(), run, fsys, "ghcr.io", "", "tok"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	fi, err := fsys.Stat(cfg)
	if err != nil || fi.User != "deployer" || fi.Group != "deploy" || fi.Mode != 0o600 {
		t.Fatalf("config.json not fixed: %+v err=%v", fi, err)
	}
	data, _ := fsys.ReadFile(cfg)
	if string(data) != `{"auths":{}}` {
		t.Fatalf("content changed: %q", data)
	}
}

func TestLoginWithFSLeavesConfigAloneWhenNotRoot(t *testing.T) {
	old := registry.Geteuid
	t.Cleanup(func() { registry.Geteuid = old })
	registry.Geteuid = func() int { return 1000 }

	run := &systest.Runner{}
	run.On("docker login ghcr.io -u cvctl --password-stdin", sys.Result{ExitCode: 0})
	fsys := systest.NewFS()
	const cfg = "/var/lib/cvctl/docker/config.json"
	_ = fsys.WriteFile(cfg, []byte("{}"), 0o644, sys.Owner{User: "root", Group: "root"})
	if err := registry.LoginWithFS(context.Background(), run, fsys, "ghcr.io", "", "tok"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := fsys.Stat(cfg); fi.User != "root" || fi.Mode != 0o644 {
		t.Fatalf("config.json should be untouched: %+v", fi)
	}
}

func TestSameHost(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"GHCR.io", "ghcr.io", true},
		{"https://ghcr.io", "ghcr.io", true},
		{"localhost:5000", "localhost:5001", false},
		{"reg.io", "reg.io:5000", false},
		{"", "", false},
		{"docker.io", "index.docker.io", true},
	} {
		if got := registry.SameHost(tc.a, tc.b); got != tc.want {
			t.Errorf("SameHost(%q,%q)=%v", tc.a, tc.b, got)
		}
	}
}

func TestDigestTokenOnlyToTokenHost(t *testing.T) {
	var auths []string
	mk := func() string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			auths = append(auths, r.Host+"|"+r.Header.Get("Authorization"))
			if r.URL.Path == "/v2/" {
				return
			}
			w.Header().Set("Docker-Content-Digest", "sha256:ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad")
			w.Header().Set("Content-Type", "application/vnd.docker.distribution.manifest.v2+json")
			w.Header().Set("Content-Length", "1024")
		}))
		t.Cleanup(srv.Close)
		return srv.Listener.Addr().String()
	}
	tokenHost, other := mk(), mk()
	res := registry.New("u", "secret", tokenHost, []string{tokenHost, other})
	ctx := context.Background()
	if _, err := res.Digest(ctx, other+"/a/b", "v1"); err != nil {
		t.Fatal(err)
	}
	for _, a := range auths {
		if strings.Contains(a, "|Basic") {
			t.Fatalf("token sent to non-token host: %s", a)
		}
	}
	auths = nil
	if _, err := res.Digest(ctx, tokenHost+"/a/b", "v1"); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range auths {
		found = found || strings.Contains(a, "|Basic")
	}
	if !found {
		t.Fatal("token host got no Basic auth")
	}
}

func TestPruneAuthsKeepsOnlyTokenHost(t *testing.T) {
	fsys := systest.NewFS()
	const cfg = "/var/lib/cvctl/docker/config.json"
	in := `{"auths":{"ghcr.io":{"auth":"a"},"https://index.docker.io/v1/":{"auth":"b"},"old.example.com":{"auth":"c"}},"credsStore":"x","HttpHeaders":{"User-Agent":"y"}}`
	if err := fsys.WriteFile(cfg, []byte(in), 0o644, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	if err := registry.PruneAuths(fsys, "GHCR.io"); err != nil {
		t.Fatal(err)
	}
	data, _ := fsys.ReadFile(cfg)
	var doc struct {
		Auths       map[string]json.RawMessage `json:"auths"`
		CredsStore  string                     `json:"credsStore"`
		HTTPHeaders map[string]string          `json:"HttpHeaders"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Auths) != 1 || doc.Auths["ghcr.io"] == nil || doc.CredsStore != "x" || doc.HTTPHeaders["User-Agent"] != "y" {
		t.Fatalf("config = %s", data)
	}
	if fi, err := fsys.Stat(cfg); err != nil || fi.User != "deployer" || fi.Group != "deploy" || fi.Mode != 0o600 {
		t.Fatalf("owner/mode: %+v %v", fi, err)
	}
	if err := registry.PruneAuths(systest.NewFS(), "ghcr.io"); err != nil {
		t.Fatalf("missing config: %v", err)
	}
}

func labeledImage(t *testing.T, arch string, labels map[string]string) v1.Image {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           "linux",
		Architecture: arch,
		Config:       v1.Config{Labels: labels},
	})
	if err != nil {
		t.Fatalf("build image: %v", err)
	}
	return img
}

func newLabelRegistry(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(ggcrregistry.New())
	t.Cleanup(srv.Close)
	return srv.Listener.Addr().String()
}

func TestLabelsSingleImage(t *testing.T) {
	host := newLabelRegistry(t)
	want := map[string]string{"cvctl.pr": "42", "cvctl.commit": "abc123"}
	img := labeledImage(t, runtime.GOARCH, want)
	ref, err := name.ParseReference(host+"/myrepo/app:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := registry.NewLabels("cvctl", "tok", host, []string{host}).Labels(context.Background(), host+"/myrepo/app:v1", d.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
}

func TestLabelsIndexUsesHostPlatform(t *testing.T) {
	host := newLabelRegistry(t)
	amd := labeledImage(t, "amd64", map[string]string{"cvctl.commit": "amd"})
	arm := labeledImage(t, "arm64", map[string]string{"cvctl.commit": "arm"})
	idx := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{Add: amd, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "amd64"}}},
		mutate.IndexAddendum{Add: arm, Descriptor: v1.Descriptor{Platform: &v1.Platform{OS: "linux", Architecture: "arm64"}}},
	)
	ref, err := name.ParseReference(host+"/myrepo/app:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.WriteIndex(ref, idx); err != nil {
		t.Fatal(err)
	}
	d, err := idx.Digest()
	if err != nil {
		t.Fatal(err)
	}

	want := "amd"
	if runtime.GOARCH == "arm64" {
		want = "arm"
	} else if runtime.GOARCH != "amd64" {
		t.Skipf("index has no %s manifest", runtime.GOARCH)
	}
	got, err := registry.NewLabels("cvctl", "tok", host, []string{host}).Labels(context.Background(), host+"/myrepo/app:v1", d.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["cvctl.commit"] != want {
		t.Fatalf("cvctl.commit = %q, want %q", got["cvctl.commit"], want)
	}
}

func TestLabelsInheritedOCIOnlyReturnedUnchanged(t *testing.T) {
	host := newLabelRegistry(t)
	want := map[string]string{
		"org.opencontainers.image.source":   "https://example.com/org/repo",
		"org.opencontainers.image.revision": "deadbeef",
	}
	img := labeledImage(t, runtime.GOARCH, want)
	ref, err := name.ParseReference(host+"/myrepo/app:v1", name.Insecure)
	if err != nil {
		t.Fatal(err)
	}
	if err := remote.Write(ref, img); err != nil {
		t.Fatal(err)
	}
	d, err := img.Digest()
	if err != nil {
		t.Fatal(err)
	}

	got, err := registry.NewLabels("cvctl", "tok", host, []string{host}).Labels(context.Background(), host+"/myrepo/app:v1", d.String())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("labels = %v, want %v", got, want)
	}
	for k := range got {
		if strings.HasPrefix(k, "cvctl.") {
			t.Errorf("unexpected cvctl key %q", k)
		}
	}
}

func TestLabelsMissingDigest(t *testing.T) {
	host := newLabelRegistry(t)
	secret := "secret-super-token-12345"
	missing := "sha256:" + strings.Repeat("0", 64)

	_, err := registry.NewLabels("cvctl", secret, host, []string{host}).Labels(context.Background(), host+"/myrepo/app", missing)
	if err == nil {
		t.Fatal("expected error for missing digest")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(oe.Hint, secret) {
		t.Fatalf("token leaked in error: %v / %s", err, oe.Hint)
	}
}
