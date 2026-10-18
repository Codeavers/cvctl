package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/spf13/cobra"

	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
)

func newPublishCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "publish <dir> <ref>",
		Short: "Lint a contract folder and publish it as an OCI artifact",
		Long: `Runs the lint checks on <dir>, then pushes compose.yml, env.spec.yml and
deploy/prod.env (if present) to <ref> (host/repository:tag).

Credentials come from CVCTL_PUBLISH_TOKEN or GITHUB_TOKEN, and the user from
CVCTL_PUBLISH_USER or GITHUB_ACTOR. The token is sent only to the host of <ref>.`,
		Example: `  GITHUB_TOKEN=... GITHUB_ACTOR=me cvctl publish ./infra ghcr.io/acme/app-infra:develop`,
		Args:    cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, ref := args[0], args[1]
			if _, err := lintDir(dir); err != nil {
				return err
			}
			pubErr := func(msg, hint string) error {
				return &output.Error{Code: "E_PUBLISH", Message: msg, Hint: hint}
			}
			host := ""
			if r, err := name.ParseReference(ref); err == nil {
				host = r.Context().RegistryStr()
			} else {
				return pubErr(fmt.Sprintf("invalid artifact reference %q", ref), "use host/repository:tag")
			}
			files := map[string][]byte{}
			for _, f := range []string{"compose.yml", "env.spec.yml"} {
				data, err := os.ReadFile(filepath.Join(dir, f))
				if err != nil {
					return pubErr(fmt.Sprintf("cannot read %s: %v", f, err), "run cvctl lint on the folder first")
				}
				files[f] = data
			}
			if data, err := os.ReadFile(filepath.Join(dir, "deploy", "prod.env")); err == nil {
				files["prod.env"] = data
			} else if !os.IsNotExist(err) {
				return pubErr(fmt.Sprintf("cannot read deploy/prod.env: %v", err), "check the file permissions")
			}

			a := infraoci.Auth{
				User:      firstEnv("CVCTL_PUBLISH_USER", "GITHUB_ACTOR"),
				Token:     firstEnv("CVCTL_PUBLISH_TOKEN", "GITHUB_TOKEN"),
				TokenHost: host,
				Insecure:  registry.InsecureHostsFromEnv(),
			}
			digest, err := infraoci.Publish(cmd.Context(), a, ref, files)
			if err != nil {
				return err
			}
			printerFrom(cmd).Result(map[string]string{"ref": ref, "digest": digest}, func(w io.Writer) {
				fmt.Fprintln(w, digest)
			})
			return nil
		},
	}
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}
