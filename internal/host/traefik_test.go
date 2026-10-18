package host

import (
	"context"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"slices"
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

const (
	proxyInspectCmd = "docker network inspect -f {{range .IPAM.Config}}{{.Gateway}} {{end}} cvctl-edge-proxy"
	proxyCreateCmd  = "docker network create --label cvctl.managed=true --subnet 10.203.0.0/24 --gateway 10.203.0.1 cvctl-edge-proxy"
)

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
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
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

	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

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
}

func TestTraefikSwitchDNSToHTTPRemovesEnvAndRecreates(t *testing.T) {
	r := &systest.Runner{}
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
	fs := systest.NewFS()
	_, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}

	// Start with files rendered for route53 on disk, including traefik.env.
	dnsCfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "route53",
	}
	staticDNS, err := renderTraefikStatic(dnsCfg)
	if err != nil {
		t.Fatal(err)
	}
	dynDNS, err := renderTraefikDynamic(dnsCfg)
	if err != nil {
		t.Fatal(err)
	}
	composeDNS, err := renderTraefikCompose(dnsCfg)
	if err != nil {
		t.Fatal(err)
	}
	_ = fs.MkdirAll(traefikDir, 0o750, rootDeployOwner)
	_ = fs.MkdirAll("/etc/traefik", 0o755, rootOwner)
	_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)
	_ = fs.MkdirAll("/etc/cvctl/secrets", 0o750, rootDeployOwner)
	_ = fs.WriteFile(traefikStaticPath, staticDNS, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikDynamicPath, dynDNS, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikComposePath, composeDNS, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikSecretsPath, []byte("AWS_ACCESS_KEY_ID=AKIA123\nAWS_REGION=us-east-1\nAWS_SECRET_ACCESS_KEY=secret\n"), 0o640, rootDeployOwner)

	// Switch host config to HTTP challenge.
	httpCfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	env := &Env{Run: r, FS: fs, Cfg: httpCfg, Secrets: sec, EUID: 0}

	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
	psJSON := `{"Service":"traefik","State":"running","Image":"` + traefikImage + `"}` + "\n"
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{
		Stdout:   []byte(psJSON),
		ExitCode: 0,
	})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	wantRemove := "remove " + traefikSecretsPath
	foundRemove := false
	for _, c := range plan.Changes {
		if c == wantRemove {
			foundRemove = true
			break
		}
	}
	if !foundRemove {
		t.Fatalf("expected Check to report %q, got %v", wantRemove, plan.Changes)
	}

	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("Apply error: %v", err)
	}
	if _, err := fs.Stat(traefikSecretsPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be removed, stat err=%v", traefikSecretsPath, err)
	}

	recreated := false
	for _, c := range r.Calls {
		if c.String() == "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate" {
			recreated = true
		}
		if c.String() == "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d" {
			t.Fatal("expected --force-recreate when removing traefik.env, but plain up -d was called")
		}
	}
	if !recreated {
		t.Fatal("expected docker compose up -d --force-recreate to be called")
	}
}

