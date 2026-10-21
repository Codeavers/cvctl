package secrets

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"reflect"
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

func TestAllowedRejectsDoubleHyphen(t *testing.T) {
	if Allowed("/srv/deploy/shop--prod/cvctl.env") {
		t.Fatal("expected Allowed to reject path with double hyphen in instance name")
	}
	if !Allowed("/srv/deploy/shop-prod/cvctl.env") {
		t.Fatal("expected Allowed to accept valid path")
	}
}

func TestRoundTripKeepsInstanceBackupKeys(t *testing.T) {
	src := seed(t)
	env := "CVCTL_BACKUP_ACCESS_KEY=bk-shop-prod\nCVCTL_BACKUP_SECRET_KEY=s3cr3t\nCVCTL_BACKUP_LIBSODIUM_KEY=abcd\nCVCTL_BACKUP_RESTIC_PASSWORD=pw\nCVCTL_SVC_DB_BACKUP_KEY_GEN=3\n"
	if err := src.WriteFile("/srv/deploy/shop-prod/cvctl.env", []byte(env), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatal(err)
	}
	if err := src.WriteFile("/etc/cvctl/secrets/walg_libsodium_key", []byte("hostkey"), 0o640, sys.Owner{User: "root", Group: "deploy"}); err != nil {
		t.Fatal(err)
	}
	entries, _, err := Collect(src, "")
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := Encrypt(&buf, entries, "pass", false); err != nil {
		t.Fatal(err)
	}
	got, err := Decrypt(&buf, "pass")
	if err != nil {
		t.Fatal(err)
	}
	dst := systest.NewFS()
	if _, err := Restore(dst, "", got, false); err != nil {
		t.Fatal(err)
	}
	b, _ := dst.ReadFile("/srv/deploy/shop-prod/cvctl.env")
	if string(b) != env {
		t.Fatalf("cvctl.env changed:\n%s", b)
	}
	// Old generations stay readable: the host key travels too.
	if k, _ := dst.ReadFile("/etc/cvctl/secrets/walg_libsodium_key"); string(k) != "hostkey" {
		t.Fatalf("host key: %q", k)
	}
}

func TestCollectHost(t *testing.T) {
	src := seed(t)
	entries, warns, err := CollectHost(src, "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 host entries, got %d", len(entries))
	}
	want := []string{"/etc/cvctl/host.env", "/etc/cvctl/secrets/restic_password"}
	for i, w := range want {
		if entries[i].Path != w {
			t.Errorf("entry %d: want %s, got %s", i, w, entries[i].Path)
		}
	}
}

func TestCollectInstance(t *testing.T) {
	src := seed(t)
	entries, warns, err := CollectInstance(src, "", "shop-prod")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(warns) != 0 {
		t.Fatalf("unexpected warnings: %v", warns)
	}
	// seed has app.env, backup-db.env, cvctl.env, docker-compose.yml (ignored),
	// and the deploy keys under /var/lib/cvctl.
	want := []string{
		"/srv/deploy/shop-prod/app.env",
		"/srv/deploy/shop-prod/backup-db.env",
		"/srv/deploy/shop-prod/cvctl.env",
		"/var/lib/cvctl/shop-prod/deploy_key",
		"/var/lib/cvctl/shop-prod/deploy_key.pub",
	}
	if len(entries) != len(want) {
		t.Fatalf("want %d instance entries, got %d: %v", len(want), len(entries), entries)
	}
	for i, w := range want {
		if entries[i].Path != w {
			t.Errorf("entry %d: want %s, got %s", i, w, entries[i].Path)
		}
	}

	// Missing instance
	if _, _, err := CollectInstance(src, "", "missing-prod"); code(err) != "E_SECRETS_NO_INSTANCE" {
		t.Fatalf("want E_SECRETS_NO_INSTANCE, got %v", err)
	}

	// Invalid instance name
	for _, bad := range []string{"shop--prod", "Shop-prod", "shop_prod", "-shop", "shop-", ""} {
		if _, _, err := CollectInstance(src, "", bad); code(err) != "E_INSTANCE_NAME" {
			t.Errorf("%q: want E_INSTANCE_NAME, got %v", bad, err)
		}
	}

	// Instance path is a file, not a directory
	_ = src.WriteFile("/srv/deploy/not-a-dir", []byte("file"), 0o640, sys.Owner{})
	if _, _, err := CollectInstance(src, "", "not-a-dir"); code(err) != "E_SECRETS_NO_INSTANCE" {
		t.Fatalf("want E_SECRETS_NO_INSTANCE for non-dir, got %v", err)
	}

	// A symlinked instance dir must fail, not yield an empty bundle that looks valid.
	sym := symlinkFS{FS: src, links: map[string]bool{"/srv/deploy/shop-prod": true}}
	if _, _, err := CollectInstance(sym, "", "shop-prod"); code(err) != "E_SECRETS_UNSAFE_PATH" {
		t.Fatalf("want E_SECRETS_UNSAFE_PATH for symlinked instance dir, got %v", err)
	}
}

