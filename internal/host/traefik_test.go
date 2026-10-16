package host

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"
	"gopkg.in/yaml.v3"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

var update = flag.Bool("update", false, "rewrite golden files")

func checkTraefikGolden(t *testing.T, modeDir, fileName string, got []byte) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("%s/%s: rendered empty content", modeDir, fileName)
	}
	dir := filepath.Join("testdata", "golden", modeDir)
	p := filepath.Join(dir, fileName)
	if *update {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read golden %s: %v", p, err)
	}
	if string(want) != string(got) {
		t.Fatalf("%s differs\n--- got\n%s\n--- want\n%s", p, got, want)
	}
}

func TestTraefikGoldens(t *testing.T) {
	cases := []struct {
		dir string
		cfg *hostcfg.Config
	}{
		{
			dir: "traefik-edge-http",
			cfg: &hostcfg.Config{
				EdgeMode:      "edge",
				AcmeEmail:     "acme@example.com",
				AcmeChallenge: "http",
			},
		},
		{
			dir: "traefik-edge-route53",
			cfg: &hostcfg.Config{
				EdgeMode:      "edge",
				AcmeEmail:     "acme@example.com",
				AcmeChallenge: "route53",
			},
		},
		{
			dir: "traefik-behind-proxy",
			cfg: &hostcfg.Config{
				EdgeMode:  "behind-proxy",
				ProxyPort: 8081,
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.dir, func(t *testing.T) {
			staticBytes, err := renderTraefikStatic(tc.cfg)
			if err != nil {
				t.Fatalf("renderTraefikStatic: %v", err)
			}
			checkTraefikGolden(t, tc.dir, "traefik.yml", staticBytes)

			dynBytes, err := renderTraefikDynamic(tc.cfg)
			if err != nil {
				t.Fatalf("renderTraefikDynamic: %v", err)
			}
			checkTraefikGolden(t, tc.dir, "_cvctl.yml", dynBytes)

			composeBytes, err := renderTraefikCompose(tc.cfg)
			if err != nil {
				t.Fatalf("renderTraefikCompose: %v", err)
			}
			checkTraefikGolden(t, tc.dir, "compose.yml", composeBytes)
		})
	}
}

func TestTraefikNoDockerSocket(t *testing.T) {
	modes := []*hostcfg.Config{
		{EdgeMode: "edge", AcmeEmail: "acme@example.com", AcmeChallenge: "http"},
		{EdgeMode: "edge", AcmeEmail: "acme@example.com", AcmeChallenge: "route53"},
		{EdgeMode: "edge", AcmeEmail: "acme@example.com", AcmeChallenge: "cloudflare"},
		{EdgeMode: "behind-proxy", ProxyPort: 8081},
	}

	for _, cfg := range modes {
		composeBytes, err := renderTraefikCompose(cfg)
		if err != nil {
			t.Fatalf("renderTraefikCompose(%+v): %v", cfg, err)
		}
		if len(composeBytes) == 0 {
			t.Fatalf("renderTraefikCompose(%+v) returned empty bytes", cfg)
		}
		if strings.Contains(string(composeBytes), "docker.sock") {
			t.Fatalf("compose for mode %s/%s contains docker.sock:\n%s", cfg.EdgeMode, cfg.AcmeChallenge, composeBytes)
		}
		var parsed map[string]any
		if err := yaml.Unmarshal(composeBytes, &parsed); err != nil {
			t.Fatalf("invalid compose YAML: %v", err)
		}
		if _, ok := parsed["services"]; !ok {
			t.Fatalf("compose YAML missing services section:\n%s", composeBytes)
		}

		cp, err := loader.LoadWithContext(context.Background(), types.ConfigDetails{
			WorkingDir: ".",
			ConfigFiles: []types.ConfigFile{
				{Filename: "compose.yml", Content: composeBytes},
			},
			Environment: types.Mapping{},
		}, func(o *loader.Options) {
			o.SkipInterpolation = true
			o.SkipConsistencyCheck = true
			o.SkipResolveEnvironment = true
			o.SetProjectName(traefikProject, true)
		})
		if err != nil {
			t.Fatalf("compose-go load failed for mode %s/%s: %v", cfg.EdgeMode, cfg.AcmeChallenge, err)
		}
		for _, vol := range cp.Services["traefik"].Volumes {
			if strings.Contains(vol.Source, "docker.sock") || strings.Contains(vol.Target, "docker.sock") {
				t.Fatalf("service volume mounts docker.sock: %+v", vol)
			}
		}

		staticBytes, err := renderTraefikStatic(cfg)
		if err != nil {
			t.Fatalf("renderTraefikStatic(%+v): %v", cfg, err)
		}
		if len(staticBytes) == 0 {
			t.Fatalf("renderTraefikStatic(%+v) returned empty bytes", cfg)
		}
		if strings.Contains(string(staticBytes), "docker.sock") {
			t.Fatalf("static config for mode %s/%s contains docker.sock:\n%s", cfg.EdgeMode, cfg.AcmeChallenge, staticBytes)
		}
	}
}

func TestTraefikSecretsNotInCompose(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	_, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	const awsID = "AKIAIOSFODNN7EXAMPLE"
	const awsSecret = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
	const cfToken = "cf_super_secret_dns_token_123"
	sec.Set("route53_access_key", awsID)
	sec.Set("route53_secret_key", awsSecret)
	sec.Set("cloudflare_dns_token", cfToken)

	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "route53",
	}
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On("docker network inspect cvctl-edge", sys.Result{Stdout: []byte("[{}]\n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik-traefik-1", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes on unconfigured host, got none")
	}

	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("Apply error: %v", err)
	}

	for _, p := range []string{traefikComposePath, traefikStaticPath, traefikDynamicPath} {
		data, err := fs.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		if strings.Contains(string(data), awsID) || strings.Contains(string(data), awsSecret) {
			t.Fatalf("%s must not contain AWS credentials:\n%s", p, data)
		}
	}

	envBytes, err := fs.ReadFile(traefikSecretsPath)
	if err != nil {
		t.Fatalf("read %s: %v", traefikSecretsPath, err)
	}
	wantEnv := "AWS_ACCESS_KEY_ID=" + awsID + "\nAWS_REGION=us-east-1\nAWS_SECRET_ACCESS_KEY=" + awsSecret + "\n"
	if string(envBytes) != wantEnv {
		t.Fatalf("traefik.env mismatch:\ngot:\n%s\nwant:\n%s", envBytes, wantEnv)
	}
	fi, err := fs.Stat(traefikSecretsPath)
	if err != nil {
		t.Fatalf("stat %s: %v", traefikSecretsPath, err)
	}
	if fi.Mode != 0o640 || fi.User != "root" || fi.Group != "deploy" {
		t.Fatalf("traefik.env meta: got mode=%04o owner=%s:%s, want 0640 root:deploy", fi.Mode, fi.User, fi.Group)
	}

	// Switching challenge to cloudflare writes CF_DNS_API_TOKEN to traefik.env.
	cfg.AcmeChallenge = "cloudflare"
	planCF, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check (cloudflare) error: %v", err)
	}
	if _, err := step.Apply(context.Background(), env, planCF); err != nil {
		t.Fatalf("Apply (cloudflare) error: %v", err)
	}
	cfBytes, err := fs.ReadFile(traefikSecretsPath)
	if err != nil {
		t.Fatalf("read %s (cloudflare): %v", traefikSecretsPath, err)
	}
	if string(cfBytes) != "CF_DNS_API_TOKEN="+cfToken+"\n" {
		t.Fatalf("cloudflare traefik.env mismatch: got %q", string(cfBytes))
	}

	// Switching challenge to http removes stale traefik.env.
	cfg.AcmeChallenge = "http"
	planHTTP, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check (http) error: %v", err)
	}
	if _, err := step.Apply(context.Background(), env, planHTTP); err != nil {
		t.Fatalf("Apply (http) error: %v", err)
	}
	if _, err := fs.Stat(traefikSecretsPath); err == nil {
		t.Fatal("expected stale traefik.env to be removed when switching to http challenge")
	}
}

