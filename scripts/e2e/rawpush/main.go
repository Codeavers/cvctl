// Command rawpush publishes a contract folder without linting it. The E2E uses
// it to prove that the agent rejects a contract that bypassed `cvctl publish`.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/registry"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: rawpush <dir> <ref>")
		os.Exit(2)
	}
	files := map[string][]byte{}
	for _, f := range []string{"compose.yml", "env.spec.yml"} {
		b, err := os.ReadFile(filepath.Join(os.Args[1], f))
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		files[f] = b
	}
	digest, err := infraoci.Publish(context.Background(), infraoci.Auth{Insecure: registry.InsecureHostsFromEnv()}, os.Args[2], files)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(digest)
}
