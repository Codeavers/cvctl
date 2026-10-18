package deploy_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

func TestWire(t *testing.T) {
	fsys := systest.NewFS()
	run := &systest.Runner{}
	d := &deploy.Deployer{
		DeployRoot: "/srv/deploy",
		FS:         fsys,
	}

	deploy.Wire(d, run)

	if d.Compose == nil {
		t.Fatal("expected non-nil Compose")
	}
	if d.Health == nil {
		t.Fatal("expected non-nil Health")
	}
	if d.Repo == nil {
		t.Fatal("expected non-nil Repo")
	}

	repoAPI := d.Repo("myapp-dev")
	if repoAPI == nil {
		t.Fatal("expected non-nil CheckoutAPI from d.Repo")
	}
	if _, ok := repoAPI.(*gitrepo.Repo); !ok {
		t.Fatalf("expected *gitrepo.Repo, got %T", repoAPI)
	}

	inst := "myapp-dev"
	instDir := filepath.Join("/srv/deploy", inst)
	cvctlEnv := "CVCTL_INFRA_PATH=server\n"
	if err := fsys.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte(cvctlEnv), 0o640, sys.Owner{User: "deployer", Group: "deploy"}); err != nil {
		t.Fatalf("write cvctl.env: %v", err)
	}

	override := filepath.Join(instDir, "compose.cvctl.yml")
	envFile := filepath.Join(instDir, "app.env")
	comp := d.Compose(inst, override, envFile)
	if comp == nil {
		t.Fatal("expected non-nil ComposeAPI")
	}

	proj, ok := comp.(*compose.Project)
	if !ok {
		t.Fatalf("expected *compose.Project, got %T", comp)
	}

	cmd := proj.Cmd("config", "-q")
	wantArgs := []string{
		"compose",
		"-p", inst,
		"--env-file", envFile,
		"-f", override,
		"config", "-q",
	}
	if !slices.Equal(cmd.Args, wantArgs) {
		t.Fatalf("got args %v, want %v", cmd.Args, wantArgs)
	}
}

