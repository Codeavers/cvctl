package secrets

import (
	"archive/tar"
	"bytes"
	"errors"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

type symlinkFS struct {
	*systest.FS
	links map[string]bool
}

func (s symlinkFS) Lstat(p string) (sys.FileInfo, error) {
	fi, err := s.FS.Lstat(p)
	fi.IsSymlink = s.links[p]
	return fi, err
}

func seed(t *testing.T) *systest.FS {
	t.Helper()
	f := systest.NewFS()
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(f.MkdirAll("/etc/cvctl/secrets", 0o750, sys.Owner{User: "root", Group: "deploy"}))
	must(f.WriteFile("/etc/cvctl/host.env", []byte("HOST=a\n"), 0o640, sys.Owner{User: "root", Group: "deploy"}))
	must(f.WriteFile("/etc/cvctl/secrets/restic_password", []byte("pw-secret"), 0o640, sys.Owner{User: "root", Group: "deploy"}))
	must(f.MkdirAll("/srv/deploy/shop-prod", 0o750, sys.Owner{}))
	must(f.WriteFile("/srv/deploy/shop-prod/cvctl.env", []byte("A=1\n"), 0o660, sys.Owner{User: "deployer", Group: "deploy"}))
	must(f.WriteFile("/srv/deploy/shop-prod/app.env", []byte("B=2\n"), 0o600, sys.Owner{User: "deployer", Group: "deploy"}))
	must(f.WriteFile("/srv/deploy/shop-prod/backup-db.env", []byte("C=3\n"), 0o600, sys.Owner{User: "deployer", Group: "deploy"}))
	must(f.WriteFile("/srv/deploy/shop-prod/docker-compose.yml", []byte("ignored"), 0o600, sys.Owner{}))
	must(f.MkdirAll("/var/lib/cvctl/shop-prod", 0o750, sys.Owner{}))
	must(f.WriteFile("/var/lib/cvctl/shop-prod/deploy_key", []byte("PRIVATE"), 0o600, sys.Owner{User: "deployer", Group: "deploy"}))
	must(f.WriteFile("/var/lib/cvctl/shop-prod/deploy_key.pub", []byte("PUB"), 0o644, sys.Owner{User: "deployer", Group: "deploy"}))
	return f
}

func code(err error) string {
	var oe *output.Error
	if errors.As(err, &oe) {
		return oe.Code
	}
	return ""
}

func TestRoundTripPreservesModeOwner(t *testing.T) {
	src := seed(t)
	entries, warns, err := Collect(src, "")
	if err != nil || len(warns) != 0 {
		t.Fatal(err, warns)
	}
	if len(entries) != 7 {
		t.Fatalf("want 7 entries, got %d", len(entries))
	}
	for _, armored := range []bool{false, true} {
		var buf bytes.Buffer
		if err := Encrypt(&buf, entries, "pass", armored); err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(buf.Bytes(), []byte("pw-secret")) {
			t.Fatal("plaintext in ciphertext")
		}
		got, err := Decrypt(&buf, "pass")
		if err != nil {
			t.Fatal(err)
		}
		dst := systest.NewFS()
		paths, err := Restore(dst, "", got, false)
		if err != nil || len(paths) != 7 {
			t.Fatal(err, paths)
		}
		fi, _ := dst.Stat("/srv/deploy/shop-prod/cvctl.env")
		if fi.Mode != 0o660 || fi.User != "deployer" || fi.Group != "deploy" {
			t.Fatalf("%+v", fi)
		}
		b, _ := dst.ReadFile("/var/lib/cvctl/shop-prod/deploy_key")
		if string(b) != "PRIVATE" {
			t.Fatal(string(b))
		}
	}
}

func TestWrongPassphrase(t *testing.T) {
	entries, _, _ := Collect(seed(t), "")
	var buf bytes.Buffer
	if err := Encrypt(&buf, entries, "right", false); err != nil {
		t.Fatal(err)
	}
	_, err := Decrypt(&buf, "wrong")
	if code(err) != "E_SECRETS_DECRYPT" {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "pw-secret") {
		t.Fatal("content echoed")
	}
}

func TestEmptyPassphrase(t *testing.T) {
	if code(Encrypt(&bytes.Buffer{}, nil, "", false)) != "E_SECRETS_PASSPHRASE" {
		t.Fatal("encrypt")
	}
	if _, err := Decrypt(&bytes.Buffer{}, ""); code(err) != "E_SECRETS_PASSPHRASE" {
		t.Fatal("decrypt")
	}
}

func TestRestoreRefusesExistingUnlessForce(t *testing.T) {
	entries, _, _ := Collect(seed(t), "")
	dst := seed(t)
	_ = dst.WriteFile("/etc/cvctl/host.env", []byte("OLD"), 0o600, sys.Owner{})
	if _, err := Restore(dst, "", entries, false); code(err) != "E_SECRETS_EXISTS" {
		t.Fatalf("%v", err)
	}
	if b, _ := dst.ReadFile("/etc/cvctl/host.env"); string(b) != "OLD" {
		t.Fatal("overwritten without --force")
	}
	if _, err := Restore(dst, "", entries, true); err != nil {
		t.Fatal(err)
	}
	if b, _ := dst.ReadFile("/etc/cvctl/host.env"); string(b) != "HOST=a\n" {
		t.Fatal(string(b))
	}
}

func TestRestoreRefusesSymlinkDestination(t *testing.T) {
	f := symlinkFS{FS: seed(t), links: map[string]bool{"/etc/cvctl/host.env": true}}
	entries := []Entry{{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("x")}}
	if _, err := Restore(f, "", entries, true); code(err) != "E_SECRETS_ENTRY_INVALID" {
		t.Fatalf("%v", err)
	}
}

func TestCollectSkipsSymlinks(t *testing.T) {
	f := symlinkFS{FS: seed(t), links: map[string]bool{"/etc/cvctl/secrets/restic_password": true}}
	entries, warns, err := Collect(f, "")
	if err != nil || len(warns) != 1 || len(entries) != 6 {
		t.Fatal(err, warns, len(entries))
	}
	if strings.Contains(warns[0], "pw-secret") {
		t.Fatal("content in warning")
	}
}

func encryptRaw(t *testing.T, hdrs []*tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	rcpt, _ := age.NewScryptRecipient("pass")
	w, _ := age.Encrypt(&buf, rcpt)
	tw := tar.NewWriter(w)
	for _, h := range hdrs {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		_, _ = tw.Write(make([]byte, h.Size))
	}
	_ = tw.Close()
	_ = w.Close()
	return buf.Bytes()
}

func TestDecryptRejectsHostileEntries(t *testing.T) {
	bad := []*tar.Header{
		{Typeflag: tar.TypeReg, Name: "/etc/passwd", Mode: 0o600},
		{Typeflag: tar.TypeReg, Name: "/etc/cvctl/secrets/../../passwd", Mode: 0o600},
		{Typeflag: tar.TypeReg, Name: "etc/cvctl/host.env", Mode: 0o600},
		{Typeflag: tar.TypeReg, Name: "/srv/deploy/Bad_Name/app.env", Mode: 0o600},
		{Typeflag: tar.TypeReg, Name: "/srv/deploy/a-prod/evil.sh", Mode: 0o600},
		{Typeflag: tar.TypeSymlink, Name: "/etc/cvctl/host.env", Linkname: "/etc/shadow"},
	}
	for _, h := range bad {
		_, err := Decrypt(bytes.NewReader(encryptRaw(t, []*tar.Header{h})), "pass")
		if code(err) != "E_SECRETS_ENTRY_INVALID" {
			t.Errorf("%q: %v", h.Name, err)
		}
	}
}

func TestRootPrefix(t *testing.T) {
	dst := systest.NewFS()
	entries := []Entry{{Path: "/etc/cvctl/host.env", Mode: 0o640, User: "root", Group: "deploy", Data: []byte("x")}}
	if _, err := Restore(dst, "/tmp/r", entries, false); err != nil {
		t.Fatal(err)
	}
	if _, err := dst.Stat("/tmp/r/etc/cvctl/host.env"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := Collect(dst, "/tmp/r")
	if len(got) != 1 || got[0].Path != "/etc/cvctl/host.env" {
		t.Fatalf("%+v", got)
	}
}

func TestDearmorStripsPtyNoise(t *testing.T) {
	entries := []Entry{{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("x")}}
	var buf bytes.Buffer
	if err := Encrypt(&buf, entries, "pass", true); err != nil {
		t.Fatal(err)
	}
	noisy := "Passphrase: \r\n" + strings.ReplaceAll(buf.String(), "\n", "\r\n") + "Connection closed\r\n"
	bin, err := Dearmor([]byte(noisy))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Decrypt(bytes.NewReader(bin), "pass"); err != nil || len(got) != 1 {
		t.Fatal(err, got)
	}
	if _, err := Dearmor([]byte("nothing")); code(err) != "E_REMOTE_EXPORT" {
		t.Fatal(err)
	}
}

func TestRestoreUnknownOwnerWritesNothing(t *testing.T) {
	entries := []Entry{
		{Path: "/etc/cvctl/host.env", Mode: 0o600, User: "root", Group: "deploy", Data: []byte("x")},
		{Path: "/etc/cvctl/secrets/a", Mode: 0o600, User: "ghost", Group: "deploy", Data: []byte("y")},
	}
	dst := systest.NewFS()
	_, err := Restore(dst, "", entries, false)
	if code(err) != "E_SYS_OWNER_LOOKUP" || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("%v", err)
	}
	if n := len(dst.Files()); n != 0 {
		t.Fatalf("wrote %d paths", n)
	}
}

func TestCollectWarnsOnNumericOwner(t *testing.T) {
	f := systest.NewFS()
	_ = f.MkdirAll("/etc/cvctl", 0o750, sys.Owner{})
	_ = f.WriteFile("/etc/cvctl/host.env", []byte("x"), 0o600, sys.Owner{User: "1001", Group: "deploy"})
	_, warns, err := Collect(f, "")
	if err != nil || len(warns) != 1 || !strings.Contains(warns[0], "/etc/cvctl/host.env") {
		t.Fatal(err, warns)
	}
}

func TestDecryptCapsTotalSize(t *testing.T) {
	var hdrs []*tar.Header
	for i := 0; i < 17; i++ {
		hdrs = append(hdrs, &tar.Header{Typeflag: tar.TypeReg, Name: "/etc/cvctl/secrets/f" + string(rune('a'+i)), Mode: 0o600, Size: 4 << 20})
	}
	_, err := Decrypt(bytes.NewReader(encryptRaw(t, hdrs)), "pass")
	if code(err) != "E_SECRETS_TOO_LARGE" {
		t.Fatalf("%v", err)
	}
}

func TestRestoreRefusesSymlinkedParent(t *testing.T) {
	base := seed(t)
	f := symlinkFS{FS: base, links: map[string]bool{"/srv/deploy/shop-prod": true}}
	entries := []Entry{
		{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("x")},
		{Path: "/srv/deploy/shop-prod/app.env", Mode: 0o600, Data: []byte("y")},
	}
	_, err := Restore(f, "", entries, true)
	if code(err) != "E_SECRETS_UNSAFE_PATH" {
		t.Fatalf("%v", err)
	}
	if b, _ := base.ReadFile("/etc/cvctl/host.env"); string(b) != "HOST=a\n" {
		t.Fatal("wrote before refusing")
	}
}

func TestRestoreCreatesInstanceDirsOwnedByDeployer(t *testing.T) {
	entries := []Entry{
		{Path: "/srv/deploy/shop-prod/cvctl.env", Mode: 0o640, User: "deployer", Group: "deploy", Data: []byte("A=1\n")},
		{Path: "/var/lib/cvctl/shop-prod/deploy_key", Mode: 0o600, User: "deployer", Group: "deploy", Data: []byte("K")},
	}
	dst := systest.NewFS()
	if _, err := Restore(dst, "", entries, false); err != nil {
		t.Fatal(err)
	}
	for dir, mode := range map[string]uint32{"/srv/deploy/shop-prod": 0o2775, "/var/lib/cvctl/shop-prod": 0o750} {
		fi, err := dst.Stat(dir)
		if err != nil || !fi.IsDir || uint32(fi.Mode) != mode || fi.User != "deployer" || fi.Group != "deploy" {
			t.Fatalf("%s: %+v %v", dir, fi, err)
		}
	}
}

func TestRestoreFixesRootOwnedInstanceDir(t *testing.T) {
	dst := systest.NewFS()
	if err := dst.MkdirAll("/srv/deploy/shop-prod", 0o750, sys.Owner{User: "root", Group: "root"}); err != nil {
		t.Fatal(err)
	}
	entries := []Entry{{Path: "/srv/deploy/shop-prod/cvctl.env", Mode: 0o640, User: "deployer", Group: "deploy", Data: []byte("A=1\n")}}
	if _, err := Restore(dst, "", entries, false); err != nil {
		t.Fatal(err)
	}
	fi, _ := dst.Stat("/srv/deploy/shop-prod")
	if fi.Mode != 0o2775 || fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("%+v", fi)
	}
}

func TestRestoreLooksUpDeployOwnerBeforeWriting(t *testing.T) {
	dst := systest.NewFS()
	delete(dst.Users, "deployer")
	entries := []Entry{{Path: "/srv/deploy/shop-prod/cvctl.env", Mode: 0o640, User: "root", Group: "root", Data: []byte("A=1\n")}}
	if _, err := Restore(dst, "", entries, false); code(err) != "E_SYS_OWNER_LOOKUP" {
		t.Fatalf("%v", err)
	}
	if n := len(dst.Files()); n != 0 {
		t.Fatalf("wrote %d paths", n)
	}
}