func TestTraefikUnchanged(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	staticBytes, err := renderTraefikStatic(cfg)
	if err != nil || len(staticBytes) == 0 {
		t.Fatalf("renderTraefikStatic failed: len=%d err=%v", len(staticBytes), err)
	}
	dynBytes, err := renderTraefikDynamic(cfg)
	if err != nil || len(dynBytes) == 0 {
		t.Fatalf("renderTraefikDynamic failed: len=%d err=%v", len(dynBytes), err)
	}
	composeBytes, err := renderTraefikCompose(cfg)
	if err != nil || len(composeBytes) == 0 {
		t.Fatalf("renderTraefikCompose failed: len=%d err=%v", len(composeBytes), err)
	}

	_ = fs.MkdirAll(traefikDir, 0o750, rootDeployOwner)
	_ = fs.MkdirAll("/etc/traefik", 0o755, rootOwner)
	_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)
	_ = fs.WriteFile(traefikStaticPath, staticBytes, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikDynamicPath, dynBytes, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikComposePath, composeBytes, 0o640, rootDeployOwner)

	r.On("docker network inspect cvctl-edge", sys.Result{Stdout: []byte("[{}]\n"), ExitCode: 0})
	psJSON := `{"Service":"traefik","State":"running","Image":"` + traefikImage + `"}` + "\n"
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{
		Stdout:   []byte(psJSON),
		ExitCode: 0,
	})

	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("unexpected Check error: %v", err)
	}
	if len(plan.Changes) != 0 {
		t.Fatalf("expected no changes when Traefik is already in desired state, got %v", plan.Changes)
	}

	callsBeforeApply := len(r.Calls)
	pending, err := step.Apply(context.Background(), env, plan)
	if err != nil || pending != "" {
		t.Fatalf("unexpected Apply result: pending=%q err=%v", pending, err)
	}
	if len(r.Calls) != callsBeforeApply {
		t.Fatalf("expected no commands during Apply when unchanged, got %v", r.Calls[callsBeforeApply:])
	}
}

