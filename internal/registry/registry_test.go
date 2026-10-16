package registry_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	res := registry.New("cvctl", "test-token", []string{host})

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
	res := registry.New("myuser", secretToken, []string{host})

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
	res := registry.New("cvctl", "token", []string{host})
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
