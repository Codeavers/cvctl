package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/secrets"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func secretsEnv(t *testing.T, fsys sys.FS, r sys.Runner) {
	t.Helper()
	of, or, on := secretsFS, secretsRunner, secretsNow
	secretsFS = func() sys.FS { return fsys }
	secretsRunner = func() sys.Runner { return r }
	secretsNow = func() time.Time { return time.Date(2026, 10, 17, 0, 0, 0, 0, time.UTC) }
	t.Cleanup(func() { secretsFS, secretsRunner, secretsNow = of, or, on })
	t.Setenv("CVCTL_SECRETS_PASSPHRASE", "pass")
}

func seedSecrets(t *testing.T) *systest.FS {
	t.Helper()
	f := systest.NewFS()
	_ = f.MkdirAll("/etc/cvctl/secrets", 0o750, sys.Owner{User: "root", Group: "deploy"})
	_ = f.WriteFile("/etc/cvctl/host.env", []byte("HOST=a\n"), 0o640, sys.Owner{User: "root", Group: "deploy"})
	_ = f.WriteFile("/etc/cvctl/secrets/restic_password", []byte("pw-secret"), 0o640, sys.Owner{User: "root", Group: "deploy"})
	return f
}

func TestSecretsExportImportRoundTrip(t *testing.T) {
	src := seedSecrets(t)
	secretsEnv(t, src, &systest.Runner{})
	before := len(src.Files())
	code, _, errb := run(t, "secrets", "export", "-o", "/out/b.age")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if len(src.Files()) != before+1 {
		t.Fatalf("export wrote more than the bundle: %v", src.Files())
	}
	fi, _ := src.Stat("/out/b.age")
	if fi.Mode != 0o600 {
		t.Fatalf("mode %o", fi.Mode)
	}
	bundle, _ := src.ReadFile("/out/b.age")
	if bytes.Contains(bundle, []byte("pw-secret")) {
		t.Fatal("plaintext in bundle")
	}

	dst := systest.NewFS()
	_ = dst.WriteFile("/in/b.age", bundle, 0o600, sys.Owner{})
	secretsEnv(t, dst, &systest.Runner{})
	code, out, errb := run(t, "secrets", "import", "/in/b.age")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	if !strings.Contains(out, "/etc/cvctl/secrets/restic_password") || strings.Contains(out, "pw-secret") {
		t.Fatal(out)
	}
	if code, _, _ := run(t, "secrets", "import", "/in/b.age"); code != 1 {
		t.Fatalf("second import without --force: %d", code)
	}
	if code, _, _ := run(t, "secrets", "import", "--force", "/in/b.age"); code != 0 {
		t.Fatalf("force: %d", code)
	}
}

func TestSecretsImportWrongPassphrase(t *testing.T) {
	src := seedSecrets(t)
	secretsEnv(t, src, &systest.Runner{})
	run(t, "secrets", "export", "-o", "/b.age")
	t.Setenv("CVCTL_SECRETS_PASSPHRASE", "nope")
	code, out, _ := run(t, "secrets", "import", "--json", "/b.age")
	if code == 0 {
		t.Fatal("expected failure")
	}
	if !strings.Contains(out, "E_SECRETS_DECRYPT") || strings.Contains(out, "pw-secret") {
		t.Fatal(out)
	}
}

func TestSecretsRootRequiresEnv(t *testing.T) {
	secretsEnv(t, seedSecrets(t), &systest.Runner{})
	if code, _, _ := run(t, "secrets", "export", "--root", "/x", "-o", "-"); code != 2 {
		t.Fatalf("code %d", code)
	}
	t.Setenv("CVCTL_ALLOW_SECRETS_ROOT", "1")
	if code, _, _ := run(t, "secrets", "export", "--root", "/x", "-o", "-"); code != 0 {
		t.Fatalf("code %d", code)
	}
}