func TestTraefikRecreateOnlyWhenStaticOrEnvChanges(t *testing.T) {
	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	staticBytes, err := renderTraefikStatic(cfg)
	if err != nil {
		t.Fatal(err)
	}
	dynBytes, err := renderTraefikDynamic(cfg)
	if err != nil {
		t.Fatal(err)
	}
	composeBytes, err := renderTraefikCompose(cfg)
	if err != nil {
		t.Fatal(err)
	}
	psJSON := `{"Service":"traefik","State":"running","Image":"` + traefikImage + `"}` + "\n"

	t.Run("traefik.yml changed uses --force-recreate", func(t *testing.T) {
		r := &systest.Runner{}
		r.OnPrefix(legacyUsersPrefix, noSuchNet)
		fs := systest.NewFS()
		_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
		env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

		_ = fs.MkdirAll(traefikDir, 0o750, rootDeployOwner)
		_ = fs.MkdirAll("/etc/traefik", 0o755, rootOwner)
		_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)
		_ = fs.WriteFile(traefikStaticPath, []byte("# stale static config\n"), 0o640, rootDeployOwner)
		_ = fs.WriteFile(traefikDynamicPath, dynBytes, 0o640, rootDeployOwner)
		_ = fs.WriteFile(traefikComposePath, composeBytes, 0o640, rootDeployOwner)

		r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
		r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(psJSON), ExitCode: 0})
		r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})
		r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

		step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
		plan, err := step.Check(context.Background(), env)
		if err != nil {
			t.Fatalf("Check error: %v", err)
		}
		if len(plan.Changes) != 1 || plan.Changes[0] != "configure "+traefikStaticPath {
			t.Fatalf("expected only static config change, got %v", plan.Changes)
		}
		if _, err := step.Apply(context.Background(), env, plan); err != nil {
			t.Fatalf("Apply error: %v", err)
		}
		recreated := false
		for _, c := range r.Calls {
			if c.String() == "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate" {
				recreated = true
			}
			if c.String() == "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d" {
				t.Fatal("expected --force-recreate when traefik.yml changed, got plain up -d")
			}
		}
		if !recreated {
			t.Fatal("expected docker compose up -d --force-recreate to be called")
		}
	})

	t.Run("only _cvctl.yml changed does not use --force-recreate", func(t *testing.T) {
		r := &systest.Runner{}
		r.OnPrefix(legacyUsersPrefix, noSuchNet)
		fs := systest.NewFS()
		_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
		env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

		_ = fs.MkdirAll(traefikDir, 0o750, rootDeployOwner)
		_ = fs.MkdirAll("/etc/traefik", 0o755, rootOwner)
		_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)
		_ = fs.WriteFile(traefikStaticPath, staticBytes, 0o640, rootDeployOwner)
		_ = fs.WriteFile(traefikDynamicPath, []byte("# stale dynamic config\n"), 0o640, rootDeployOwner)
		_ = fs.WriteFile(traefikComposePath, composeBytes, 0o640, rootDeployOwner)

		r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
		r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(psJSON), ExitCode: 0})
		r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{ExitCode: 0})
		r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

		step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
		plan, err := step.Check(context.Background(), env)
		if err != nil {
			t.Fatalf("Check error: %v", err)
		}
		if len(plan.Changes) != 1 || plan.Changes[0] != "configure "+traefikDynamicPath {
			t.Fatalf("expected only dynamic config change, got %v", plan.Changes)
		}
		if _, err := step.Apply(context.Background(), env, plan); err != nil {
			t.Fatalf("Apply error: %v", err)
		}
		plainUp := false
		for _, c := range r.Calls {
			if c.String() == "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d" {
				plainUp = true
			}
			if c.String() == "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate" {
				t.Fatal("did not expect --force-recreate when only _cvctl.yml changed")
			}
		}
		if !plainUp {
			t.Fatal("expected plain docker compose up -d to be called")
		}
	})
}

func TestTraefikUnchanged(t *testing.T) {
	r := &systest.Runner{}
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
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

	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
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
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
	fs := systest.NewFS()
	_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)

	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On(proxyInspectCmd, sys.Result{ExitCode: 1, Stderr: []byte("Error: No such network: cvctl-edge-proxy\n")})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On(proxyCreateCmd, sys.Result{Stdout: []byte("net-id\n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n"), ExitCode: 0})

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
			if c.String() == proxyCreateCmd {
				n++
			}
		}
		return n
	}
	if got := countCreate(); got != 1 {
		t.Fatalf("expected 1 call to 'docker network create cvctl-edge-proxy' after first Apply, got %d", got)
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
	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
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
		t.Fatalf("expected 'docker network create cvctl-edge-proxy' not to be called again, got %d total calls", got)
	}
}

