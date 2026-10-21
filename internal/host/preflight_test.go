package host

import (
	"context"
	"errors"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func seedOS(fs *systest.FS, osrelease string) {
	if osrelease != "" {
		_ = fs.WriteFile("/etc/os-release", []byte(osrelease), 0o644, sys.Owner{User: "root", Group: "root"})
	}
}

func preflightEnv(osrelease string, euid int) *Env {
	fs := systest.NewFS()
	seedOS(fs, osrelease)
	_ = fs.MkdirAll("/run/systemd/system", 0o755, sys.Owner{User: "root", Group: "root"})
	return &Env{FS: fs, EUID: euid}
}

const ubuntu2404 = "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n"

const (
	debian12 = "ID=debian\nVERSION_ID=\"12\"\nVERSION_CODENAME=bookworm\n"
	mint     = "ID=linuxmint\nID_LIKE=\"ubuntu debian\"\nVERSION_ID=\"22\"\nVERSION_CODENAME=wilma\nUBUNTU_CODENAME=noble\n"
	arch     = "ID=arch\n"
	fedora   = "ID=fedora\nVERSION_ID=\"41\"\n"
)

func code(t *testing.T, err error) string {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want output.Error, got %v", err)
	}
	if oe.Hint == "" {
		t.Fatal("empty hint")
	}
	return oe.Code
}

func TestPreflightOK(t *testing.T) {
	p, err := PreflightStep{}.Check(context.Background(), preflightEnv(ubuntu2404, 0))
	if err != nil || len(p.Changes) != 0 || p.Skip != "" {
		t.Fatalf("got %+v %v", p, err)
	}
}

func TestPreflightAcceptsAnyDistro(t *testing.T) {
	for _, rel := range []string{
		"ID=ubuntu\nVERSION_ID=\"22.04\"\nVERSION_CODENAME=jammy\n",
		"ID=ubuntu\nVERSION_ID=\"25.10\"\nVERSION_CODENAME=questing\n",
		"ID=ubuntu\nVERSION_ID=\"26.04\"\nVERSION_CODENAME=resolute\n",
		debian12,
		mint,
		arch,
		fedora,
		"",
	} {
		if _, err := (PreflightStep{}).Check(context.Background(), preflightEnv(rel, 0)); err != nil {
			t.Fatalf("%q: %v", rel, err)
		}
	}
}

func TestPreflightNoSystemd(t *testing.T) {
	env := preflightEnv(ubuntu2404, 0)
	_ = env.FS.Remove("/run/systemd/system")
	_, err := PreflightStep{}.Check(context.Background(), env)
	if c := code(t, err); c != "E_HOST_UNSUPPORTED" {
		t.Fatal(c)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Message != "cvctl init needs a Linux host with systemd" {
		t.Fatalf("message %q", oe.Message)
	}
}

func TestHostOSInfo(t *testing.T) {
	cases := []struct {
		name, rel, distro, codename string
		apt, repo                   bool
	}{
		{"ubuntu", ubuntu2404, "ubuntu", "noble", true, true},
		{"debian", debian12, "debian", "bookworm", true, true},
		{"mint", mint, "ubuntu", "noble", true, true},
		{"arch", arch, "", "", false, false},
		{"fedora", fedora, "", "", false, false},
		{"missing", "", "", "", false, false},
	}
	for _, tc := range cases {
		fs := systest.NewFS()
		seedOS(fs, tc.rel)
		o, err := hostOS(fs)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		d, c, ok := o.dockerRepo()
		if o.apt() != tc.apt || ok != tc.repo || d != tc.distro || c != tc.codename {
			t.Errorf("%s: apt=%v repo=(%q,%q,%v)", tc.name, o.apt(), d, c, ok)
		}
	}
}

func TestPreflightNotRoot(t *testing.T) {
	_, err := PreflightStep{}.Check(context.Background(), preflightEnv(ubuntu2404, 1000))
	if c := code(t, err); c != "E_HOST_NOT_ROOT" {
		t.Fatal(c)
	}
	var oe *output.Error
	errors.As(err, &oe)
	if oe.Hint != "sudo cvctl init" {
		t.Fatalf("hint %q", oe.Hint)
	}
}
