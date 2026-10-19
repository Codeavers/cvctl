package project

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

func TestReadEnvFileComposeRules(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "# comment\n\nexport JWT_SECRET=abc\nQUOTED=\"with spaces\"\nSINGLE='a$b'\nEQ=x=y=z\nREF=${JWT_SECRET}-1\nHOSTREF=${HOME}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadEnvFile(path)
	if err != nil {
		t.Fatalf("ReadEnvFile: %v", err)
	}
	want := map[string]string{"JWT_SECRET": "abc", "QUOTED": "with spaces", "SINGLE": "a$b", "EQ": "x=y=z", "REF": "abc-1", "HOSTREF": ""}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
}

func TestReadEnvFileErrorsHideContent(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadEnvFile(filepath.Join(dir, "missing.env")); !isCode(err, "E_USAGE_ENV_FILE") {
		t.Fatalf("missing file: got %v, want E_USAGE_ENV_FILE", err)
	}
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("SECRET=\"s3cr3t-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := ReadEnvFile(path)
	if !isCode(err, "E_USAGE_ENV_FILE") {
		t.Fatalf("bad file: got %v, want E_USAGE_ENV_FILE", err)
	}
	if strings.Contains(err.Error(), "s3cr3t") {
		t.Fatalf("error leaks file content: %v", err)
	}
}

func TestSelectEnvFileKeepsRandomAndPromptOnly(t *testing.T) {
	spec := &envspec.Spec{Vars: map[string]envspec.Var{
		"JWT_SECRET": {Source: "random"},
		"SMTP_USER":  {Source: "prompt"},
		"TOKEN":      {Source: "prompt"},
		"DB_URL":     {Source: "service", Service: "db"},
		"APP_ENV":    {Source: "fixed"},
		"PUBLIC_URL": {Source: "template"},
	}}
	vals := map[string]string{
		"JWT_SECRET": "old-jwt", "SMTP_USER": "mailer", "TOKEN": "from-file",
		"DB_URL": "postgres://old", "APP_ENV": "x", "PUBLIC_URL": "y", "POSTGRES_PASSWORD": "pw",
	}
	taken, rep, err := selectEnvFile(spec, vals, map[string]string{"TOKEN": "from-set"})
	if err != nil {
		t.Fatal(err)
	}
	if len(taken) != 2 || taken["JWT_SECRET"] != "old-jwt" || taken["SMTP_USER"] != "mailer" {
		t.Fatalf("taken = %v", taken)
	}
	if !slices.Equal(rep.Used, []string{"JWT_SECRET", "SMTP_USER"}) {
		t.Errorf("used = %v", rep.Used)
	}
	wantIgnored := []string{"APP_ENV (source fixed)", "DB_URL (source service)", "POSTGRES_PASSWORD (not in env.spec)", "PUBLIC_URL (source template)", "TOKEN (--set)"}
	if !slices.Equal(rep.Ignored, wantIgnored) {
		t.Errorf("ignored = %v, want %v", rep.Ignored, wantIgnored)
	}
}

func TestSelectEnvFileRejectsNewline(t *testing.T) {
	spec := &envspec.Spec{Vars: map[string]envspec.Var{"KEY": {Source: "random"}}}
	_, _, err := selectEnvFile(spec, map[string]string{"KEY": "a\nb"}, nil)
	if !isCode(err, "E_ENVSPEC_VALUE") {
		t.Fatalf("got %v, want E_ENVSPEC_VALUE", err)
	}
	if strings.Contains(err.Error(), "a\nb") {
		t.Fatalf("error leaks value: %v", err)
	}
}

const envFileSpec = `
vars:
  DB_URL:
    source: service
    service: db
  JWT_SECRET:
    source: random
  SMTP_USER:
    source: prompt
`

func TestAddEnvFileSeedsAppEnv(t *testing.T) {
	tmp := t.TempDir()
	infraDir := filepath.Join(tmp, "infra")
	writeTestInfra(t, infraDir, validCompose, envFileSpec)
	stub := &stubRepo{infraSource: infraDir}
	e := &Env{
		FS:         sys.NewOSFS(),
		DeployRoot: tmp,
		StateRoot:  filepath.Join(tmp, "var-lib-cvctl"),
		Catalog:    testCatalog(t),
		RepoFactory: func(dir, _ string) RepoLike {
			r := *stub
			r.dir = dir
			return &r
		},
	}
	res, err := Add(context.Background(), e, "git@github.com:myorg/my-service-infra.git", AddOptions{
		EnvFile: map[string]string{"JWT_SECRET": "kept-jwt", "SMTP_USER": "file-user", "DB_URL": "postgres://old", "POSTGRES_PASSWORD": "old-pw"},
		Set:     map[string]string{"SMTP_USER": "set-user"},
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(tmp, "my-service-dev", "app.env"))
	if err != nil {
		t.Fatal(err)
	}
	app := string(data)
	for _, want := range []string{"kept-jwt", "set-user"} {
		if !strings.Contains(app, want) {
			t.Errorf("app.env missing %q:\n%s", want, app)
		}
	}
	for _, bad := range []string{"file-user", "postgres://old", "old-pw", "POSTGRES_PASSWORD"} {
		if strings.Contains(app, bad) {
			t.Errorf("app.env contains %q:\n%s", bad, app)
		}
	}
	if res.EnvFile == nil || !slices.Equal(res.EnvFile.Used, []string{"JWT_SECRET"}) {
		t.Errorf("report = %+v", res.EnvFile)
	}
}

func isCode(err error, code string) bool {
	var oe *output.Error
	return errors.As(err, &oe) && oe.Code == code
}
