package compose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

func TestHealthOK(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	psCmd := p.Cmd("ps", "--format", "json")
	runner.On(psCmd.String(), sys.Result{
		Stdout:   []byte(`[{"Service":"api","Name":"demo-dev-api-1","State":"running"}]`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{json .NetworkSettings.Networks}} demo-dev-api-1", sys.Result{
		Stdout:   []byte(`{"demo-dev_app": {"IPAddress": "127.0.0.1"}}`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{.RestartCount}} demo-dev-api-1", sys.Result{
		Stdout:   []byte("0\n"),
		ExitCode: 0,
	})

	ctx := WithHealthInterval(context.Background(), 5*time.Millisecond)
	h := contract.HealthSpec{
		Service: "api",
		HTTP:    "/health",
		Timeout: 500 * time.Millisecond,
	}

	if err := Health(ctx, runner, p, h, port, ts.Client()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHealthTimeout(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer ts.Close()

	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatal(err)
	}

	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	psCmd := p.Cmd("ps", "--format", "json")
	runner.On(psCmd.String(), sys.Result{
		Stdout:   []byte(`[{"Service":"api","Name":"demo-dev-api-1","State":"running"}]`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{json .NetworkSettings.Networks}} demo-dev-api-1", sys.Result{
		Stdout:   []byte(`{"demo-dev_app": {"IPAddress": "127.0.0.1"}}`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{.RestartCount}} demo-dev-api-1", sys.Result{
		Stdout:   []byte("0\n"),
		ExitCode: 0,
	})

	ctx := WithHealthInterval(context.Background(), 5*time.Millisecond)
	h := contract.HealthSpec{
		Service: "api",
		HTTP:    "/health",
		Timeout: 40 * time.Millisecond,
	}

	err = Health(ctx, runner, p, h, port, ts.Client())
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_HEALTH_FAILED" {
		t.Fatalf("expected code E_HEALTH_FAILED, got %q", outErr.Code)
	}
}

func TestHealthRestartCount(t *testing.T) {
	runner := &systest.Runner{}
	p := Project{
		Run:      runner,
		Instance: "demo-dev",
		Dir:      "/srv/deploy/demo-dev",
	}

	psCmd := p.Cmd("ps", "--format", "json")
	runner.On(psCmd.String(), sys.Result{
		Stdout:   []byte(`[{"Service":"api","Name":"demo-dev-api-1","State":"running"}]`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{json .NetworkSettings.Networks}} demo-dev-api-1", sys.Result{
		Stdout:   []byte(`{"demo-dev_app": {"IPAddress": "127.0.0.1"}}`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{.RestartCount}} demo-dev-api-1", sys.Result{
		Stdout:   []byte("2\n"),
		ExitCode: 0,
	})

	ctx := WithHealthInterval(context.Background(), 5*time.Millisecond)
	h := contract.HealthSpec{
		Service: "api",
		HTTP:    "/health",
		Timeout: 500 * time.Millisecond,
	}

	err := Health(ctx, runner, p, h, 3000, nil)
	if err == nil {
		t.Fatal("expected error for restarted container, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_HEALTH_FAILED" {
		t.Fatalf("expected code E_HEALTH_FAILED, got %q", outErr.Code)
	}
	wantSubstr := "container demo-dev-api-1 restarted"
	if !strings.Contains(outErr.Message, wantSubstr) {
		t.Fatalf("expected message to contain %q, got %q", wantSubstr, outErr.Message)
	}
}
