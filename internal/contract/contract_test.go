package contract

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/kinds"
)

func TestValidateFixtures(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"valid":          "",
		"no-role":        "E_CONTRACT_NO_ROLE",
		"no-memory":      "E_CONTRACT_NO_MEMORY_LIMIT",
		"ports":          "E_CONTRACT_PORTS_PUBLISHED",
		"no-health":      "E_CONTRACT_NO_HEALTH",
		"env-undeclared": "E_CONTRACT_ENV_UNDECLARED",
		"unknown-kind":   "E_CONTRACT_UNKNOWN_KIND",
		"bad-version":    "E_CONTRACT_KIND_VERSION",
		"collision":      "E_CONTRACT_NAME_COLLISION",
		"depends-data":   "E_CONTRACT_DEPENDS_ON_DATA",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", name)
			p, err := Load(dir, cat)
			if err != nil {
				t.Fatal(err)
			}
			spec, err := envspec.Load(filepath.Join(dir, "env.spec.yml"))
			if err != nil {
				t.Fatal(err)
			}
			issues := Validate(p, cat, spec)
			if want == "" {
				if len(issues) != 0 {
					t.Fatalf("unexpected issues: %+v", issues)
				}
				return
			}
			if len(issues) != 1 || issues[0].Code != want {
				t.Fatalf("want exactly %s, got %+v", want, issues)
			}
			if issues[0].Hint == "" {
				t.Fatal("empty hint")
			}
		})
	}
}

func TestLoadValid(t *testing.T) {
	cat, _ := kinds.Load()
	p, err := Load("testdata/valid", cat)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Apps) != 1 || p.Apps[0].Name != "api" || p.Apps[0].Role != "app" || p.Apps[0].ExposePort != 3000 {
		t.Fatalf("apps: %+v", p.Apps)
	}
	if len(p.Data) != 1 {
		t.Fatalf("data: %+v", p.Data)
	}
	d := p.Data[0]
	if d.Kind != "postgres" || d.Version != "16" || d.EnvPrefix != "DB" || d.Backup != "wal-g" {
		t.Fatalf("data: %+v", d)
	}
	if p.X.Health.HTTP != "/health" || p.X.Health.Timeout.Seconds() != 30 || p.X.Verify == nil || len(p.X.Hooks.PreDeploy) != 1 {
		t.Fatalf("ext: %+v", p.X)
	}
}

func TestMissingComposeAndSpec(t *testing.T) {
	cat, _ := kinds.Load()
	if _, err := Load(t.TempDir(), cat); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnvRefs(t *testing.T) {
	got := EnvRefs([]byte("a: ${A:-x}\nb: ${B?e}\nc: $C\nd: $$D\ne: ${A}"))
	if want := []string{"A", "B", "C"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestNormalize(t *testing.T) {
	if got := Normalize("main-db.x"); got != "MAIN_DB_X" {
		t.Fatal(got)
	}
}