func TestTraefikUnhealthy(t *testing.T) {
	r := &systest.Runner{}
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("starting\n"), ExitCode: 0})
	r.On("docker logs --tail 20 cvctl-traefik", sys.Result{
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
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		EdgeMode:      "edge",
		AcmeEmail:     "acme@example.com",
		AcmeChallenge: "http",
	}
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n"), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json", sys.Result{Stdout: []byte(""), ExitCode: 0})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate", sys.Result{
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
	if !strings.Contains(oe.Hint, "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d --force-recreate") {
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

func TestTraefikBehindProxyItem7Requirements(t *testing.T) {
	behindCfg := &hostcfg.Config{
		EdgeMode:  "behind-proxy",
		ProxyPort: 8081,
	}

	// 1. Static config trusted IPs
	staticBytes, err := renderTraefikStatic(behindCfg)
	if err != nil {
		t.Fatalf("renderTraefikStatic: %v", err)
	}
	var staticObj traefikStaticConfig
	if err := yaml.Unmarshal(staticBytes, &staticObj); err != nil {
		t.Fatalf("unmarshal static: %v", err)
	}
	webEp, ok := staticObj.EntryPoints["web"]
	if !ok || webEp.ForwardedHeaders == nil {
		t.Fatalf("expected web entrypoint with forwardedHeaders, got %+v", webEp)
	}
	wantIPs := []string{"127.0.0.1/32", "10.203.0.1/32"}
	if !slices.Equal(webEp.ForwardedHeaders.TrustedIPs, wantIPs) {
		t.Fatalf("trustedIPs: got %v, want %v", webEp.ForwardedHeaders.TrustedIPs, wantIPs)
	}

	// 2. Dynamic config middlewares: cvctl-headers, cvctl-ratelimit, cvctl-inflight in cvctl-default
	// and depth: 1 in behind-proxy mode
	dynBytes, err := renderTraefikDynamic(behindCfg)
	if err != nil {
		t.Fatalf("renderTraefikDynamic: %v", err)
	}
	var dynObj traefikDynamicConfig
	if err := yaml.Unmarshal(dynBytes, &dynObj); err != nil {
		t.Fatalf("unmarshal dynamic: %v", err)
	}
	defChain, ok := dynObj.HTTP.Middlewares["cvctl-default"]
	if !ok || defChain.Chain == nil {
		t.Fatalf("expected cvctl-default chain, got %+v", dynObj.HTTP.Middlewares)
	}
	wantChain := []string{"cvctl-headers", "cvctl-ratelimit", "cvctl-inflight"}
	if !slices.Equal(defChain.Chain.Middlewares, wantChain) {
		t.Fatalf("chain middlewares: got %v, want %v", defChain.Chain.Middlewares, wantChain)
	}
	rl, ok := dynObj.HTTP.Middlewares["cvctl-ratelimit"]
	if !ok || rl.RateLimit == nil || rl.RateLimit.SourceCriterion == nil || rl.RateLimit.SourceCriterion.IPStrategy == nil {
		t.Fatalf("expected cvctl-ratelimit with sourceCriterion.ipStrategy, got %+v", rl)
	}
	if rl.RateLimit.SourceCriterion.IPStrategy.Depth != 1 {
		t.Fatalf("depth: got %d, want 1", rl.RateLimit.SourceCriterion.IPStrategy.Depth)
	}

	// 3. Compose container_name: cvctl-traefik
	composeBytes, err := renderTraefikCompose(behindCfg)
	if err != nil {
		t.Fatalf("renderTraefikCompose: %v", err)
	}
	var compObj traefikComposeConfig
	if err := yaml.Unmarshal(composeBytes, &compObj); err != nil {
		t.Fatalf("unmarshal compose: %v", err)
	}
	svc, ok := compObj.Services["traefik"]
	if !ok {
		t.Fatalf("missing service traefik: %+v", compObj.Services)
	}
	if svc.ContainerName != "cvctl-traefik" {
		t.Fatalf("container_name: got %q, want cvctl-traefik", svc.ContainerName)
	}

	// Edge mode: depth must NOT be present
	edgeCfg := &hostcfg.Config{EdgeMode: "edge"}
	edgeDynBytes, err := renderTraefikDynamic(edgeCfg)
	if err != nil {
		t.Fatalf("renderTraefikDynamic edge: %v", err)
	}
	var edgeDynObj traefikDynamicConfig
	if err := yaml.Unmarshal(edgeDynBytes, &edgeDynObj); err != nil {
		t.Fatalf("unmarshal edge dynamic: %v", err)
	}
	edgeRL := edgeDynObj.HTTP.Middlewares["cvctl-ratelimit"]
	if edgeRL.RateLimit != nil && edgeRL.RateLimit.SourceCriterion != nil {
		t.Fatalf("expected no sourceCriterion in edge mode, got %+v", edgeRL.RateLimit.SourceCriterion)
	}
}

func TestTraefikCheckWithDockerMissingReturnsChangesNoError(t *testing.T) {
	r := &systest.Runner{}
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
	fs := systest.NewFS()
	cfg := &hostcfg.Config{
		EdgeMode:  "behind-proxy",
		ProxyPort: 8081,
	}
	env := &Env{Run: r, FS: fs, Cfg: cfg, EUID: 0}
	r.OnErrorPrefix("docker", errors.New("exec: \"docker\": executable file not found in $PATH"))

	step := TraefikStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("expected no error from Check when docker is missing, got %v", err)
	}
	if len(plan.Changes) == 0 {
		t.Fatal("expected changes, got none")
	}
	if !slices.Contains(plan.Changes, "create network cvctl-edge-proxy") {
		t.Fatalf("expected 'create network cvctl-edge-proxy' in changes, got %v", plan.Changes)
	}
	if !slices.Contains(plan.Changes, "start traefik") {
		t.Fatalf("expected 'start traefik' in changes, got %v", plan.Changes)
	}
}

