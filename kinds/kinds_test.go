package kinds

import (
	"errors"
	"reflect"
	"regexp"
	"strings"
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
	for _, n := range []string{"postgres", "postgis", "redis", "s3"} {
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

func TestPostgisKind(t *testing.T) {
	c := mustLoad(t)
	pg, gis := c["postgres"], c["postgis"]
	if gis.Backup != "wal-g" || gis.DefaultVersion != "16" || gis.Port != pg.Port || gis.VolumePath != pg.VolumePath {
		t.Errorf("postgis = %+v", gis)
	}
	if !reflect.DeepEqual(gis.Command, pg.Command) || !reflect.DeepEqual(gis.BackupCommandArgs, pg.BackupCommandArgs) ||
		!reflect.DeepEqual(gis.Health, pg.Health) || !reflect.DeepEqual(gis.CapAdd, pg.CapAdd) || gis.RestoreCheck != pg.RestoreCheck {
		t.Errorf("postgis diverges from postgres: %+v", gis)
	}
	img, err := gis.ImageFor("17")
	if err != nil || img != "ghcr.io/codeavers/cvctl-postgis:17" {
		t.Errorf("img=%q err=%v", img, err)
	}
	if _, err := gis.ImageFor("15"); err == nil {
		t.Errorf("postgis 15 accepted")
	}
}

func TestValidBackupAcceptsDump(t *testing.T) {
	if !ValidBackup("dump") {
		t.Fatal("dump must be a valid backup strategy")
	}
}

func TestKindDumpNeedsCommands(t *testing.T) {
	const base = "image: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: dump\n"
	cases := map[string]struct {
		extra string
		ok    bool
	}{
		"none":         {"", false},
		"only dump":    {"dump_command: [d]\n", false},
		"only restore": {"restore_command: [r]\n", false},
		"both":         {"dump_command: [d]\nrestore_command: [r]\n", true},
		"helper":       {"helper: cvctl-sqlite\nno_container: true\n", true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c, err := LoadFS(fstest.MapFS{"x.yml": {Data: []byte(base + tc.extra)}})
			if !tc.ok {
				if err == nil || code(t, err) != "E_KIND_INVALID" {
					t.Fatalf("want E_KIND_INVALID, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if name == "helper" && (c["x"].Helper != "cvctl-sqlite" || !c["x"].NoContainer) {
				t.Fatalf("kind: %+v", c["x"])
			}
			if name == "both" && (!reflect.DeepEqual(c["x"].DumpCommand, []string{"d"}) || !reflect.DeepEqual(c["x"].RestoreCommand, []string{"r"})) {
				t.Fatalf("kind: %+v", c["x"])
			}
		})
	}
}

// A dump kind's command and health also reach docker argv, in the temporary
// engine of a restore.
func TestDumpKindKeepsPasswordOutOfCommandAndHealth(t *testing.T) {
	const base = "image: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: dump\ndump_command: [d]\nrestore_command: [r]\n"
	for name, tc := range map[string]struct {
		extra string
		ok    bool
	}{
		"clean":   {"command: [run]\nhealth: [CMD, ping]\n", true},
		"command": {"command: [run, \"{password}\"]\n", false},
		"health":  {"health: [CMD, ping, \"{password}\"]\n", false},
		"app":     {"health: [CMD, ping, \"{app_password}\"]\n", false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := LoadFS(fstest.MapFS{"k.yml": {Data: []byte(base + tc.extra)}})
			if (err == nil) != tc.ok {
				t.Fatalf("err: %v", err)
			}
		})
	}
}

// sh -c " " exits 0 with no output, which would store an empty dump as a success.
func TestKindRejectsBlankDumpCommand(t *testing.T) {
	const base = "image: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: dump\n"
	for name, extra := range map[string]string{
		"blank dump":    "dump_command: [\"  \"]\nrestore_command: [r]\n",
		"blank restore": "dump_command: [d]\nrestore_command: [\"\\t\", \" \"]\n",
		"empty string":  "dump_command: [\"\"]\nrestore_command: [r]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadFS(fstest.MapFS{"k.yml": {Data: []byte(base + extra)}}); err == nil {
				t.Fatal("want error")
			}
		})
	}
}

