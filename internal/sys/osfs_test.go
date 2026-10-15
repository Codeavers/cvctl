package sys

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestOSFSWriteAtomicMode(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f.conf")
	f := NewOSFS()
	if err := f.WriteFile(p, []byte("a"), 0o640, Owner{}); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile(p, []byte("b"), 0o640, Owner{}); err != nil {
		t.Fatal(err)
	}
	fi, err := f.Stat(p)
	if err != nil || fi.Mode != 0o640 {
		t.Fatalf("%+v %v", fi, err)
	}
	if b, _ := f.ReadFile(p); string(b) != "b" {
		t.Fatal(string(b))
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 1 {
		t.Fatalf("leftover files: %v", ents)
	}
}

func TestOSFSMissingAndRemove(t *testing.T) {
	f := NewOSFS()
	p := filepath.Join(t.TempDir(), "nope")
	if _, err := f.ReadFile(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := f.Stat(p); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := f.Remove(p); err != nil {
		t.Fatal(err)
	}
}

func TestOSFSMkdirAllKeepsSetgid(t *testing.T) {
	f := NewOSFS()
	p := filepath.Join(t.TempDir(), "a", "b")
	if err := f.MkdirAll(p, 0o2750, Owner{}); err != nil {
		t.Fatal(err)
	}
	fi, _ := f.Stat(p)
	if !fi.IsDir || fi.Mode != 0o2750 {
		t.Fatalf("%+v", fi)
	}
	if err := f.MkdirAll(p, 0o700, Owner{}); err != nil {
		t.Fatal(err)
	}
	fi, _ = f.Stat(p)
	if fi.Mode != 0o700 {
		t.Fatalf("existing dir mode not fixed: %+v", fi)
	}
}

func TestOSFSUnknownOwner(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root")
	}
	f := NewOSFS()
	if err := f.WriteFile(filepath.Join(t.TempDir(), "x"), nil, 0o600, Owner{User: "no-such-user-xyz"}); err != nil {
		t.Fatalf("unprivileged must skip chown silently: %v", err)
	}
}

func TestCmdString(t *testing.T) {
	if s := (Cmd{Name: "a", Args: []string{"b", "c"}}).String(); s != "a b c" {
		t.Fatal(s)
	}
	if s := (Cmd{Name: "a"}).String(); s != "a" {
		t.Fatal(s)
	}
}

func TestExecRunnerExitCode(t *testing.T) {
	res, err := NewExecRunner().Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "exit 3"}})
	if err != nil || res.ExitCode != 3 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestExecRunnerStdoutStdinAndStartFailure(t *testing.T) {
	r := NewExecRunner()
	res, err := r.Run(context.Background(), Cmd{Name: "cat", Stdin: []byte("hey")})
	if err != nil || string(res.Stdout) != "hey" {
		t.Fatalf("%+v %v", res, err)
	}
	if _, err := r.Run(context.Background(), Cmd{Name: "definitely-not-a-binary-xyz"}); err == nil {
		t.Fatal("expected start error")
	}
}
