package kinds

import (
	"errors"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/codeavers/cvctl/internal/output"
)

func code(t *testing.T, err error) string {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %v", err)
	}
	if oe.Hint == "" {
		t.Errorf("error %s has no hint", oe.Code)
	}
	return oe.Code
}

func mustLoad(t *testing.T) Catalog {
	t.Helper()
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestLoadEmbedded(t *testing.T) {
	c := mustLoad(t)
	for _, n := range []string{"postgres", "redis", "s3"} {
		if _, ok := c[n]; !ok {
			t.Errorf("missing kind %s", n)
		}
	}
	if c["postgres"].Backup != "wal-g" {
		t.Errorf("backup = %q", c["postgres"].Backup)
	}
}

func TestLoadRejectsBadBackup(t *testing.T) {
	fsys := fstest.MapFS{"bad.yml": {Data: []byte("name: bad\nimage: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: tape\n")}}
	_, err := LoadFS(fsys)
	if got := code(t, err); got != "E_KIND_INVALID" {
		t.Errorf("code = %s", got)
	}
}

func TestLoadRejectsBadImageAndVersion(t *testing.T) {
	for _, y := range []string{
		"name: a\nimage: x\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: none\n",
		"name: a\nimage: x:{version}\nversions: [\"1\"]\ndefault_version: \"2\"\nbackup: none\n",
	} {
		_, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(y)}})
		if got := code(t, err); got != "E_KIND_INVALID" {
			t.Errorf("code = %s", got)
		}
	}
}