func TestSqliteKind(t *testing.T) {
	k := mustLoad(t)["sqlite"]
	if !k.NoContainer || k.Helper == "" || k.Backup != "dump" {
		t.Fatalf("kind: %+v", k)
	}
	if k.Port != 0 || len(k.Health) != 0 || len(k.External) != 0 || len(k.ContainerEnv) != 0 || len(k.CapAdd) != 0 {
		t.Fatalf("a kind without a container has no port, health, external mode, env or capabilities: %+v", k)
	}
	got := k.LocalVars("STORE", "store", Credentials{})
	if len(got) != 1 || got["STORE_PATH"] != "/data/store/app.db" {
		t.Fatalf("vars: %v", got)
	}
}

func TestNoContainerRequiresHelperAndNoExternal(t *testing.T) {
	base := "name: a\nimage: x:{version}\nversions: [\"1\"]\ndefault_version: \"1\"\nbackup: dump\nno_container: true\n"
	for _, y := range []string{base, base + "helper: h:1\nexternal: [url]\n"} {
		_, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(y)}})
		if got := code(t, err); got != "E_KIND_INVALID" {
			t.Errorf("code = %s", got)
		}
	}
	if _, err := LoadFS(fstest.MapFS{"a.yml": {Data: []byte(base + "helper: h:1\nenv:\n  PATH: /data/{host}/x\n")}}); err != nil {
		t.Fatal(err)
	}
}

func TestCatalogLoadsEngines(t *testing.T) {
	c := mustLoad(t)
	creds := Credentials{User: "app", Password: "p@ss", DB: "appdb"}
	for name, want := range map[string]struct {
		image, defaultVersion, backup, url string
		versions                           []string
	}{
		"mysql":   {"mysql:8.4", "8.4", "dump", "mysql://app:p%40ss@h:3306/appdb", []string{"8.4", "9"}},
		"mariadb": {"mariadb:11.4", "11.4", "dump", "mysql://app:p%40ss@h:3306/appdb", []string{"10.11", "11.4"}},
	} {
		t.Run(name, func(t *testing.T) {
			k, ok := c[name]
			if !ok {
				t.Fatalf("missing kind %s", name)
			}
			if !reflect.DeepEqual(k.Versions, want.versions) || k.DefaultVersion != want.defaultVersion || k.Backup != want.backup {
				t.Errorf("kind: %+v", k)
			}
			if img, err := k.ImageFor(k.DefaultVersion); err != nil || img != want.image {
				t.Errorf("img=%q err=%v", img, err)
			}
			if got := k.LocalVars("X", "h", creds); len(got) != 1 || got["X_URL"] != want.url {
				t.Errorf("vars: %v", got)
			}
			if !reflect.DeepEqual(k.External, []string{"url"}) {
				t.Errorf("external: %v", k.External)
			}
		})
	}
}

// dump_command, restore_command and restore_check run under docker exec, where
// an expanded -p"$X" is the tool's argv and shows in ps.
func TestEngineSecretsNotInCommand(t *testing.T) {
	argvSecret := regexp.MustCompile(`(^|\s)(-p|--password)[=\s"']*\$`)
	for name, k := range mustLoad(t) {
		if k.Backup != "dump" || k.Helper != "" {
			continue
		}
		t.Run(name, func(t *testing.T) {
			for _, part := range [][]string{k.Command, k.Health, k.DumpCommand, k.RestoreCommand, {k.RestoreCheck}} {
				s := strings.Join(part, " ")
				if strings.Contains(s, "{password}") || strings.Contains(s, "{app_password}") {
					t.Errorf("password placeholder in %q", s)
				}
				if argvSecret.MatchString(s) {
					t.Errorf("password in argv: %q", s)
				}
			}
			if strings.TrimSpace(k.RestoreCheck) == "" {
				t.Error("no restore_check")
			}
		})
	}
}

// The image's init runs a temporary server on the socket only: a socket check
// would pass there, and the restore would load into a server about to stop.
func TestMySQLHealthWaitsForTCP(t *testing.T) {
	c := mustLoad(t)
	for _, name := range []string{"mysql", "mariadb"} {
		if s := strings.Join(c[name].Health, " "); !strings.Contains(s, "--protocol=tcp") {
			t.Errorf("%s health: %q", name, s)
		}
	}
}
