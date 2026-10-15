package systest

import (
	"context"
	"errors"
	"io/fs"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
)

func TestFakeRunnerRecordsAndResponds(t *testing.T) {
	r := &Runner{}
	res, err := r.Run(context.Background(), sys.Cmd{Name: "nope", Args: []string{"x"}})
	if err != nil || res.ExitCode != 127 {
		t.Fatalf("unregistered: %+v %v", res, err)
	}
	r.On("id -u", sys.Result{Stdout: []byte("0")})
	res, _ = r.Run(context.Background(), sys.Cmd{Name: "id", Args: []string{"-u"}})
	if string(res.Stdout) != "0" || len(r.Calls) != 2 {
		t.Fatalf("got %+v calls=%d", res, len(r.Calls))
	}
}

func TestFakeRunnerExactBeatsPrefixLongestWins(t *testing.T) {
	r := &Runner{}
	r.OnPrefix("docker", sys.Result{ExitCode: 1})
	r.OnPrefix("docker network", sys.Result{ExitCode: 2})
	r.On("docker network ls", sys.Result{ExitCode: 3})
	for cmd, want := range map[string]int{"docker ps": 1, "docker network create x": 2, "docker network ls": 3} {
		c := sys.Cmd{Name: "docker", Args: splitArgs(cmd)}
		res, _ := r.Run(context.Background(), c)
		if res.ExitCode != want {
			t.Errorf("%s: got %d want %d", cmd, res.ExitCode, want)
		}
	}
}

func splitArgs(s string) []string {
	var out []string
	cur := ""
	for _, ch := range s {
		if ch == ' ' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(ch)
	}
	out = append(out, cur)
	return out[1:]
}

func TestFakeFSWriteStat(t *testing.T) {
	f := NewFS()
	if err := f.WriteFile("/etc/x", []byte("hi"), 0o640, sys.Owner{User: "root", Group: "deploy"}); err != nil {
		t.Fatal(err)
	}
	fi, err := f.Stat("/etc/x")
	if err != nil || fi.Mode != 0o640 || fi.User != "root" || fi.Group != "deploy" || fi.IsDir {
		t.Fatalf("%+v %v", fi, err)
	}
	b, _ := f.ReadFile("/etc/x")
	if string(b) != "hi" {
		t.Fatal(string(b))
	}
	if _, ok := f.Files()["/etc/x"]; !ok {
		t.Fatal("Files missing entry")
	}
}

func TestFakeFSMissing(t *testing.T) {
	f := NewFS()
	if _, err := f.ReadFile("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err := f.Stat("/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	if err := f.Remove("/nope"); err != nil {
		t.Fatal(err)
	}
}

func TestFakeFSMkdirAllAndOwnerKeep(t *testing.T) {
	f := NewFS()
	_ = f.MkdirAll("/srv/a/b", 0o750, sys.Owner{User: "deployer"})
	fi, _ := f.Stat("/srv/a/b")
	if !fi.IsDir || fi.Mode != 0o750 || fi.User != "deployer" {
		t.Fatalf("%+v", fi)
	}
	_ = f.MkdirAll("/srv/a/b", 0o700, sys.Owner{})
	fi, _ = f.Stat("/srv/a/b")
	if fi.Mode != 0o700 || fi.User != "deployer" {
		t.Fatalf("empty owner must leave unchanged: %+v", fi)
	}
}
