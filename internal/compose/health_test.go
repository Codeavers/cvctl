package compose

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
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

func TestHealthRedirectNotFollowed(t *testing.T) {
	tsTarget := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("OK"))
	}))
	defer tsTarget.Close()

	tsRedirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tsTarget.URL, http.StatusFound)
	}))
	defer tsRedirect.Close()

	u, err := url.Parse(tsRedirect.URL)
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

	err = Health(ctx, runner, p, h, port, nil)
	if err == nil {
		t.Fatal("expected health check to fail when server redirects, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok || outErr.Code != "E_HEALTH_FAILED" {
		t.Fatalf("expected E_HEALTH_FAILED, got %v", err)
	}
}

func TestHealthServerSleepTimeoutPerRequest(t *testing.T) {
	var attempts atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		select {
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		case <-r.Context().Done():
		}
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
		Timeout: 3500 * time.Millisecond,
	}

	err = Health(ctx, runner, p, h, port, nil)
	if err == nil {
		t.Fatal("expected timeout error, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok || outErr.Code != "E_HEALTH_FAILED" {
		t.Fatalf("expected E_HEALTH_FAILED, got %v", err)
	}
	if got := attempts.Load(); got < 2 {
		t.Fatalf("expected at least 2 attempts (per-request timeout + polling), got %d", got)
	}
}

func TestNormalizeProject(t *testing.T) {
	if got := normalizeProject("Foo.Bar"); got != "foobar" {
		t.Fatalf("normalizeProject(Foo.Bar) = %q, want foobar", got)
	}
	if got := normalizeProject("Foo.Bar") + "_app"; got != "foobar_app" {
		t.Fatalf("network = %q, want foobar_app", got)
	}
	if got := normalizeProject("My_Project-1.2@Live"); got != "my_project-12live" {
		t.Fatalf("normalizeProject(My_Project-1.2@Live) = %q, want my_project-12live", got)
	}
}

func TestHealthNormalizedNetwork(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Instance: "Foo.Bar",
		Dir:      "/srv/deploy/Foo.Bar",
	}

	psCmd := p.Cmd("ps", "--format", "json")
	runner.On(psCmd.String(), sys.Result{
		Stdout:   []byte(`[{"Service":"api","Name":"foobar-api-1","State":"running"}]`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{json .NetworkSettings.Networks}} foobar-api-1", sys.Result{
		Stdout:   []byte(`{"foobar_app": {"IPAddress": "127.0.0.1"}}`),
		ExitCode: 0,
	})

	runner.On("docker inspect -f {{.RestartCount}} foobar-api-1", sys.Result{
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
		t.Fatalf("unexpected error with normalized network: %v", err)
	}
}

func TestHealthRestartCountUnknown(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		Stdout:   []byte("unknown\n"),
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
		t.Fatal("expected error when restart count is unknown, got nil")
	}
	outErr, ok := err.(*output.Error)
	if !ok {
		t.Fatalf("expected *output.Error, got %T: %v", err, err)
	}
	if outErr.Code != "E_HEALTH_FAILED" {
		t.Fatalf("expected code E_HEALTH_FAILED, got %q", outErr.Code)
	}
}