func writeTraefikFiles(t *testing.T, fs *systest.FS, cfg *hostcfg.Config) {
	t.Helper()
	st, _ := renderTraefikStatic(cfg)
	dy, _ := renderTraefikDynamic(cfg)
	co, _ := renderTraefikCompose(cfg)
	_ = fs.MkdirAll(traefikDir, 0o750, rootDeployOwner)
	_ = fs.MkdirAll("/etc/traefik", 0o755, rootOwner)
	_ = fs.MkdirAll("/etc/traefik/dynamic", 0o2775, deployerOwner)
	_ = fs.WriteFile(traefikStaticPath, st, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikDynamicPath, dy, 0o640, rootDeployOwner)
	_ = fs.WriteFile(traefikComposePath, co, 0o640, rootDeployOwner)
}

const (
	lsEdgeCmd         = "docker network ls --filter label=cvctl.managed=true --filter name=_edge --format {{.Name}}"
	traefikNetsCmd    = "docker inspect -f {{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}} cvctl-traefik"
	legacyEdgeCmd     = "docker network inspect -f {{range $k, $v := .Containers}}{{$v.Name}} {{end}} cvctl-edge"
	legacyUsersPrefix = "docker network inspect -f {{range $k, $v := .Containers}}"
	psCmd             = "docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml ps --format json"
)

func runningPS() sys.Result {
	return sys.Result{Stdout: []byte(`{"Service":"traefik","State":"running","Image":"` + traefikImage + `"}` + "\n")}
}

func newTraefikEnv(cfg *hostcfg.Config) (*systest.Runner, *systest.FS, *Env) {
	r := &systest.Runner{}
	r.OnPrefix(legacyUsersPrefix, noSuchNet)
	fs := systest.NewFS()
	_, sec, _ := hostcfg.Load(fs, "/etc/cvctl")
	return r, fs, &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}
}

