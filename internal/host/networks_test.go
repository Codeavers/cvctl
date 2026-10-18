package host

import (
	"context"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestNetworkMissingWordings(t *testing.T) {
	for _, s := range []string{
		"Error response from daemon: network cvctl-edge-proxy not found",
		"Error: No such network: cvctl-edge-proxy",
	} {
		if !networkMissing([]byte(s)) {
			t.Fatalf("not recognized as missing: %q", s)
		}
	}
	if networkMissing([]byte("Cannot connect to the Docker daemon")) {
		t.Fatal("daemon error taken as a missing network")
	}
}

// While another container still uses a legacy network, own must stay on it
// (put back after a recreate) so that container keeps reaching it.
func TestRetireLegacyNetworkKeepsOwnWhileUsed(t *testing.T) {
	for _, tc := range []struct {
		name, network, own, ownNets, wantConnect string
		aliases                                  []string
	}{
		{"traefik recreated", "cvctl-edge", "cvctl-traefik", "cvctl-edge-proxy a_edge ", "docker network connect cvctl-edge cvctl-traefik", nil},
		{"traefik attached", "cvctl-edge", "cvctl-traefik", "cvctl-edge cvctl-edge-proxy ", "", nil},
		{"silo recreated", "cvctl-backup", siloContainer, "a_backup ", "docker network connect --alias cvctl-storage cvctl-backup cvctl-storage-s3-1", []string{"cvctl-storage"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &systest.Runner{}
			r.On("docker network inspect -f {{range $k, $v := .Containers}}{{$v.Name}} {{end}} "+tc.network, sys.Result{Stdout: []byte(tc.own + " old-app-1 ")})
			r.On("docker inspect -f {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}} "+tc.own, sys.Result{Stdout: []byte(tc.ownNets)})
			r.OnPrefix("docker network connect", sys.Result{})
			warn := retireLegacyNetwork(context.Background(), &Env{Run: r}, tc.network, tc.own, tc.aliases...)
			if !strings.Contains(warn, "old-app-1") || strings.Contains(warn, "failed") {
				t.Fatalf("warn = %q", warn)
			}
			connects := 0
			for _, c := range r.Calls {
				s := c.String()
				if strings.Contains(s, "network disconnect") || strings.Contains(s, "network rm") {
					t.Fatalf("legacy network still in use was touched: %s", s)
				}
				if strings.Contains(s, "network connect") {
					connects++
					if s != tc.wantConnect {
						t.Fatalf("connect = %q, want %q", s, tc.wantConnect)
					}
				}
			}
			if (tc.wantConnect != "") != (connects == 1) {
				t.Fatalf("connects = %d, want %q; calls %v", connects, tc.wantConnect, r.Calls)
			}
		})
	}
}

func TestRetireLegacyNetworkRemovesWhenOnlyOwn(t *testing.T) {
	r := &systest.Runner{}
	r.On("docker network inspect -f {{range $k, $v := .Containers}}{{$v.Name}} {{end}} cvctl-edge", sys.Result{Stdout: []byte("cvctl-traefik ")})
	r.On("docker inspect -f {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}} cvctl-traefik", sys.Result{Stdout: []byte("cvctl-edge cvctl-edge-proxy ")})
	r.On("docker network disconnect cvctl-edge cvctl-traefik", sys.Result{})
	r.On("docker network rm cvctl-edge", sys.Result{})
	if warn := retireLegacyNetwork(context.Background(), &Env{Run: r}, "cvctl-edge", "cvctl-traefik"); warn != "" {
		t.Fatalf("warn = %q", warn)
	}
	if countCalls(r, "docker network rm cvctl-edge") != 1 || countCalls(r, "docker network disconnect cvctl-edge cvctl-traefik") != 1 {
		t.Fatalf("calls: %v", r.Calls)
	}
}
