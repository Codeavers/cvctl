package cli

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/state"
)

const (
	routeWebOnly = "http:\n  routers:\n    myapp-dev-web:\n      rule: Host(`app.example.com`)\n      entryPoints: [web]\n"
	routeEdge    = "http:\n  routers:\n    myapp-dev-web:\n      rule: Host(`app.example.com`)\n      entryPoints: [websecure]\n      tls:\n        certResolver: le\n"
)

type statusRoutesJSON struct {
	Data struct {
		Instances []struct {
			Instance string `json:"instance"`
			Status   string `json:"status"`
			Routes   string `json:"routes"`
		} `json:"instances"`
		Warnings []string `json:"warnings"`
	} `json:"data"`
}

func statusWithRoute(t *testing.T, edge, route string, probe RouteProber) statusRoutesJSON {
	t.Helper()
	_, env := setupOpsFixture(t, "myapp-dev")
	t.Setenv("CVCTL_STORAGE", "s3")
	env.Host.EdgeMode = edge
	env.ProbeRoute = probe
	if route != "" {
		if err := os.WriteFile(filepath.Join(env.Deployer.TraefikDir, "myapp-dev.yml"), []byte(route), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	_ = env.Store.Save(&state.State{Instance: "myapp-dev", Status: state.StatusOK, UpdatedAt: time.Now()})
	oldEnv := newOpsEnv
	t.Cleanup(func() { newOpsEnv = oldEnv })
	newOpsEnv = func(o opsOpts) (*OpsEnv, error) { return env, nil }

	code, out, errb := run(t, "status", "--json")
	if code != 0 {
		t.Fatalf("code %d: out=%s err=%s", code, out, errb)
	}
	var res statusRoutesJSON
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("unmarshal: %v out=%s", err, out)
	}
	if len(res.Data.Instances) != 1 {
		t.Fatalf("instances %+v", res.Data.Instances)
	}
	return res
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// CODEA-49: status said OK while the route of an edge host listened only on web.
func TestStatusFlagsRouteForOtherEdgeMode(t *testing.T) {
	res := statusWithRoute(t, "edge", routeWebOnly, func(context.Context, string, int, string) error { return nil })
	it := res.Data.Instances[0]
	if it.Status != "OK" || it.Routes != routesMismatch {
		t.Fatalf("got %+v", it)
	}
	if !hasWarning(res.Data.Warnings, "cvctl deploy myapp-dev") {
		t.Fatalf("warnings %v", res.Data.Warnings)
	}
}

func TestStatusRouteOK(t *testing.T) {
	var gotEdge, gotDomain string
	res := statusWithRoute(t, "edge", routeEdge, func(_ context.Context, edge string, _ int, domain string) error {
		gotEdge, gotDomain = edge, domain
		return nil
	})
	if it := res.Data.Instances[0]; it.Routes != routesOK {
		t.Fatalf("got %+v warnings %v", it, res.Data.Warnings)
	}
	if gotEdge != "edge" || gotDomain != "app.example.com" {
		t.Fatalf("probe got edge=%q domain=%q", gotEdge, gotDomain)
	}
}

func TestStatusRouteNotResponding(t *testing.T) {
	res := statusWithRoute(t, "edge", routeEdge, func(context.Context, string, int, string) error { return errNoRouter })
	if it := res.Data.Instances[0]; it.Routes != routesDown {
		t.Fatalf("got %+v", it)
	}
	if !hasWarning(res.Data.Warnings, "app.example.com does not respond") {
		t.Fatalf("warnings %v", res.Data.Warnings)
	}
}

func TestStatusNoRouteFile(t *testing.T) {
	res := statusWithRoute(t, "edge", "", func(context.Context, string, int, string) error {
		t.Fatal("no route file: nothing to probe")
		return nil
	})
	if it := res.Data.Instances[0]; it.Routes != routesNone {
		t.Fatalf("got %+v", it)
	}
}

func TestStatusProbeHonoursDeadline(t *testing.T) {
	oldProbe, oldCheck := routeProbeTimeout, routeCheckTimeout
	t.Cleanup(func() { routeProbeTimeout, routeCheckTimeout = oldProbe, oldCheck })
	routeProbeTimeout, routeCheckTimeout = 100*time.Millisecond, 200*time.Millisecond
	start := time.Now()
	res := statusWithRoute(t, "edge", routeEdge, func(ctx context.Context, _ string, _ int, _ string) error {
		<-ctx.Done()
		return ctx.Err()
	})
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("status took %s", d)
	}
	if it := res.Data.Instances[0]; it.Routes != routesDown {
		t.Fatalf("got %+v", it)
	}
}

func serverAddr(t *testing.T, s *httptest.Server) string {
	t.Helper()
	return strings.TrimPrefix(strings.TrimPrefix(s.URL, "https://"), "http://")
}

func TestProbeAddr(t *testing.T) {
	traefik404 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }))
	defer traefik404.Close()
	app404 := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"not found"}`))
	}))
	defer app404.Close()
	var gotHost string
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost = r.Host
		http.Redirect(w, r, "/login", http.StatusFound)
	}))
	defer redirect.Close()

	ctx := context.Background()
	if err := probeAddr(ctx, "https", serverAddr(t, traefik404), "app.example.com"); !errors.Is(err, errNoRouter) {
		t.Fatalf("traefik 404: got %v", err)
	}
	if err := probeAddr(ctx, "https", serverAddr(t, app404), "app.example.com"); err != nil {
		t.Fatalf("app 404: got %v", err)
	}
	if err := probeAddr(ctx, "http", serverAddr(t, redirect), "app.example.com"); err != nil {
		t.Fatalf("redirect: got %v", err)
	}
	if gotHost != "app.example.com" {
		t.Fatalf("Host header %q", gotHost)
	}
}

func TestProbeAddrDoesNotHang(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	// Accepts and never answers, like a filtered or wedged proxy.
	go func() {
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				c.Close()
			}
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			conns = append(conns, c)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := probeAddr(ctx, "https", ln.Addr().String(), "app.example.com"); err == nil {
		t.Fatal("expected an error from a silent server")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("probe took %s", d)
	}

	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := closed.Addr().String()
	closed.Close()
	if err := probeAddr(context.Background(), "http", addr, "app.example.com"); err == nil {
		t.Fatal("expected an error from a closed port")
	}
}