func TestTraefikCustomSubnetGateway(t *testing.T) {
	c := hostcfg.NewConfig()
	_ = c.Set("EDGE_MODE", "behind-proxy")
	_ = c.Set("EDGE_SUBNET", "192.168.50.0/24")
	st, err := renderTraefikStatic(c)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(st), "192.168.50.1/32") || strings.Contains(string(st), "10.203.0.1") {
		t.Fatalf("static must trust the custom gateway only:\n%s", st)
	}
}

func TestTraefikWrongGatewayIsError(t *testing.T) {
	cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
	r, fs, env := newTraefikEnv(cfg)
	writeTraefikFiles(t, fs, cfg)
	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("172.18.0.1 \n")})
	_, err := TraefikStep{}.Check(context.Background(), env)
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != "E_TRAEFIK_EDGE_SUBNET" || !strings.Contains(oe.Hint, "docker network rm cvctl-edge-proxy") {
		t.Fatalf("want E_TRAEFIK_EDGE_SUBNET with hint, got %v", err)
	}
}

func TestTraefikCheckAdoptedHostWithLegacyNetwork(t *testing.T) {
	cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
	r, fs, env := newTraefikEnv(cfg)
	writeTraefikFiles(t, fs, cfg)
	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n")})
	r.On(legacyEdgeCmd, sys.Result{Stdout: []byte("cvctl-traefik ")})
	r.On(psCmd, runningPS())
	plan, err := TraefikStep{}.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{{Kind: "network-remove", Target: "cvctl-edge"}}
	if !slices.Equal(plan.Actions, want) {
		t.Fatalf("actions: got %v, want %v", plan.Actions, want)
	}
}

func TestTraefikFreshHostPlansNetworkAndStart(t *testing.T) {
	cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
	r, fs, env := newTraefikEnv(cfg)
	writeTraefikFiles(t, fs, cfg)
	r.On(proxyInspectCmd, noSuchNet)
	plan, err := TraefikStep{}.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{{Kind: "network-create", Target: "cvctl-edge-proxy"}, {Kind: "start", Target: "traefik"}}
	if !slices.Equal(plan.Actions, want) {
		t.Fatalf("actions: got %v, want %v", plan.Actions, want)
	}
}

func TestTraefikReconnectAndIdempotentApply(t *testing.T) {
	cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
	r, fs, env := newTraefikEnv(cfg)
	writeTraefikFiles(t, fs, cfg)
	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n")})
	r.On(psCmd, runningPS())
	r.On(lsEdgeCmd, sys.Result{Stdout: []byte("app_edge\nother_edge\nfoo_edge_extra\n")})
	r.On(traefikNetsCmd, sys.Result{Stdout: []byte("cvctl-edge-proxy other_edge \n")})
	r.OnPrefix("docker network connect", sys.Result{})
	r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n")})
	r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{})
	step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}

	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{{Kind: "reconnect", Target: "app_edge"}}
	if !slices.Equal(plan.Actions, want) {
		t.Fatalf("actions: got %v, want %v", plan.Actions, want)
	}
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatal(err)
	}
	connected := false
	for _, c := range r.Calls {
		if c.String() == "docker network connect app_edge cvctl-traefik" {
			connected = true
		}
	}
	if !connected {
		t.Fatalf("expected connect to app_edge, calls: %v", r.Calls)
	}

	r.On(traefikNetsCmd, sys.Result{Stdout: []byte("cvctl-edge-proxy other_edge app_edge \n")})
	plan, err = step.Check(context.Background(), env)
	if err != nil || len(plan.Actions) != 0 {
		t.Fatalf("second Check must plan nothing: %v %v", plan.Actions, err)
	}
	n := len(r.Calls)
	if _, err := step.Apply(context.Background(), env, plan); err != nil || len(r.Calls) != n {
		t.Fatalf("second Apply must run nothing: err=%v calls=%v", err, r.Calls[n:])
	}
}

