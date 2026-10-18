package host

import "testing"

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