func TestSecretsImportRoot(t *testing.T) {
	src := seedSecrets(t)
	secretsEnv(t, src, &systest.Runner{})
	if code, _, errb := run(t, "secrets", "export", "-o", "/out/b.age"); code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	bundle, _ := src.ReadFile("/out/b.age")
	dst := systest.NewFS()
	_ = dst.WriteFile("/in/b.age", bundle, 0o600, sys.Owner{})
	secretsEnv(t, dst, &systest.Runner{})

	code, out, _ := run(t, "secrets", "import", "--root", "/tmp/r", "--json", "/in/b.age")
	if code != 2 || !strings.Contains(out, "E_USAGE_ROOT") {
		t.Fatalf("without the env var: %d %s", code, out)
	}
	if len(dst.Files()) != 1 {
		t.Fatalf("rejected import wrote files: %v", dst.Files())
	}

	t.Setenv("CVCTL_ALLOW_SECRETS_ROOT", "1")
	code, out, errb := run(t, "secrets", "import", "--root", "/tmp/r", "/in/b.age")
	if code != 0 || !strings.Contains(out, "/etc/cvctl/secrets/restic_password") {
		t.Fatalf("with the env var: %d %s %s", code, out, errb)
	}
	got, err := dst.ReadFile("/tmp/r/etc/cvctl/secrets/restic_password")
	if err != nil || string(got) != "pw-secret" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestRemoteExport(t *testing.T) {
	var armored bytes.Buffer
	if err := secrets.Encrypt(&armored, []secrets.Entry{{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("x")}}, "pass", true); err != nil {
		t.Fatal(err)
	}
	r := &systest.Runner{}
	noisy := "Passphrase: \r\n" + strings.ReplaceAll(armored.String(), "\n", "\r\n")
	r.On("ssh -t -p 2228 ops@srv.example.com sudo cvctl secrets export -o -", sys.Result{Stdout: []byte(noisy)})
	fsys := systest.NewFS()
	secretsEnv(t, fsys, r)
	code, _, errb := run(t, "remote", "export", "srv.example.com", "--port", "2228", "--user", "ops")
	if code != 0 {
		t.Fatalf("%d %s", code, errb)
	}
	name := "cvctl-secrets-srv.example.com-2026-10-17.age"
	fi, err := fsys.Stat(name)
	if err != nil || fi.Mode != 0o600 {
		t.Fatalf("%+v %v", fi, err)
	}
	bin, _ := fsys.ReadFile(name)
	if got, err := secrets.Decrypt(bytes.NewReader(bin), "pass"); err != nil || len(got) != 1 {
		t.Fatal(err)
	}
}

func TestRemoteExportRejectsBadHostAndFailure(t *testing.T) {
	r := &systest.Runner{}
	secretsEnv(t, systest.NewFS(), r)
	if code, _, _ := run(t, "remote", "export", "-oProxyCommand=x"); code != 2 {
		t.Fatalf("code %d", code)
	}
	if code, _, _ := run(t, "remote", "export", "good.host"); code != 1 {
		t.Fatalf("code %d", code)
	}
}

func TestSecretsExportScopeFlags(t *testing.T) {
	src := seedSecrets(t)
	_ = src.MkdirAll("/srv/deploy/shop-prod", 0o750, sys.Owner{})
	_ = src.WriteFile("/srv/deploy/shop-prod/cvctl.env", []byte("A=1\n"), 0o660, sys.Owner{User: "deployer", Group: "deploy"})
	_ = src.WriteFile("/srv/deploy/shop-prod/app.env", []byte("B=2\n"), 0o600, sys.Owner{User: "deployer", Group: "deploy"})
	secretsEnv(t, src, &systest.Runner{})

	// Export only instance
	code, out, errb := run(t, "secrets", "export", "-o", "/out/inst.age", "--instance", "shop-prod")
	if code != 0 {
		t.Fatalf("instance export failed (%d): %s %s", code, out, errb)
	}
	bundle, _ := src.ReadFile("/out/inst.age")
	dstInst := systest.NewFS()
	_ = dstInst.WriteFile("/in/inst.age", bundle, 0o600, sys.Owner{})
	secretsEnv(t, dstInst, &systest.Runner{})
	code, out, errb = run(t, "secrets", "import", "/in/inst.age")
	if code != 0 {
		t.Fatalf("instance import failed (%d): %s %s", code, out, errb)
	}
	if !strings.Contains(out, "/srv/deploy/shop-prod/cvctl.env") || strings.Contains(out, "host.env") {
		t.Fatalf("instance import output unexpected: %s", out)
	}
	if _, err := dstInst.Stat("/srv/deploy/shop-prod/cvctl.env"); err != nil {
		t.Errorf("cvctl.env missing: %v", err)
	}
	if _, err := dstInst.Stat("/etc/cvctl/host.env"); err == nil {
		t.Errorf("host.env should not exist in instance import")
	}

	// Export only host
	secretsEnv(t, src, &systest.Runner{})
	code, out, errb = run(t, "secrets", "export", "-o", "/out/host.age", "--host")
	if code != 0 {
		t.Fatalf("host export failed (%d): %s %s", code, out, errb)
	}
	hBundle, _ := src.ReadFile("/out/host.age")
	dstHost := systest.NewFS()
	_ = dstHost.WriteFile("/in/host.age", hBundle, 0o600, sys.Owner{})
	secretsEnv(t, dstHost, &systest.Runner{})
	code, out, errb = run(t, "secrets", "import", "/in/host.age")
	if code != 0 {
		t.Fatalf("host import failed (%d): %s %s", code, out, errb)
	}
	if !strings.Contains(out, "/etc/cvctl/host.env") || strings.Contains(out, "cvctl.env") {
		t.Fatalf("host import output unexpected: %s", out)
	}
	if _, err := dstHost.Stat("/etc/cvctl/host.env"); err != nil {
		t.Errorf("host.env missing: %v", err)
	}
	if _, err := dstHost.Stat("/srv/deploy/shop-prod/cvctl.env"); err == nil {
		t.Errorf("cvctl.env should not exist in host import")
	}

	// Mutually exclusive flags
	secretsEnv(t, src, &systest.Runner{})
	code, out, errb = run(t, "secrets", "export", "-o", "/out/both.age", "--instance", "shop-prod", "--host", "--json")
	if code != 2 || !strings.Contains(out+errb, "E_USAGE_SCOPE") {
		t.Fatalf("expected E_USAGE_SCOPE (exit 2), got %d out=%q errb=%q", code, out, errb)
	}

	// Invalid instance name
	code, out, errb = run(t, "secrets", "export", "-o", "/out/bad.age", "--instance", "shop--prod", "--json")
	if code != 2 || !strings.Contains(out+errb, "E_INSTANCE_NAME") {
		t.Fatalf("expected E_INSTANCE_NAME (exit 2), got %d out=%q errb=%q", code, out, errb)
	}

	// Non-existent instance
	code, out, errb = run(t, "secrets", "export", "-o", "/out/missing.age", "--instance", "missing-prod", "--json")
	if code != 1 || !strings.Contains(out+errb, "E_SECRETS_NO_INSTANCE") {
		t.Fatalf("expected E_SECRETS_NO_INSTANCE (exit 1), got %d out=%q errb=%q", code, out, errb)
	}
}

func TestRemoteExportScopeFlags(t *testing.T) {
	var armored bytes.Buffer
	if err := secrets.Encrypt(&armored, []secrets.Entry{{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("x")}}, "pass", true); err != nil {
		t.Fatal(err)
	}
	noisy := "Passphrase: \r\n" + strings.ReplaceAll(armored.String(), "\n", "\r\n")

	// Remote export with --host
	r := &systest.Runner{}
	r.On("ssh -t -p 22 srv.example.com sudo cvctl secrets export -o - --host", sys.Result{Stdout: []byte(noisy)})
	fsys := systest.NewFS()
	secretsEnv(t, fsys, r)
	code, _, errb := run(t, "remote", "export", "srv.example.com", "--host")
	if code != 0 {
		t.Fatalf("remote export --host failed (%d): %s", code, errb)
	}

	// Remote export with --instance
	r = &systest.Runner{}
	r.On("ssh -t -p 22 srv.example.com sudo cvctl secrets export -o - --instance shop-prod", sys.Result{Stdout: []byte(noisy)})
	fsys = systest.NewFS()
	secretsEnv(t, fsys, r)
	code, _, errb = run(t, "remote", "export", "srv.example.com", "--instance", "shop-prod")
	if code != 0 {
		t.Fatalf("remote export --instance failed (%d): %s", code, errb)
	}

	// Remote export with mutually exclusive flags
	code, out, errb := run(t, "remote", "export", "srv.example.com", "--instance", "shop-prod", "--host", "--json")
	if code != 2 || !strings.Contains(out+errb, "E_USAGE_SCOPE") {
		t.Fatalf("expected E_USAGE_SCOPE (exit 2), got %d out=%q errb=%q", code, out, errb)
	}

	// Remote export with invalid instance name
	code, out, errb = run(t, "remote", "export", "srv.example.com", "--instance", "shop--prod", "--json")
	if code != 2 || !strings.Contains(out+errb, "E_INSTANCE_NAME") {
		t.Fatalf("expected E_INSTANCE_NAME (exit 2), got %d out=%q errb=%q", code, out, errb)
	}
}
