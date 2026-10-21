package host

import (
	"context"
	"errors"
	"testing"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func preflightEnv(osrelease string, euid int) *Env {
	fs := systest.NewFS()
	if osrelease != "" {
		_ = fs.WriteFile("/etc/os-release", []byte(osrelease), 0o644, sys.Owner{User: "root", Group: "root"})
	}
	return &Env{FS: fs, EUID: euid}
}

const ubuntu2404 = "NAME=\"Ubuntu\"\nID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION_CODENAME=noble\n"

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

func TestPreflightNewerReleases(t *testing.T) {
	for _, rel := range []string{"ID=ubuntu\nVERSION_ID=\"25.10\"\nVERSION_CODENAME=questing\n", "ID=ubuntu\nVERSION_ID=\"26.04\"\nVERSION_CODENAME=resolute\n"} {
		if _, err := (PreflightStep{}).Check(context.Background(), preflightEnv(rel, 0)); err != nil {
			t.Fatalf("%q: %v", rel, err)
		}
	}
}

func TestPreflightNotUbuntu(t *testing.T) {
	for _, rel := range []string{"ID=debian\nVERSION_ID=\"12\"\n", "ID=ubuntu\nVERSION_ID=\"22.04\"\nVERSION_CODENAME=jammy\n", "ID=ubuntu\nVERSION_ID=\"26.04\"\n", ""} {
		_, err := PreflightStep{}.Check(context.Background(), preflightEnv(rel, 0))
		if c := code(t, err); c != "E_HOST_UNSUPPORTED" {
			t.Fatalf("%q: %s", rel, c)
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