func TestWireHealth(t *testing.T) {
	tmp := t.TempDir()
	inst := "myapp-dev"
	instDir := filepath.Join(tmp, inst)
	serverDir := filepath.Join(instDir, "infra", "server")
	if err := os.MkdirAll(serverDir, 0o755); err != nil {
		t.Fatal(err)
	}

	composeYML := `services:
  web:
    image: myimage:v1
    labels:
      cvctl.role: app
      cvctl.expose: "8080"
x-cvctl:
  health:
    service: web
    http: /health
`
	if err := os.WriteFile(filepath.Join(serverDir, "compose.yml"), []byte(composeYML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "cvctl.env"), []byte("CVCTL_INFRA_PATH=server\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	run := &systest.Runner{}
	psOut := `[{"Service":"web","Name":"myapp-dev-web-1","State":"running","Health":"healthy"}]`
	run.On("docker compose -p myapp-dev --env-file "+filepath.Join(instDir, "app.env")+" -f "+filepath.Join(instDir, "compose.cvctl.yml")+" ps --format json", sys.Result{Stdout: []byte(psOut), ExitCode: 0})

	inspectOut := `{"myapp_dev_app":{"IPAddress":"127.0.0.1"}}`
	run.On("docker inspect -f {{json .NetworkSettings.Networks}} myapp-dev-web-1", sys.Result{Stdout: []byte(inspectOut), ExitCode: 0})

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}

	d := &deploy.Deployer{
		DeployRoot: tmp,
		Catalog:    cat,
	}
	deploy.Wire(d, run)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	// Calling d.Health exercises the contract loading, project binding, and compose.Health call.
	// Since HTTP ping fails against dummy 127.0.0.1 without mock server, it returns an error.
	if err := d.Health(ctx, inst, filepath.Join(instDir, "compose.cvctl.yml")); err == nil {
		t.Fatal("expected error connecting to dummy health endpoint")
	}
}

func TestSourceDeps(t *testing.T) {
	run := &systest.Runner{}
	hcfg := &hostcfg.Config{
		Registries:   "ghcr.io/codeavers,docker.io/myorg",
		RegistryUser: "myuser",
	}
	sec := hostcfg.NewSecrets()
	sec.Set("registry_token", "secret-token")

	deps := deploy.SourceDeps("demo-dev", run, hcfg, sec)
	if deps.Repo == nil {
		t.Fatal("expected non-nil Repo in Deps")
	}
	if deps.Resolver == nil {
		t.Fatal("expected non-nil Resolver in Deps")
	}
	wantAllowed := []string{"ghcr.io/codeavers", "docker.io/myorg"}
	if !slices.Equal(deps.Allowed, wantAllowed) {
		t.Fatalf("got Allowed %v, want %v", deps.Allowed, wantAllowed)
	}
}

func TestWireSetsAllowedFromHost(t *testing.T) {
	d := &deploy.Deployer{Host: &hostcfg.Config{Registries: "ghcr.io/a, docker.io/b"}}
	deploy.Wire(d, &systest.Runner{})
	if want := []string{"ghcr.io/a", "docker.io/b"}; !slices.Equal(d.Allowed, want) {
		t.Fatalf("got %v, want %v", d.Allowed, want)
	}

	d2 := &deploy.Deployer{}
	deploy.Wire(d2, &systest.Runner{})
	if d2.Allowed == nil || len(d2.Allowed) != 0 {
		t.Fatalf("no host registries must yield an empty non-nil list, got %#v", d2.Allowed)
	}
}

func TestBackupTargetFrom(t *testing.T) {
	sec := hostcfg.NewSecrets()
	sec.Set("s3_access_key", "ak")
	sec.Set("s3_secret_key", "sk")
	sec.Set("walg_libsodium_key", "lk")
	sec.Set("restic_password", "rp")
	cfg := hostcfg.NewConfig()
	_ = cfg.Set("HOST_NAME", "h1")
	_ = cfg.Set("S3_ENDPOINT", "http://ext:9000")
	_ = cfg.Set("S3_BUCKET", "b")

	got := deploy.BackupTargetFrom(cfg, sec)
	if got == nil || got.Endpoint != "http://ext:9000" || got.Region != "us-east-1" || got.Host != "h1" || got.ResticPassword != "rp" {
		t.Fatalf("fallback/default wrong: %+v", got)
	}
	_ = cfg.Set("S3_ENDPOINT_INTERNAL", "http://cvctl-storage:9000")
	_ = cfg.Set("S3_REGION", "eu-west-1")
	got = deploy.BackupTargetFrom(cfg, sec)
	if got.Endpoint != "http://cvctl-storage:9000" || got.Region != "eu-west-1" {
		t.Fatalf("internal endpoint/region not used: %+v", got)
	}
	if deploy.BackupTargetFrom(nil, sec) != nil || deploy.BackupTargetFrom(cfg, nil) != nil {
		t.Fatal("nil cfg/secrets must give nil")
	}
	sec.Set("walg_libsodium_key", "")
	if deploy.BackupTargetFrom(cfg, sec) != nil {
		t.Fatal("missing libsodium key must give nil")
	}
	sec.Set("walg_libsodium_key", "lk")
	_ = cfg.Set("S3_BUCKET", "")
	if deploy.BackupTargetFrom(cfg, sec) != nil {
		t.Fatal("missing bucket must give nil")
	}
}

func TestWireSetsRestorePointer(t *testing.T) {
	d := &deploy.Deployer{}
	deploy.Wire(d, &systest.Runner{})
	if _, ok := d.Restore.(*backup.RestorePoint); !ok {
		t.Fatalf("Restore = %T, want *backup.RestorePoint", d.Restore)
	}
}
