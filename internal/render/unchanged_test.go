package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/kinds"
)

var updateUnchanged = flag.Bool("update-unchanged", false, "rewrite testdata/golden/unchanged; only for a deliberate change of existing output")

const fixedDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// TestExistingContractsRenderUnchanged pins the override of every contract that
// predates the sqlite kind. Its goldens were written by the code before that
// kind existed and -update leaves them alone, so a change here is a change to
// instances already deployed.
func TestExistingContractsRenderUnchanged(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		"testdata/valid",
		"testdata/buckets",
		"../contract/testdata/valid",
		"../contract/testdata/postgis-valid",
		"../contract/testdata/s3-valid",
		"../../examples/demo",
		"../../examples/e2e",
	} {
		p, err := contract.Load(dir, cat)
		if err != nil {
			t.Fatalf("%s: %v", dir, err)
		}
		d := &instance.Decisions{
			Instance: "proj-prod", Env: "prod", Strategy: "blue-green",
			Services: map[string]instance.ServiceDecision{},
			Domains:  map[string]string{},
			Digests:  map[string]string{},
		}
		for _, a := range p.Apps {
			d.Digests[a.Name] = fixedDigest
			d.Domains[a.Name] = a.Name + ".example.com"
		}
		for _, ds := range p.Data {
			d.Domains[ds.Name] = ds.Name + ".example.com"
			d.Services[ds.Name] = instance.ServiceDecision{Mode: "local", Version: ds.Version, Creds: kinds.Credentials{
				User: "u", Password: "p$w", DB: "dbname", AppUser: "app", AppPassword: "ap",
			}}
		}
		for variant, b := range map[string]*BackupTarget{"plain": nil, "backup": testBackup()} {
			got, err := Override(p, d, cat, Options{InstanceDir: "/srv/deploy/proj-prod", Edge: "edge", Backup: b})
			if err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			name := strings.NewReplacer("../", "", "/", "-", "testdata-", "").Replace(dir) + "-" + variant + ".yml"
			path := filepath.Join("testdata", "golden", "unchanged", name)
			if *updateUnchanged {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(want) != string(got) {
				t.Errorf("%s differs from what the code before sqlite rendered\n--- got\n%s\n--- want\n%s", name, got, want)
			}
		}
	}
}