func TestLocalVarsEscapes(t *testing.T) {
	k := mustLoad(t)["postgres"]
	got := k.LocalVars("DATABASE", "main-db", Credentials{User: "app", Password: "p@ss:w/rd", DB: "app"})
	want := map[string]string{"DATABASE_URL": "postgres://app:p%40ss%3Aw%2Frd@main-db:5432/app"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestLocalVarsLiteralWithoutScheme(t *testing.T) {
	k := mustLoad(t)["s3"]
	got := k.LocalVars("S3", "h", Credentials{User: "root-u", Password: "root-p", DB: "bk", AppUser: "a@b", AppPassword: "p/q"})
	if got["S3_SECRET_KEY"] != "p/q" || got["S3_ACCESS_KEY"] != "a@b" || got["S3_ENDPOINT"] != "http://h:9000" || got["S3_BUCKET"] != "bk" {
		t.Errorf("got %v", got)
	}
}

func TestS3Buckets(t *testing.T) {
	c := mustLoad(t)
	if !c["s3"].Buckets {
		t.Errorf("s3.Buckets = false, want true")
	}
	if c["postgres"].Buckets {
		t.Errorf("postgres.Buckets = true, want false")
	}
	if c["redis"].Buckets {
		t.Errorf("redis.Buckets = true, want false")
	}
}

func TestLocalVarsS3AppKeys(t *testing.T) {
	k := mustLoad(t)["s3"]
	creds := Credentials{
		User:        "root-u",
		Password:    "root-p",
		DB:          "my-bucket",
		AppUser:     "app-u",
		AppPassword: "app-p",
	}
	got := k.LocalVars("S3", "s3-host", creds)
	if got["S3_ACCESS_KEY"] != "app-u" {
		t.Errorf("S3_ACCESS_KEY = %q, want app user %q", got["S3_ACCESS_KEY"], "app-u")
	}
	if got["S3_SECRET_KEY"] != "app-p" {
		t.Errorf("S3_SECRET_KEY = %q, want app password %q", got["S3_SECRET_KEY"], "app-p")
	}
	if got["S3_ACCESS_KEY"] == "root-u" || got["S3_SECRET_KEY"] == "root-p" {
		t.Errorf("LocalVars returned root keys instead of app keys")
	}
	if got["S3_ENDPOINT"] != "http://s3-host:9000" {
		t.Errorf("S3_ENDPOINT = %q, want %q", got["S3_ENDPOINT"], "http://s3-host:9000")
	}
	if got["S3_BUCKET"] != "my-bucket" {
		t.Errorf("S3_BUCKET = %q, want %q", got["S3_BUCKET"], "my-bucket")
	}
}

func TestContainerEnvForS3RootKeys(t *testing.T) {
	k := mustLoad(t)["s3"]
	creds := Credentials{
		User:        "root-u",
		Password:    "root-p",
		DB:          "my-bucket",
		AppUser:     "app-u",
		AppPassword: "app-p",
	}
	env := k.ContainerEnvFor(creds)
	if env["MINIO_ROOT_USER"] != "root-u" {
		t.Errorf("MINIO_ROOT_USER = %q, want root user %q", env["MINIO_ROOT_USER"], "root-u")
	}
	if env["MINIO_ROOT_PASSWORD"] != "root-p" {
		t.Errorf("MINIO_ROOT_PASSWORD = %q, want root password %q", env["MINIO_ROOT_PASSWORD"], "root-p")
	}
}

func TestUsesApp(t *testing.T) {
	c := mustLoad(t)
	if !c["s3"].UsesApp() {
		t.Errorf("s3.UsesApp() = false, want true")
	}
	if c["postgres"].UsesApp() {
		t.Errorf("postgres.UsesApp() = true, want false")
	}
	if c["redis"].UsesApp() {
		t.Errorf("redis.UsesApp() = true, want false")
	}
}

func TestLocalVarsEscapesAppCredentials(t *testing.T) {
	k := Kind{
		Name: "custom",
		Port: 9000,
		Env: map[string]string{
			"URL": "s3://{app_user}:{app_password}@{host}:{port}/{db}",
		},
	}
	got := k.LocalVars("CUSTOM", "s3-host", Credentials{
		AppUser:     "app@user",
		AppPassword: "p@ss:w/rd",
		DB:          "bucket",
	})
	want := map[string]string{
		"CUSTOM_URL": "s3://app%40user:p%40ss%3Aw%2Frd@s3-host:9000/bucket",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestSubstituteAppCredentials(t *testing.T) {
	k := Kind{}
	got := k.Substitute("app={app_user}:{app_password} root={user}:{password} db={db}", Credentials{
		User:        "ru",
		Password:    "rp",
		DB:          "mydb",
		AppUser:     "au",
		AppPassword: "ap",
	})
	want := "app=au:ap root=ru:rp db=mydb"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestContainerEnvLiteral(t *testing.T) {
	k := mustLoad(t)["postgres"]
	env := k.ContainerEnvFor(Credentials{User: "app", Password: "p@ss:w/rd", DB: "app"})
	if env["POSTGRES_PASSWORD"] != "p@ss:w/rd" {
		t.Errorf("got %q", env["POSTGRES_PASSWORD"])
	}
}

func TestExternalVarsS3(t *testing.T) {
	k := mustLoad(t)["s3"]
	vals := map[string]string{"endpoint": "e", "bucket": "b", "access_key": "a", "secret_key": "s", "region": "r"}
	got, err := k.ExternalVars("S3", vals)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"S3_ENDPOINT": "e", "S3_BUCKET": "b", "S3_ACCESS_KEY": "a", "S3_SECRET_KEY": "s", "S3_REGION": "r"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
	delete(vals, "bucket")
	_, err = k.ExternalVars("S3", vals)
	if c := code(t, err); c != "E_KIND_EXTERNAL_MISSING" {
		t.Errorf("code = %s", c)
	}
}

func TestImageFor(t *testing.T) {
	k := mustLoad(t)["postgres"]
	img, err := k.ImageFor("16")
	if err != nil || img != "ghcr.io/codeavers/cvctl-postgres:16" {
		t.Errorf("img=%q err=%v", img, err)
	}
	_, err = k.ImageFor("9")
	if c := code(t, err); c != "E_CONTRACT_KIND_VERSION" {
		t.Errorf("code = %s", c)
	}
}

func TestSubstitute(t *testing.T) {
	k := mustLoad(t)["redis"]
	got := k.Substitute(k.Command[2], Credentials{Password: "a/b"})
	if got != "a/b" {
		t.Errorf("got %q", got)
	}
}

func TestLoadRejectsEnvExternalMismatch(t *testing.T) {
	base := "name: a\nimage: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: none\n"
	for name, extra := range map[string]string{
		"env without external": "env: {URL: \"u\"}\n",
		"external without env": "external: [url]\n",
		"different names":      "env: {URL: \"u\"}\nexternal: [endpoint]\n",
	} {
		_, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(base + extra)}})
		if got := code(t, err); got != "E_KIND_INVALID" {
			t.Errorf("%s: code = %s", name, got)
		}
	}
	ok := base + "env: {Access_Key: \"u\"}\nexternal: [access_key]\n"
	if _, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(ok)}}); err != nil {
		t.Errorf("matching sets rejected: %v", err)
	}
}

func TestLoadRejectsPasswordInShellHealth(t *testing.T) {
	base := "name: a\nimage: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: none\n"
	for name, extra := range map[string]string{
		"health":               "health: [CMD-SHELL, \"ping {password}\"]\n",
		"command":              "command: [serve, \"{password}\"]\nhealth: [CMD-SHELL, ping]\n",
		"health app_password":  "health: [CMD-SHELL, \"ping {app_password}\"]\n",
		"command app_password": "command: [serve, \"{app_password}\"]\nhealth: [CMD-SHELL, ping]\n",
	} {
		_, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(base + extra)}})
		if got := code(t, err); got != "E_KIND_INVALID" {
			t.Errorf("%s: code = %s", name, got)
		}
	}
	ok := base + "command: [serve, \"{password}\", \"{app_password}\"]\nhealth: [CMD, ping, \"{password}\", \"{app_password}\"]\n"
	if _, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(ok)}}); err != nil {
		t.Errorf("CMD form rejected: %v", err)
	}
}