func TestPackUnpackRoundTrip(t *testing.T) {
	src := seed(t)
	entries, _, err := Collect(src, "")
	if err != nil {
		t.Fatal(err)
	}
	packed, err := Pack(entries)
	if err != nil {
		t.Fatalf("Pack failed: %v", err)
	}
	got, err := Unpack(packed)
	if err != nil {
		t.Fatalf("Unpack failed: %v", err)
	}
	if !reflect.DeepEqual(got, entries) {
		t.Fatalf("Unpack mismatch:\ngot:  %+v\nwant: %+v", got, entries)
	}
}

func TestPackOutputEqualsPlaintextTarInsideBundle(t *testing.T) {
	src := seed(t)
	entries, _, err := Collect(src, "")
	if err != nil {
		t.Fatal(err)
	}
	packed, err := Pack(entries)
	if err != nil {
		t.Fatal(err)
	}

	// Prove that Encrypt streams the exact same tar payload into age
	var buf bytes.Buffer
	if err := Encrypt(&buf, entries, "pass", false); err != nil {
		t.Fatal(err)
	}
	ciphertext := buf.Bytes()

	id, err := age.NewScryptIdentity("pass")
	if err != nil {
		t.Fatal(err)
	}
	dr, err := age.Decrypt(bytes.NewReader(ciphertext), id)
	if err != nil {
		t.Fatal(err)
	}
	tarInside, err := io.ReadAll(dr)
	if err != nil {
		t.Fatal(err)
	}

	if !bytes.Equal(tarInside, packed) {
		t.Fatal("Pack output does not match the plaintext tar inside the age bundle")
	}

	// And Decrypt returns the same entries
	decrypted, err := Decrypt(bytes.NewReader(ciphertext), "pass")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decrypted, entries) {
		t.Fatal("Decrypt output does not match original entries")
	}
}

func TestPackRejectsInvalidEntries(t *testing.T) {
	// Not allowed path
	if _, err := Pack([]Entry{{Path: "/etc/shadow", Mode: 0o600, Data: []byte("x")}}); code(err) != "E_SECRETS_ENTRY_INVALID" {
		t.Fatalf("want E_SECRETS_ENTRY_INVALID, got %v", err)
	}

	// Duplicate path
	dups := []Entry{
		{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("a")},
		{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: []byte("b")},
	}
	if _, err := Pack(dups); code(err) != "E_SECRETS_ENTRY_INVALID" {
		t.Fatalf("want E_SECRETS_ENTRY_INVALID, got %v", err)
	}

	// Exceeds max entry size
	huge := []Entry{{Path: "/etc/cvctl/host.env", Mode: 0o600, Data: make([]byte, maxEntrySize+1)}}
	if _, err := Pack(huge); code(err) != "E_SECRETS_CORRUPT" {
		t.Fatalf("want E_SECRETS_CORRUPT, got %v", err)
	}

	// Exceeds max entries
	many := make([]Entry, maxEntries+1)
	for i := range many {
		many[i] = Entry{Path: "/etc/cvctl/host.env", Mode: 0o600}
	}
	if _, err := Pack(many); code(err) != "E_SECRETS_CORRUPT" {
		t.Fatalf("want E_SECRETS_CORRUPT for >maxEntries, got %v", err)
	}

	// Exceeds total size
	var largeEntries []Entry
	for i := 0; i < 17; i++ {
		largeEntries = append(largeEntries, Entry{
			Path: "/etc/cvctl/secrets/k" + string(rune('a'+i)),
			Mode: 0o600,
			Data: make([]byte, 4<<20),
		})
	}
	if _, err := Pack(largeEntries); code(err) != "E_SECRETS_TOO_LARGE" {
		t.Fatalf("want E_SECRETS_TOO_LARGE, got %v", err)
	}
}