func TestTraefikLegacyNetworkRetire(t *testing.T) {
	run := func(users string) (string, []string) {
		cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
		r, fs, env := newTraefikEnv(cfg)
		writeTraefikFiles(t, fs, cfg)
		r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n")})
		r.On(legacyEdgeCmd, sys.Result{Stdout: []byte("cvctl-traefik ")})
		r.On(psCmd, runningPS())
		r.On(traefikNetsCmd, sys.Result{Stdout: []byte("cvctl-edge-proxy cvctl-edge \n")})
		r.On("docker network disconnect cvctl-edge cvctl-traefik", sys.Result{})
		r.On(legacyEdgeCmd, sys.Result{Stdout: []byte(users)})
		r.On("docker network rm cvctl-edge", sys.Result{})
		r.On("docker compose -p cvctl-traefik -f /etc/cvctl/traefik/compose.yml up -d", sys.Result{})
		r.On("docker inspect -f {{.State.Health.Status}} cvctl-traefik", sys.Result{Stdout: []byte("healthy\n")})
		step := TraefikStep{PollInterval: time.Millisecond, PollTimeout: 10 * time.Millisecond}
		plan, err := step.Check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		warn, err := step.Apply(context.Background(), env, plan)
		if err != nil {
			t.Fatal(err)
		}
		var calls []string
		for _, c := range r.Calls {
			calls = append(calls, c.String())
		}
		return warn, calls
	}
	warn, calls := run("")
	if warn != "" || !slices.Contains(calls, "docker network disconnect cvctl-edge cvctl-traefik") || !slices.Contains(calls, "docker network rm cvctl-edge") {
		t.Fatalf("unused legacy network must be disconnected and removed: warn=%q calls=%v", warn, calls)
	}
}

func TestTraefikLegacyNetworkInUsePlansNothing(t *testing.T) {
	cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
	r, fs, env := newTraefikEnv(cfg)
	writeTraefikFiles(t, fs, cfg)
	r.On(proxyInspectCmd, sys.Result{Stdout: []byte("10.203.0.1 \n")})
	r.On(legacyEdgeCmd, sys.Result{Stdout: []byte("cvctl-traefik old-app-1 ")})
	r.On(psCmd, runningPS())
	for i := 0; i < 2; i++ {
		plan, err := TraefikStep{}.Check(context.Background(), env)
		if err != nil {
			t.Fatal(err)
		}
		if len(plan.Actions) != 0 || len(plan.Changes) != 0 || !strings.Contains(plan.Pending, "old-app-1") {
			t.Fatalf("run %d: want zero actions and a pending warning, got %+v", i, plan)
		}
	}
}

func TestTraefikDockerDownIsErrorNotNetworkCreate(t *testing.T) {
	cfg := &hostcfg.Config{EdgeMode: "behind-proxy", ProxyPort: 8081}
	r, fs, env := newTraefikEnv(cfg)
	writeTraefikFiles(t, fs, cfg)
	r.On(proxyInspectCmd, sys.Result{ExitCode: 1, Stderr: []byte("Cannot connect to the Docker daemon\n")})
	if _, err := (TraefikStep{}).Check(context.Background(), env); err == nil {
		t.Fatal("daemon failure must be an error")
	}
}

func TestTraefikComposePinsProxyGateway(t *testing.T) {
	for _, cfg := range []*hostcfg.Config{
		{EdgeMode: "behind-proxy", ProxyPort: 8081},
		{EdgeMode: "edge", AcmeChallenge: "http", AcmeEmail: "a@example.com"},
	} {
		b, err := renderTraefikCompose(cfg)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Services map[string]struct {
				Networks map[string]struct {
					GwPriority int `yaml:"gw_priority"`
				} `yaml:"networks"`
			} `yaml:"services"`
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatal(err)
		}
		nets := doc.Services["traefik"].Networks
		if len(nets) != 1 || nets["cvctl-edge-proxy"].GwPriority != 1000 {
			t.Fatalf("traefik must pin cvctl-edge-proxy with gw_priority 1000: %v", nets)
		}
	}
}
