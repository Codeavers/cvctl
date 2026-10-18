package deploy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codeavers/cvctl/internal/state"
)

const bucketsComposeYAML = `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }
  hooks:
    pre-deploy:  [{ service: api, run: "node dist/migrate.js" }]
  s3:
    media:
      buckets:
        products: { public: true }
        prescriptions: {}

services:
  api:
    image: ghcr.io/org/project-api
    environment:
      S3_ACCESS_KEY: ${S3_ACCESS_KEY}
      S3_SECRET_KEY: ${S3_SECRET_KEY}
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy: { resources: { limits: { memory: 512m } } }
  media:
    labels: { cvctl.kind: s3, cvctl.env: S3, cvctl.expose: "9000" }
`

const bucketsEnvSpecYAML = `vars:
  S3_ACCESS_KEY: { source: service, service: media }
  S3_SECRET_KEY: { source: service, service: media }
`

const (
	bucketsRootPassword = "rootsecretpassword0123456789abcd"
	bucketsAppPassword  = "appsecretpassword0123456789abcde"
)

var bucketsCvctlEnv = `CVCTL_INSTANCE=demo-dev
CVCTL_ENV=dev
CVCTL_STRATEGY=recreate
CVCTL_INFRA_BRANCH=develop
CVCTL_TRACK_TAG=develop
CVCTL_INFRA_PATH=.
CVCTL_DOMAIN_API=api.example.com
CVCTL_DOMAIN_MEDIA=media.example.com
CVCTL_DIGEST_API=sha256:0000000000000000000000000000000000000000000000000000000000000001
CVCTL_SVC_MEDIA_MODE=local
CVCTL_SVC_MEDIA_USER=media
CVCTL_SVC_MEDIA_DB=media
CVCTL_SVC_MEDIA_PASSWORD=` + bucketsRootPassword + `
CVCTL_SVC_MEDIA_APP_USER=media-app
CVCTL_SVC_MEDIA_APP_PASSWORD=` + bucketsAppPassword + `
`

func setupBucketsRig(t *testing.T) *testRig {
	t.Helper()
	rig := setupTestRig(t, "dev", "demo-dev")
	for path, content := range map[string]string{
		filepath.Join(rig.infraDir, "compose.yml"):  bucketsComposeYAML,
		filepath.Join(rig.infraDir, "env.spec.yml"): bucketsEnvSpecYAML,
		filepath.Join(rig.instDir, "cvctl.env"):     bucketsCvctlEnv,
	} {
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return rig
}

func TestDeployProvisionsBucketsBeforeHooks(t *testing.T) {
	rig := setupBucketsRig(t)
	rig.compose.execOutput = []byte("@@buckets\n{\"status\":\"success\",\"type\":\"folder\",\"key\":\"products/\"}\n{\"status\":\"success\",\"type\":\"folder\",\"key\":\"old/\"}\n")

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Status != state.StatusOK {
		t.Fatalf("status = %v, detail %q", res.Status, res.Detail)
	}

	order := strings.Join(rig.compose.calls, " ")
	iUp := strings.Index(order, "updata:media")
	iExec := strings.Index(order, "execscript:media")
	iHook := strings.Index(order, "runhook:api:")
	if iUp < 0 || iExec < 0 || iHook < 0 || !(iUp < iExec && iExec < iHook) {
		t.Fatalf("want updata -> execscript -> runhook, got %q", order)
	}

	var script string
	for _, inv := range rig.compose.invocations {
		if inv.op == "execscript" {
			script = inv.script
		}
	}
	if !strings.Contains(script, "local/products") || !strings.Contains(script, "local/prescriptions") {
		t.Fatalf("script misses declared buckets:\n%s", script)
	}
	if !strings.Contains(script, bucketsAppPassword) {
		t.Fatal("script must carry the app password on stdin")
	}
	if strings.Contains(script, bucketsRootPassword) {
		t.Fatal("script must never carry the root password")
	}

	data, err := os.ReadFile(filepath.Join(rig.deployer.stateRoot(), "demo-dev", "s3-orphans.json"))
	if err != nil {
		t.Fatalf("orphans file: %v", err)
	}
	if strings.TrimSpace(string(data)) != `{"media":["old"]}` {
		t.Fatalf("orphans = %s", data)
	}

	appEnv, err := os.ReadFile(filepath.Join(rig.instDir, "app.env"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(appEnv), bucketsRootPassword) {
		t.Fatal("root password leaked into app.env")
	}
	if !strings.Contains(string(appEnv), "S3_ACCESS_KEY=media-app") {
		t.Fatalf("app.env lacks the app key:\n%s", appEnv)
	}
}

func TestDeployRemovesOrphansFileWhenNoneLeft(t *testing.T) {
	rig := setupBucketsRig(t)
	path := filepath.Join(rig.deployer.stateRoot(), "demo-dev", "s3-orphans.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"media":["old"]}`), 0o640); err != nil {
		t.Fatal(err)
	}
	rig.compose.execOutput = []byte("@@buckets\n{\"status\":\"success\",\"type\":\"folder\",\"key\":\"products/\"}\n")

	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphans file should be gone, stat err = %v", err)
	}
}

func TestDeployProvisionFailureIsTransientAndRunsNoHooks(t *testing.T) {
	rig := setupBucketsRig(t)
	rig.compose.failExec = errors.New("exit 1")
	rig.compose.execStderr = []byte("mc: user add failed for secret " + bucketsAppPassword + " root " + bucketsRootPassword)
	want := defaultDesired()

	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", want, Options{Actor: "agent"})
	if err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	if res.Status != state.StatusFailed || !strings.Contains(res.Detail, "E_S3_PROVISION") {
		t.Fatalf("status %v detail %q", res.Status, res.Detail)
	}
	if !strings.Contains(res.Detail, "mc: user add failed") {
		t.Fatalf("detail lacks the container stderr: %q", res.Detail)
	}
	if strings.Contains(res.Detail, bucketsAppPassword) || strings.Contains(res.Detail, bucketsRootPassword) {
		t.Fatal("secret leaked into the detail")
	}
	for _, m := range rig.notifier.messages {
		if strings.Contains(m.Text, bucketsAppPassword) || strings.Contains(m.Text, bucketsRootPassword) {
			t.Fatal("secret leaked into a notification")
		}
	}
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "runhook:") || c == "up" {
			t.Fatalf("%s ran after a provisioning failure", c)
		}
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil {
		t.Fatal(err)
	}
	if st.FailedHash != "" {
		t.Fatalf("provisioning failure must be transient, FailedHash = %q", st.FailedHash)
	}
	if st.Current != nil {
		t.Fatalf("Current must stay untouched, got %+v", st.Current)
	}
}

func TestDeploySkipsProvisioningWithoutBucketServices(t *testing.T) {
	rig := setupTestRig(t, "dev", "demo-dev")
	if _, err := rig.deployer.Deploy(context.Background(), "demo-dev", defaultDesired(), Options{Actor: "agent"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range rig.compose.calls {
		if strings.HasPrefix(c, "execscript:") {
			t.Fatalf("unexpected %s", c)
		}
	}
}