func TestTraefikCreatesNetworkOnce(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)

	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On("docker network inspect cvctl-edge", sys.Result{ExitCode: 1, Stderr: []byte("Error: No such network: cvctl-edge\n")})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker network create cvctl-edge", sys.Result{Stdout: []byte("net-id\n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik-traefik-1", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
	plan1, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("first Check error: %v", err)
	}
	if len(plan1.Changes) == 0 {
		t.Fatal("expected changes on first Check, got none")
	}

	if _, err := step.Apply(context.Background(), env, plan1); err != nil {
		t.Fatalf("first Apply error: %v", err)
	}

	countCreate := func() int {
		n := 0
		for _, c := range r.Calls {
			if c.String() == "docker network create cvctl-edge" {
				n++
			}
		}
		return n
	}
	if got := countCreate(); got != 1 {
		t.Fatalf("expected 1 call to 'docker network create cvctl-edge' after first Apply, got %d", got)
	}

	// Verify directory and file permissions set by Apply.
	if fi, err := fs.Stat("/etc/traefik"); err != nil || !fi.IsDir || fi.Mode != 0o755 || fi.User != "root" || fi.Group != "root" {
		t.Fatalf("/etc/traefik meta: got %+v err=%v, want dir 0755 root:root", fi, err)
	}
	if fi, err := fs.Stat("/etc/traefik/dynamic"); err != nil || !fi.IsDir || fi.Mode != 0o2775 || fi.User != "deployer" || fi.Group != "deploy" {
		t.Fatalf("/etc/traefik/dynamic meta must remain untouched: got %+v err=%v", fi, err)
	}
	if fi, err := fs.Stat(traefikDir); err != nil || !fi.IsDir || fi.Mode != 0o750 || fi.User != "root" || fi.Group != "deploy" {
		t.Fatalf("%s meta: got %+v err=%v, want dir 0750 root:deploy", traefikDir, fi, err)
	}

	// Now network exists, but container is stopped so Apply runs again.
	r.On("docker network inspect cvctl-edge", sys.Result{Stdout: []byte("[{}]\n"), ExitCode: 0})
	plan2, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("second Check error: %v", err)
	}
	if len(plan2.Changes) == 0 {
		t.Fatal("expected changes when container is not running, got none")
	}
	if _, err := step.Apply(context.Background(), env, plan2); err != nil {
		t.Fatalf("second Apply error: %v", err)
	}
	if got := countCreate(); got != 1 {
		t.Fatalf("expected 'docker network create cvctl-edge' not to be called again, got %d total calls", got)
	}
}

func TestTraefikUnhealthy(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On("docker network inspect cvctl-edge", sys.Result{Stdout: []byte("[{}]\n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik-traefik-1", sys.Result{Stdout: []byte("starting\n"), ExitCode: 0})
	r.On("docker logs --tail 20 cvctl-traefik-traefik-1", sys.Result{
		Stderr:   []byte("ERR command traefik error: port 80 already in use\n"),
		ExitCode: 0,
	})

	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 5 * time.Millisecond}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_TRAEFIK_UNHEALTHY error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %T (%v)", err, err)
	}
	if oe.Code != "E_TRAEFIK_UNHEALTHY" {
		t.Fatalf("got code %q, want E_TRAEFIK_UNHEALTHY", oe.Code)
	}
	if !strings.Contains(oe.Message, "port 80 already in use") {
		t.Fatalf("expected logs in error message, got %q", oe.Message)
	}
}

func TestTraefikCommandFailure(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On("docker network inspect cvctl-edge", sys.Result{Stdout: []byte("[{}]\n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{
		Stderr:   []byte("driver failed programming external connectivity\n"),
		ExitCode: 1,
	})

	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_TRAEFIK_FAILED error, got nil")
	}

	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("want *output.Error, got %T (%v)", err, err)
	}
	if oe.Code != "E_TRAEFIK_FAILED" {
		t.Fatalf("got code %q, want E_TRAEFIK_FAILED", oe.Code)
	}
	if !strings.Contains(oe.Message, "driver failed programming external connectivity") {
		t.Fatalf("expected stderr in message, got %q", oe.Message)
	}
	if !strings.Contains(oe.Hint, "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d") {
		t.Fatalf("expected command in hint, got %q", oe.Hint)
	}
}

func TestTraefikRegisteredInDefaultSteps(t *testing.T) {
	steps := DefaultSteps()
	if len(steps) < 9 {
		t.Fatalf("expected at least 9 steps in DefaultSteps, got %d", len(steps))
	}
	if steps[7].Name() != "unattended-upgrades" {
		t.Fatalf("step 7: got %q, want unattended-upgrades", steps[7].Name())
	}
	if steps[8].Name() != "traefik" {
		t.Fatalf("step 8: got %q, want traefik", steps[8].Name())
	}
}
