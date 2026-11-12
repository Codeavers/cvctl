package render

import (
	"strings"
	"testing"
)

func TestCheckRouteMatchesRenderedMode(t *testing.T) {
	p, d, _ := setup(t)
	for _, mode := range []string{"edge", "behind-proxy"} {
		b, err := Traefik(p, d, Options{Edge: mode})
		if err != nil {
			t.Fatal(err)
		}
		rc, err := CheckRoute(b, mode)
		if err != nil {
			t.Fatal(err)
		}
		if len(rc.Problems) != 0 {
			t.Fatalf("%s: unexpected problems %v", mode, rc.Problems)
		}
		if len(rc.Routers) != 1 || rc.Routers[0].Domain != "api.example.com" {
			t.Fatalf("%s: routers %+v", mode, rc.Routers)
		}
	}
}

// CODEA-49: an agent with a stale behind-proxy config rewrote the route of an
// edge host with only the web entry point.
func TestCheckRouteBehindProxyRouteOnEdgeHost(t *testing.T) {
	p, d, _ := setup(t)
	b, err := Traefik(p, d, Options{Edge: "behind-proxy"})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := CheckRoute(b, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Problems) != 1 || !strings.Contains(rc.Problems[0], "edge mode") {
		t.Fatalf("problems %v", rc.Problems)
	}
}

func TestCheckRouteEdgeRouteBehindProxy(t *testing.T) {
	p, d, _ := setup(t)
	b, err := Traefik(p, d, Options{Edge: "edge"})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := CheckRoute(b, "behind-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Problems) != 1 || !strings.Contains(rc.Problems[0], "behind a proxy") {
		t.Fatalf("problems %v", rc.Problems)
	}
}

func TestCheckRouteWebsecureWithoutTLS(t *testing.T) {
	b := []byte("http:\n  routers:\n    a:\n      rule: Host(`a.example.com`)\n      entryPoints: [websecure]\n")
	rc, err := CheckRoute(b, "edge")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Problems) != 1 {
		t.Fatalf("problems %v", rc.Problems)
	}
}

func TestCheckRouteBucketRouterHasNoProbeDomain(t *testing.T) {
	b := []byte("http:\n  routers:\n    b:\n      rule: Host(`s3.example.com`) && (Method(`GET`) || Method(`HEAD`)) && PathRegexp(`^/(pub)/.+`)\n      entryPoints: [web]\n")
	rc, err := CheckRoute(b, "behind-proxy")
	if err != nil {
		t.Fatal(err)
	}
	if len(rc.Problems) != 0 || len(rc.Routers) != 1 || rc.Routers[0].Domain != "" {
		t.Fatalf("got %+v", rc)
	}
}

func TestCheckRouteMalformed(t *testing.T) {
	if _, err := CheckRoute([]byte("http: [unclosed"), "edge"); err == nil {
		t.Fatal("expected a parse error")
	}
}
