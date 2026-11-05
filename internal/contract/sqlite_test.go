package contract

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/kinds"
)

const sqliteBase = `
services:
  api:
    image: ghcr.io/acme/api:1
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy:
      resources:
        limits:
          memory: 128M
  db:
    labels:
      cvctl.kind: postgres
      cvctl.env: DB
%SQLITE%
x-cvctl:
  health:
    service: api
    http: /healthz
`

func loadSqlite(t *testing.T, services string) (*Project, kinds.Catalog) {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw := strings.Replace(sqliteBase, "%SQLITE%", services, 1)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, cat)
	if err != nil {
		t.Fatal(err)
	}
	return p, cat
}

func sqliteSvc(name string, labels ...string) string {
	s := "  " + name + ":\n    labels:\n      cvctl.kind: sqlite\n"
	for _, l := range labels {
		s += "      " + l + "\n"
	}
	return s
}

func TestSqliteLint(t *testing.T) {
	ok := []string{"cvctl.owner: api", "cvctl.env: STORE"}
	for _, tc := range []struct {
		name, services, want string
	}{
		{"valid", sqliteSvc("store", ok...), ""},
		{"valid uid", sqliteSvc("store", append(ok, `cvctl.uid: "1000"`)...), ""},
		{"backup none", sqliteSvc("store", append(ok, "cvctl.backup: none")...), ""},
		{"two on one owner", sqliteSvc("a", ok[0], "cvctl.env: A") + sqliteSvc("b", ok[0], "cvctl.env: B"), ""},
		{"no owner", sqliteSvc("store", "cvctl.env: STORE"), "E_CONTRACT_SQLITE_OWNER"},
		{"owner does not exist", sqliteSvc("store", "cvctl.owner: ghost", "cvctl.env: STORE"), "E_CONTRACT_SQLITE_OWNER"},
		{"owner is not an app", sqliteSvc("store", "cvctl.owner: db", "cvctl.env: STORE"), "E_CONTRACT_SQLITE_OWNER"},
		{"same path on one owner", sqliteSvc("a", ok...) + sqliteSvc("b", ok...), "E_CONTRACT_SQLITE_PATH"},
		{"expose", sqliteSvc("store", append(ok, `cvctl.expose: "9000"`)...), "E_CONTRACT_EXPOSE_DATA"},
		{"external", sqliteSvc("store", append(ok, "cvctl.external: allowed")...), "E_CONTRACT_SQLITE_EXTERNAL"},
		{"volume backup", sqliteSvc("store", append(ok, "cvctl.backup: volume")...), "E_CONTRACT_BACKUP_UNSUPPORTED"},
		{"uid not numeric", sqliteSvc("store", append(ok, "cvctl.uid: node")...), "E_CONTRACT_SQLITE_UID"},
		{"uid root", sqliteSvc("store", append(ok, `cvctl.uid: "0"`)...), "E_CONTRACT_SQLITE_UID"},
		{"two uids on one owner", sqliteSvc("a", ok[0], "cvctl.env: A", `cvctl.uid: "1000"`) + sqliteSvc("b", ok[0], "cvctl.env: B", `cvctl.uid: "1001"`), "E_CONTRACT_SQLITE_UID"},
		{"same uid on one owner", sqliteSvc("a", ok[0], "cvctl.env: A", `cvctl.uid: "1000"`) + sqliteSvc("b", ok[0], "cvctl.env: B", `cvctl.uid: "1000"`), ""},
		{"uid on the first service only", sqliteSvc("a", ok[0], "cvctl.env: A", `cvctl.uid: "1000"`) + sqliteSvc("b", ok[0], "cvctl.env: B"), "E_CONTRACT_SQLITE_UID"},
		{"uid on the second service only", sqliteSvc("a", ok[0], "cvctl.env: A") + sqliteSvc("b", ok[0], "cvctl.env: B", `cvctl.uid: "1000"`), "E_CONTRACT_SQLITE_UID"},
		{"uid with leading zero", sqliteSvc("store", append(ok, `cvctl.uid: "0100"`)...), "E_CONTRACT_SQLITE_UID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, cat := loadSqlite(t, tc.services)
			var codes []string
			for _, is := range Validate(p, cat, nil) {
				if is.Hint == "" {
					t.Errorf("%s has no hint", is.Code)
				}
				codes = append(codes, is.Code)
			}
			if tc.want == "" && len(codes) != 0 {
				t.Fatalf("issues: %v", codes)
			}
			if tc.want != "" && !slices.Contains(codes, tc.want) {
				t.Fatalf("want %s, got %v", tc.want, codes)
			}
		})
	}
}

func TestSqliteDependsOnStaysRejected(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	raw := strings.Replace(sqliteBase, "%SQLITE%", sqliteSvc("store", "cvctl.owner: api", "cvctl.env: STORE"), 1)
	raw = strings.Replace(raw, "    image: ghcr.io/acme/api:1\n", "    image: ghcr.io/acme/api:1\n    depends_on: [store]\n", 1)
	if err := os.WriteFile(filepath.Join(dir, "compose.yml"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	p, err := Load(dir, cat)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, is := range Validate(p, cat, nil) {
		found = found || is.Code == "E_CONTRACT_DEPENDS_ON_DATA"
	}
	if !found {
		t.Fatal("depends_on towards a sqlite service must stay rejected")
	}
}

func TestSqliteLoadReadsOwnerAndUID(t *testing.T) {
	p, _ := loadSqlite(t, sqliteSvc("store", "cvctl.owner: api", "cvctl.env: STORE", `cvctl.uid: "1000"`))
	for _, d := range p.Data {
		if d.Name == "store" {
			if d.Owner != "api" || d.UID != "1000" || d.Backup != "dump" {
				t.Fatalf("data: %+v", d)
			}
			return
		}
	}
	t.Fatal("store not loaded")
}

func TestSqliteSpecProvidesPath(t *testing.T) {
	p, cat := loadSqlite(t, sqliteSvc("store", "cvctl.owner: api", "cvctl.env: STORE"))
	spec := &envspec.Spec{Vars: map[string]envspec.Var{"STORE_PATH": {Source: "service", Service: "store"}}}
	if issues := Validate(p, cat, spec); len(issues) != 0 {
		t.Fatalf("issues: %+v", issues)
	}
}

func TestValidUID(t *testing.T) {
	for v, want := range map[string]bool{
		"1": true, "1000": true, "65534": true, "4294967294": true,
		"": false, "0": false, "00": false, "0100": false, "007": false, "-1": false, "+1": false, "node": false, "1000:1000": false, "4294967295": false, "1 ": false,
	} {
		if got := ValidUID(v); got != want {
			t.Errorf("ValidUID(%q) = %v", v, got)
		}
	}
}

func TestVerifyOnDumpServiceRejected(t *testing.T) {
	p, cat := loadSqlite(t, sqliteSvc("store", "cvctl.owner: api", "cvctl.env: STORE"))
	for name, tc := range map[string]struct {
		service string
		want    bool
	}{"dump": {"store", true}, "wal-g": {"db", false}} {
		t.Run(name, func(t *testing.T) {
			p.X.Verify = &VerifySpec{Service: tc.service, SQL: "select 1"}
			found := false
			for _, is := range Validate(p, cat, nil) {
				found = found || is.Code == "E_CONTRACT_VERIFY_DUMP"
			}
			if found != tc.want {
				t.Fatalf("E_CONTRACT_VERIFY_DUMP = %v", found)
			}
		})
	}
}