func TestPartialBundleRestore(t *testing.T) {
	src := seed(t)

	// Host-only bundle
	hostEntries, _, err := CollectHost(src, "")
	if err != nil {
		t.Fatal(err)
	}
	dstHost := systest.NewFS()
	restoredHost, err := Restore(dstHost, "", hostEntries, false)
	if err != nil {
		t.Fatalf("host-only restore: %v", err)
	}
	if len(restoredHost) != 2 {
		t.Fatalf("want 2 restored host files, got %d", len(restoredHost))
	}
	if _, err := dstHost.Stat("/etc/cvctl/host.env"); err != nil {
		t.Errorf("host.env missing: %v", err)
	}
	if _, err := dstHost.Stat("/etc/cvctl/secrets/restic_password"); err != nil {
		t.Errorf("restic_password missing: %v", err)
	}

	// Instance-only bundle
	instEntries, _, err := CollectInstance(src, "", "shop-prod")
	if err != nil {
		t.Fatal(err)
	}
	dstInst := systest.NewFS()
	restoredInst, err := Restore(dstInst, "", instEntries, false)
	if err != nil {
		t.Fatalf("instance-only restore: %v", err)
	}
	if len(restoredInst) != 5 {
		t.Fatalf("want 5 restored instance files (3 env, 2 deploy key), got %d", len(restoredInst))
	}
	if _, err := dstInst.Stat("/var/lib/cvctl/shop-prod/deploy_key"); err != nil {
		t.Errorf("deploy_key missing: %v", err)
	}
	fi, err := dstInst.Stat("/srv/deploy/shop-prod")
	if err != nil || !fi.IsDir || fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("deploy dir not created with deployer:deploy: %+v %v", fi, err)
	}
	if _, err := dstInst.Stat("/srv/deploy/shop-prod/cvctl.env"); err != nil {
		t.Errorf("cvctl.env missing: %v", err)
	}
	if _, err := dstInst.Stat("/srv/deploy/shop-prod/app.env"); err != nil {
		t.Errorf("app.env missing: %v", err)
	}
	if _, err := dstInst.Stat("/srv/deploy/shop-prod/backup-db.env"); err != nil {
		t.Errorf("backup-db.env missing: %v", err)
	}
	// Verify host secrets were not created
	if _, err := dstInst.Stat("/etc/cvctl/host.env"); err == nil {
		t.Error("host.env should not exist in instance restore")
	}
}

