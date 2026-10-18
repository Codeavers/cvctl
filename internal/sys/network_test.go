package sys_test

import (
	"context"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestEnsureNetwork(t *testing.T) {
	ctx := context.Background()
	t.Run("exists", func(t *testing.T) {
		r := &systest.Runner{}
		r.On("docker network inspect -f {{.Internal}} a_edge", sys.Result{})
		if err := sys.EnsureNetwork(ctx, r, "a_edge", false); err != nil {
			t.Fatal(err)
		}
		if len(r.Calls) != 1 {
			t.Fatalf("calls: %v", r.Calls)
		}
	})
	t.Run("missing internal", func(t *testing.T) {
		r := &systest.Runner{}
		r.On("docker network inspect -f {{.Internal}} a_backup", sys.Result{ExitCode: 1})
		r.On("docker network create --label cvctl.managed=true --internal a_backup", sys.Result{})
		if err := sys.EnsureNetwork(ctx, r, "a_backup", true); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("race", func(t *testing.T) {
		r := &systest.Runner{}
		r.On("docker network inspect -f {{.Internal}} a_edge", sys.Result{ExitCode: 1})
		r.On("docker network create --label cvctl.managed=true a_edge", sys.Result{ExitCode: 1, Stderr: []byte("network with name a_edge already exists")})
		if err := sys.EnsureNetwork(ctx, r, "a_edge", false); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		r := &systest.Runner{}
		r.On("docker network inspect -f {{.Internal}} a_edge", sys.Result{ExitCode: 1})
		r.On("docker network create --label cvctl.managed=true a_edge", sys.Result{ExitCode: 1, Stderr: []byte("boom")})
		if err := sys.EnsureNetwork(ctx, r, "a_edge", false); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("err: %v", err)
		}
	})
}

func TestConnectNetwork(t *testing.T) {
	ctx := context.Background()
	const cmd = "docker network connect --alias a--web a_edge cvctl-traefik"
	r := &systest.Runner{}
	r.On(cmd, sys.Result{})
	if err := sys.ConnectNetwork(ctx, r, "a_edge", "cvctl-traefik", "a--web"); err != nil {
		t.Fatal(err)
	}
	r = &systest.Runner{}
	r.On(cmd, sys.Result{ExitCode: 1, Stderr: []byte("endpoint with name cvctl-traefik already exists in network a_edge")})
	if err := sys.ConnectNetwork(ctx, r, "a_edge", "cvctl-traefik", "a--web"); err != nil {
		t.Fatal(err)
	}
	r = &systest.Runner{}
	r.On(cmd, sys.Result{ExitCode: 1, Stderr: []byte("no such container")})
	if err := sys.ConnectNetwork(ctx, r, "a_edge", "cvctl-traefik", "a--web"); err == nil {
		t.Fatal("expected error")
	}
}

func TestEnsureNetworkRejectsInternalMismatch(t *testing.T) {
	r := &systest.Runner{}
	r.On("docker network inspect -f {{.Internal}} a_backup", sys.Result{Stdout: []byte("true\n")})
	err := sys.EnsureNetwork(context.Background(), r, "a_backup", false)
	if err == nil || !strings.Contains(err.Error(), "internal=true") {
		t.Fatalf("want an internal mismatch error, got %v", err)
	}
	r2 := &systest.Runner{}
	r2.On("docker network inspect -f {{.Internal}} a_backup", sys.Result{Stdout: []byte("false\n")})
	if err := sys.EnsureNetwork(context.Background(), r2, "a_backup", false); err != nil {
		t.Fatal(err)
	}
}
