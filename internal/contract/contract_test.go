package contract

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/kinds"
)

func TestValidateFixtures(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"valid":                  "",
		"no-role":                "E_CONTRACT_NO_ROLE",
		"no-memory":              "E_CONTRACT_NO_MEMORY_LIMIT",
		"ports":                  "E_CONTRACT_PORTS_PUBLISHED",
		"no-health":              "E_CONTRACT_NO_HEALTH",
		"env-undeclared":         "E_CONTRACT_ENV_UNDECLARED",
		"unknown-kind":           "E_CONTRACT_UNKNOWN_KIND",
		"bad-version":            "E_CONTRACT_KIND_VERSION",
		"collision":              "E_CONTRACT_NAME_COLLISION",
		"reserved-backup":        "E_CONTRACT_NAME_RESERVED",
		"empty-http":             "E_CONTRACT_NO_HEALTH",
		"bad-expose":             "E_CONTRACT_EXPOSE_INVALID",
		"depends-data":           "E_CONTRACT_DEPENDS_ON_DATA",
		"bad-role":               "E_CONTRACT_ROLE_INVALID",
		"no-image":               "E_CONTRACT_NO_IMAGE",
		"bad-backup":             "E_CONTRACT_BACKUP_INVALID",
		"backup-walg-redis":      "E_CONTRACT_BACKUP_UNSUPPORTED",
		"backup-volume-postgres": "E_CONTRACT_BACKUP_UNSUPPORTED",
		"backup-none-postgres":   "",
		"spec-service-var":       "E_CONTRACT_SPEC_SERVICE_VAR",
		"spec-service-missing":   "E_CONTRACT_SPEC_SERVICE",
		"spec-service-nondata":   "E_CONTRACT_SPEC_SERVICE",
		"spec-template-domain":   "E_CONTRACT_SPEC_TEMPLATE_REF",
		"spec-template-var":      "E_CONTRACT_SPEC_TEMPLATE_REF",
		"verify-service":         "E_CONTRACT_VERIFY_SERVICE",
		"s3-valid":               "",
		"s3-not-s3":              "E_CONTRACT_S3_NOT_S3",
		"s3-bucket-name":         "E_CONTRACT_S3_BUCKET_NAME",
		"s3-route-no-public":     "E_CONTRACT_S3_ROUTE_NO_PUBLIC",
		"s3-public-no-route":     "E_CONTRACT_S3_PUBLIC_NO_ROUTE",
		"expose-data-nobuckets":  "E_CONTRACT_EXPOSE_DATA",
		"expose-data-badport":    "E_CONTRACT_EXPOSE_DATA",
	}
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("testdata", name)
			var issues []output.Issue
			p, err := Load(dir, cat)
			var ie *output.IssuesError
			if errors.As(err, &ie) {
				// The raw allowlist rejects before compose-go loads the file.
				issues = ie.Issues
			} else if err != nil {
				t.Fatal(err)
			} else {
				spec, err := envspec.Load(filepath.Join(dir, "env.spec.yml"))
				if err != nil {
					t.Fatal(err)
				}
				issues = Validate(p, cat, spec)
			}
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
	if len(p.Data) != 3 {
		t.Fatalf("data: %+v", p.Data)
	}
	byName := map[string]DataService{}
	for _, d := range p.Data {
		byName[d.Name] = d
	}
	if !byName["cache"].ExternalAllowed || !byName["media"].ExternalAllowed || byName["main-db"].ExternalAllowed {
		t.Fatalf("external: %+v", p.Data)
	}
	d := byName["main-db"]
	if d.Kind != "postgres" || d.Version != "16" || d.EnvPrefix != "DATABASE" || d.Backup != "wal-g" {
		t.Fatalf("data: %+v", d)
	}
	if p.X.Health.HTTP != "/health" || p.X.Health.Timeout.Seconds() != 90 || p.X.Verify == nil || len(p.X.Hooks.PreDeploy) != 1 || len(p.X.Hooks.PostDeploy) != 1 || p.X.Hooks.PostDeploy[0].Run != "node dist/warmup.js" {
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

func TestContractHealthPort(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	// 1. Valid fixture has expose 3000 on api, Port 0 in compose.
	p, err := Load("testdata/valid", cat)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := envspec.Load("testdata/valid/env.spec.yml")
	if err != nil {
		t.Fatal(err)
	}
	if issues := Validate(p, cat, spec); len(issues) != 0 {
		t.Fatalf("unexpected issues: %+v", issues)
	}
	if p.HealthPort() != 3000 {
		t.Fatalf("HealthPort() = %d, want 3000", p.HealthPort())
	}
	if p.X.Health.EffectivePort(p.Apps[0].ExposePort) != 3000 {
		t.Fatalf("EffectivePort = %d, want 3000", p.X.Health.EffectivePort(p.Apps[0].ExposePort))
	}

	// 2. Health service has no expose, and health.port == 0 -> E_CONTRACT_HEALTH_PORT
	pNoExpose := *p
	pNoExpose.Apps = []AppService{{Name: "api", Role: "app", ExposePort: 0}}
	pNoExpose.X.Health.Port = 0
	issues := Validate(&pNoExpose, cat, spec)
	var found bool
	for _, issue := range issues {
		if issue.Code == "E_CONTRACT_HEALTH_PORT" {
			found = true
			wantHint := "add cvctl.expose to api or x-cvctl.health.port"
			if issue.Hint != wantHint {
				t.Fatalf("hint = %q, want %q", issue.Hint, wantHint)
			}
		}
	}
	if !found {
		t.Fatalf("expected E_CONTRACT_HEALTH_PORT issue, got %+v", issues)
	}

	// 3. Health service has no expose, but health.port > 0 -> OK
	pWithPort := *p
	pWithPort.Apps = []AppService{{Name: "api", Role: "app", ExposePort: 0}}
	pWithPort.X.Health.Port = 8080
	if pWithPort.HealthPort() != 8080 {
		t.Fatalf("HealthPort() = %d, want 8080", pWithPort.HealthPort())
	}
	if pWithPort.X.Health.EffectivePort(0) != 8080 {
		t.Fatalf("EffectivePort = %d, want 8080", pWithPort.X.Health.EffectivePort(0))
	}
	for _, issue := range Validate(&pWithPort, cat, spec) {
		if issue.Code == "E_CONTRACT_HEALTH_PORT" {
			t.Fatalf("unexpected E_CONTRACT_HEALTH_PORT when port is set: %+v", issue)
		}
	}
}

func TestS3Buckets(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load("testdata/s3-valid", cat)
	if err != nil {
		t.Fatal(err)
	}
	var mediaDs *DataService
	for i := range p.Data {
		if p.Data[i].Name == "media" {
			mediaDs = &p.Data[i]
			break
		}
	}
	if mediaDs == nil {
		t.Fatal("media service not found in p.Data")
	}
	if mediaDs.ExposePort != 9000 {
		t.Fatalf("ExposePort = %d, want 9000", mediaDs.ExposePort)
	}

	bks := p.Buckets("media")
	if len(bks) != 2 {
		t.Fatalf("Buckets(media) count = %d, want 2", len(bks))
	}
	if bks[0].Name != "prescriptions" || bks[0].Public {
		t.Fatalf("first bucket = %+v, want prescriptions (private)", bks[0])
	}
	if bks[1].Name != "products" || !bks[1].Public {
		t.Fatalf("second bucket = %+v, want products (public)", bks[1])
	}

	pub := p.PublicBuckets("media")
	if len(pub) != 1 || pub[0].Name != "products" || !pub[0].Public {
		t.Fatalf("PublicBuckets(media) = %+v, want [products]", pub)
	}

	if len(p.Buckets("nonexistent")) != 0 {
		t.Fatalf("Buckets(nonexistent) = %+v, want empty", p.Buckets("nonexistent"))
	}
	if len(p.PublicBuckets("nonexistent")) != 0 {
		t.Fatalf("PublicBuckets(nonexistent) = %+v, want empty", p.PublicBuckets("nonexistent"))
	}
}

func TestS3ComposeInvalidShape(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	composePath := filepath.Join(dir, "compose.yml")
	badCompose := `x-cvctl:
  s3: invalid
services:
  api:
    image: ghcr.io/x/api:1
    labels: { cvctl.role: app }
`
	if err := os.WriteFile(composePath, []byte(badCompose), 0644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(dir, cat)
	if err == nil {
		t.Fatal("expected error for invalid s3 shape, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok || outErr.Code != "E_CONTRACT_COMPOSE_INVALID" {
		t.Fatalf("want E_CONTRACT_COMPOSE_INVALID, got %v", err)
	}
}

func TestS3SpecValidation(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load("testdata/s3-valid", cat)
	if err != nil {
		t.Fatal(err)
	}

	baseVars := func() map[string]envspec.Var {
		return map[string]envspec.Var{
			"S3_ENDPOINT":             {Source: "service", Service: "media"},
			"S3_BUCKET_PRODUCTS":      {Source: "service", Service: "media"},
			"S3_PUBLIC_URL_PRODUCTS":  {Source: "service", Service: "media"},
			"S3_BUCKET_PRESCRIPTIONS": {Source: "service", Service: "media"},
		}
	}

	// S3_BUCKET is rejected when buckets are declared
	varsWithBucket := baseVars()
	varsWithBucket["S3_BUCKET"] = envspec.Var{Source: "service", Service: "media"}
	issues := Validate(p, cat, &envspec.Spec{Vars: varsWithBucket})
	var found bool
	for _, issue := range issues {
		if issue.Code == "E_CONTRACT_SPEC_SERVICE_VAR" && strings.Contains(issue.Message, "var S3_BUCKET ") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected E_CONTRACT_SPEC_SERVICE_VAR for S3_BUCKET when buckets are declared, got %+v", issues)
	}

	// S3_PUBLIC_URL_PRESCRIPTIONS is rejected (prescriptions is private)
	varsWithPrivateURL := baseVars()
	varsWithPrivateURL["S3_PUBLIC_URL_PRESCRIPTIONS"] = envspec.Var{Source: "service", Service: "media"}
	issues = Validate(p, cat, &envspec.Spec{Vars: varsWithPrivateURL})
	found = false
	for _, issue := range issues {
		if issue.Code == "E_CONTRACT_SPEC_SERVICE_VAR" && strings.Contains(issue.Message, "var S3_PUBLIC_URL_PRESCRIPTIONS ") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected E_CONTRACT_SPEC_SERVICE_VAR for S3_PUBLIC_URL_PRESCRIPTIONS, got %+v", issues)
	}
}

func TestHookTimeoutValidation(t *testing.T) {
	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	p, err := Load("testdata/valid", cat)
	if err != nil {
		t.Fatal(err)
	}
	for timeout, bad := range map[string]bool{"": false, "20m": false, "2h": false, "2h1m": true, "0s": true, "-5m": true, "soon": true} {
		p.X.Hooks.PreDeploy[0].Timeout = timeout
		found := false
		for _, iss := range Validate(p, cat, nil) {
			if iss.Code == "E_CONTRACT_HOOK_TIMEOUT" {
				found = true
			}
		}
		if found != bad {
			t.Errorf("timeout %q: E_CONTRACT_HOOK_TIMEOUT = %v, want %v", timeout, found, bad)
		}
	}
}