const goldenBundle = `-----BEGIN AGE ENCRYPTED FILE-----
YWdlLWVuY3J5cHRpb24ub3JnL3YxCi0+IHNjcnlwdCBVMjBFZFA3VHd1ODdmbEFT
RUVOYjdnIDE4Ci9kVzE0TFF6WjRpTEpSYjdJcFU5eTd0b1pwbVBmNnNDajNjdFJl
UXdUdE0KLS0tIEJPc3BUQXFXNVZGV3I3MHlzaW5xL0pEeGNHN1gxZGQ5Rlh3cVAr
MGoycnMKsfPdpzY1i1SFujZfedD54oHff3F1uPDG1oQ4YzEV5mhw1TQr58Ng3ypF
QkvuJLNgmbqTTfDhw+r6Yx2rzE2fMp6kHsZPEuOkv4w/e4arZVg477GUiFg8U00W
E1jQbx3DIrVg0QibMT/9ogNnkzAskyrHSvuHXcDFpc6yagPLH0EQlArxynvo9+0J
JuQknCutraqwBGigdvm/1deVtyvaWpo5R0535Hqc6TXKAzb1AvIGEj0UuzViJFAX
3iZ07cVbb5r+E+RuY02wei6MoYrmiNaXzd8TqKt3my5oWOzoVpacv05KTeG1F/RG
Hd4iQxv/UGzB8lb01TSiEarwqrBvF/nypS82v5iFUU3NO1Hn0dZwr2h2iEuIXltG
SjBGsQ9a/1rB71Tn2TyHgiouI7Jg2r9hy40Y8oAfkKm+XQia36svj0PxM0XQphww
bHsfzSxixgXsR8DWtnWzOqlPRN1QU5XsKo/BJcZ6V7CHLCj7M3TbacxPo7OxwaGD
gCAxmGfjOHuzz4WosHJnUpcLmfi2/KovLBFwWiOjg5c62IkDdJE3vUhGzO8cNYXh
a5rNfLbe6q2kANOo0l8zFRZp/MOL5zxQR1QP+UcXuqfH4ahO8P3R2BhNHgKyqLkr
siArlUd7Mm+5zvqHywyceKcBWHSjsUt24CBWhhVMgu8lbVa9s9OSLrHDwcLC0D/K
DeXFYz+/uYyzdPuVpddqO3oOAE59GgBY6SWhwuOgYTBYFyybUPbW5pWYHTs7rB3B
3bhIWTeynb/IeUy/215ubF8SfqWm/RXYUIz1O4XoL4hXKooBPXYeWYtGbSJc1SWp
Odf2Vqxr2MWcJICklkS0gtRGcAzKUWr5KCUMOIsHqZF/mlTkBB7H/FoBLzyMYRBz
v2MPLNyWRnOWLg/qmNQnGmeLWJakmTDB0kqJSFFgJnYgXLrfbeI2/cA14KEqIvXN
WDxfX+Kh6fYvtr2m9CG96lqMu83Yp8hhScNqzssNbbxODW/sbwRtN9GzYbZB1M8a
AFQgM4tx44nGWnYHkGwfRKFTuZo2Uwn99d6zaP6SKlBwed95FHDVbrTf1bn8MPyZ
5vFs8DJnzOtrIIyRBq1HsewgMJ3mVs3SHVIAPCcPvzVg1fpgTn03PnHE36wVE+iw
s1K/1Bw1W6ZI/g0EX4mKKjJT71S/4NyE2Ca/E2Z3vhuy1CYiiwOPd3BRgqqgnnSh
4/F6jmWdFtEQ/znMqjhb9edmyJB+xYqL88PSfIEYJRyJhx6FX7uva//GiMnk+Lez
wz3py5aSTPn9LoYd2+6zCK3QRLypbJbnnPHlcrZ/HTa8Tvmp755eIJTU0q2Ij+Rx
P303aEiydOylb23iE1OeqSJRhHA450p4JNq8ZOTtfPOJJRnJIrWQBGht5PB4vcwq
+Cldr8HZJOarmWC2ByB/RR5lDN6tA3nAV/+4tqPp6NjudxCYEqRdOSF4/ZCckFfw
5FXCbatuZQ80OfKIBgPrQHykF32hXUbFbI3XoJes8ww/TSbBCeJRY94snyZZAio1
//KQ2RAe1Av5ns+JHDmWXqp2tjTLrH6zbbcf3GiQ5ztThdJiinkRhqfkZlrHCzwe
gICGbe+6dvEyvKf2YmOSHZLW0a6Dc0UZEIKrD7gy3QdbriEXGR6I2nbXVUaorpAL
LalCCEI7gpzA6n4nv6OIsVFLFyxs7yWddyZrfyxZUjFptxRs0j3qJI4pUcNaZrFd
JcN7ZG+wn7PnSc1hrYfD96Cf+DKTLzkG7RoORuKbTC2TxVnokGErk+y5jumhfD++
oge+4TqgI1Z192IgIWLrjjclB6j/qzHcXiht5+9pgzxmLOodun7+z4IQCI6zD+yE
6hEHd66XyxAGDc/mhUZO8G7wzCGLoGUhtoYhNvRSvWQi8p11MEBgoXchBaSMMVV3
mXF8lQM3hAKoFhVREEhAWVrVPl5q2Z7Q82v19KJHAkKzPXrZQb6N+mCfiRY5V4zz
A+Ht8hGjri7Oz/4tZ6BFC2tBQl0Iay6sxQPYFn8AjKmyRvAxq0JZpTEzkzd4CEO5
TGHM5S5dYfsPSUwA4N1y4Ee74ORp4y+m/rx0SIAE21rUdAIOEbjnjKIwaZlCrB2d
yZo8kHGFplizFvo9Qa2cLy/eanee5d/vsJfWEGHg8O8TLHfCrEbFWoD3tGChduPZ
WceuQJgP+RCnnCdtjxmnOYlK6XYRW3ZXbngU8EuV72wG+rSmLT1alVTDbNJZJlId
lmPCrUuYuPaO9UEI3VSs1g/rtlyvJySgacMYQHFoofOO2DmNv9Zz+Yg5VptLFLc/
jmcKci2TDfVxniBsARsDuUfkeLdH5exfOMLinMJNAHzUT/K9Us12chO4rt2+OZOI
ivjH2Co6SgRBypePsR2yaYVCU6/dcjsjlbQyweedYm5RsVZWnhdosCE/qq5kpjUt
10sE7HnEPtrGYLpaM+pfR+RibJDndwd9H05kHgI5vGGHHHMIv/Op1VdmeafcLTJg
IvDksLdhVlifge4nqylTZmzu6vldNO1FytU2o9bI5TbDu4b+94Fh/N7Ts8yjU1KS
i9dnOJzUJugFJc18ixPMlU4ZgAfnX7JVgkNi3MTNDvda5zL9/TWb1ZP17YRcJ23G
qwduoMwxta+VZ4hBdS0SZDRz24RaxGWANwliJzSfVZYHt61999c8f+8loVrG57sY
wpF5mAFDbTm7oJDskMfFVUYRjmCF24N6AtdefMU1vYFrFSuH2heforc65i/9Lb8d
RARTYRBGR+yQbdpyxBYviXvqGFpTOw==
-----END AGE ENCRYPTED FILE-----`

func TestExistingGoldenBundleDecrypts(t *testing.T) {
	got, err := Decrypt(strings.NewReader(goldenBundle), "pass")
	if err != nil {
		t.Fatalf("failed to decrypt golden bundle: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want 1 entry from golden bundle, got %d", len(got))
	}
	if got[0].Path != "/etc/cvctl/host.env" || string(got[0].Data) != "HOST=a\n" || got[0].User != "root" || got[0].Group != "deploy" || got[0].Mode != 0o640 {
		t.Fatalf("unexpected entry: %+v", got[0])
	}
}
