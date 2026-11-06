package instance

import (
	"slices"
	"testing"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/kinds"
)

func sqliteProject(t *testing.T) (*contract.Project, *Decisions, kinds.Catalog) {
	t.Helper()
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p := &contract.Project{Data: []contract.DataService{{Name: "store", Kind: "sqlite", Owner: "api", EnvPrefix: "STORE", Backup: "dump"}}}
	d := &Decisions{Instance: "app-prod", Env: "prod", Strategy: "recreate", Services: map[string]ServiceDecision{"store": {Mode: "local"}}}
	return p, d, cat
}

func sqliteCodes(d *Decisions, p *contract.Project, cat kinds.Catalog) []string {
	var out []string
	for _, is := range Check(d, p, cat) {
		out = append(out, is.Code)
	}
	return out
}

func TestSqliteNoCredentialsNoHealth(t *testing.T) {
	p, d, cat := sqliteProject(t)
	if got := sqliteCodes(d, p, cat); len(got) != 0 {
		t.Fatalf("a sqlite service needs no credentials: %v", got)
	}
}

func TestCheckRejectsBlueGreenWithSqlite(t *testing.T) {
	p, d, cat := sqliteProject(t)
	d.Strategy = "blue-green"
	if got := sqliteCodes(d, p, cat); !slices.Contains(got, "E_SQLITE_RECREATE") {
		t.Fatalf("codes: %v", got)
	}
}

func TestCheckKeepsBlueGreenWithoutSqlite(t *testing.T) {
	p, d, cat := sqliteProject(t)
	p.Data[0].Kind = "redis"
	d.Strategy = "blue-green"
	if got := sqliteCodes(d, p, cat); slices.Contains(got, "E_SQLITE_RECREATE") {
		t.Fatalf("codes: %v", got)
	}
}

func TestCheckRejectsExternalSqlite(t *testing.T) {
	p, d, cat := sqliteProject(t)
	d.Services["store"] = ServiceDecision{Mode: "external"}
	if got := sqliteCodes(d, p, cat); !slices.Contains(got, "E_DECISION_EXTERNAL_NOT_ALLOWED") {
		t.Fatalf("codes: %v", got)
	}
}
