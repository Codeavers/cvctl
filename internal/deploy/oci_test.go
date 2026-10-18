package deploy

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	ggcrreg "github.com/google/go-containerregistry/pkg/registry"

	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
)

const testSpec = "vars: {}\n"

// appCompose is a valid contract with one exposed app "api".
func appCompose(image string) string {
	return `x-cvctl:
  health: { service: api, http: /health, timeout: 30s }
services:
  api:
    image: ` + image + `
    labels:
      cvctl.role: app
      cvctl.expose: "3000"
    deploy: { resources: { limits: { memory: 512m } } }
`
}

func ociRegistry(t *testing.T) (string, infraoci.Auth) {
	t.Helper()
	srv := httptest.NewServer(ggcrreg.New(ggcrreg.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	return host, infraoci.Auth{User: "u", Token: "tok", TokenHost: host, Insecure: []string{host}}
}

func publish(t *testing.T, a infraoci.Auth, ref string, files map[string][]byte) string {
	t.Helper()
	dg, err := infraoci.Publish(context.Background(), a, ref, files)
	if err != nil {
		t.Fatal(err)
	}
	return dg
}

func wantErrCode(t *testing.T, err error, code string) {
	t.Helper()
	var oe *output.Error
	if !errors.As(err, &oe) || oe.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

func ociDecisions(host, env string) *instance.Decisions {
	return &instance.Decisions{Instance: "proj-" + env, Env: env, InfraSource: instance.SourceOCI, InfraRef: host + "/org/proj-infra"}
}

func TestOCIDevSourceResolvesContractAndImages(t *testing.T) {
	host, a := ociRegistry(t)
	dg := publish(t, a, host+"/org/proj-infra:develop", map[string][]byte{
		"compose.yml":  []byte(appCompose("ghcr.io/codeavers/api")),
		"env.spec.yml": []byte(testSpec),
	})
	publish(t, a, host+"/org/proj-infra:main", map[string][]byte{
		"compose.yml":  []byte(appCompose("ghcr.io/codeavers/other")),
		"env.spec.yml": []byte(testSpec),
	})
	res := &fakeResolver{}
	src := SourceFor(ociDecisions(host, "dev"), Deps{OCI: a, Resolver: res, Allowed: []string{"ghcr.io/codeavers"}})
	if _, ok := src.(*OCIDevSource); !ok {
		t.Fatalf("SourceFor = %T, want *OCIDevSource", src)
	}
	got, err := src.Desired(context.Background(), ociDecisions(host, "dev"))
	if err != nil {
		t.Fatal(err)
	}
	if got.InfraDigest != dg || got.InfraCommit != "" {
		t.Fatalf("desired = %+v, want InfraDigest %s", got, dg)
	}
	if len(res.calls) != 1 || res.calls[0] != [2]string{"ghcr.io/codeavers/api", "develop"} {
		t.Fatalf("resolver calls = %v", res.calls)
	}
	if _, ok := got.Digests["api"]; !ok {
		t.Fatalf("digests = %v", got.Digests)
	}
}

func TestOCIDevSourceRejectsDisallowedRegistry(t *testing.T) {
	host, a := ociRegistry(t)
	publish(t, a, host+"/org/proj-infra:develop", map[string][]byte{
		"compose.yml":  []byte(appCompose("evil.io/x/api")),
		"env.spec.yml": []byte(testSpec),
	})
	res := &fakeResolver{}
	src := &OCIDevSource{Auth: a, Resolver: res, Allowed: []string{"ghcr.io/codeavers"}}
	_, err := src.Desired(context.Background(), ociDecisions(host, "dev"))
	wantErrCode(t, err, "E_REGISTRY_NOT_ALLOWED")
	if len(res.calls) != 0 {
		t.Fatalf("resolver called for a disallowed image: %v", res.calls)
	}
}

func TestOCIProdSourceReadsProdEnv(t *testing.T) {
	host, a := ociRegistry(t)
	pin := "sha256:" + strings.Repeat("3", 64)
	dg := publish(t, a, host+"/org/proj-infra:main", map[string][]byte{
		"compose.yml":  []byte(appCompose("ghcr.io/codeavers/api")),
		"env.spec.yml": []byte(testSpec),
		"prod.env":     []byte("CVCTL_DIGEST_API=" + pin + "\n"),
	})
	src := SourceFor(ociDecisions(host, "prod"), Deps{OCI: a, Allowed: []string{"ghcr.io/codeavers"}})
	if _, ok := src.(*OCIProdSource); !ok {
		t.Fatalf("SourceFor = %T, want *OCIProdSource", src)
	}
	got, err := src.Desired(context.Background(), ociDecisions(host, "prod"))
	if err != nil {
		t.Fatal(err)
	}
	if got.InfraDigest != dg || got.Digests["api"] != pin {
		t.Fatalf("desired = %+v", got)
	}
}

func TestOCIProdSourceRequiresProdEnv(t *testing.T) {
	host, a := ociRegistry(t)
	publish(t, a, host+"/org/proj-infra:main", map[string][]byte{
		"compose.yml":  []byte(appCompose("ghcr.io/codeavers/api")),
		"env.spec.yml": []byte(testSpec),
	})
	src := &OCIProdSource{Auth: a, Allowed: []string{"ghcr.io/codeavers"}}
	_, err := src.Desired(context.Background(), ociDecisions(host, "prod"))
	wantErrCode(t, err, "E_INFRA_ARTIFACT")
}

func TestOCIProdSourceMissingDigest(t *testing.T) {
	host, a := ociRegistry(t)
	publish(t, a, host+"/org/proj-infra:main", map[string][]byte{
		"compose.yml":  []byte(appCompose("ghcr.io/codeavers/api")),
		"env.spec.yml": []byte(testSpec),
		"prod.env":     []byte("OTHER=1\n"),
	})
	src := &OCIProdSource{Auth: a, Allowed: []string{"ghcr.io/codeavers"}}
	_, err := src.Desired(context.Background(), ociDecisions(host, "prod"))
	wantErrCode(t, err, "E_PROD_DIGEST_MISSING")
}

func TestOCISourceRejectsForbiddenContract(t *testing.T) {
	host, a := ociRegistry(t)
	publish(t, a, host+"/org/proj-infra:develop", map[string][]byte{
		"compose.yml":  []byte(appCompose("ghcr.io/codeavers/api") + "    privileged: true\n"),
		"env.spec.yml": []byte(testSpec),
	})
	src := &OCIDevSource{Auth: a, Resolver: &fakeResolver{}, Allowed: []string{"ghcr.io/codeavers"}}
	_, err := src.Desired(context.Background(), ociDecisions(host, "dev"))
	wantErrCode(t, err, "E_DEPLOY_INVALID")
	if !strings.Contains(err.Error(), "E_CONTRACT_FIELD_FORBIDDEN") {
		t.Fatalf("err = %v", err)
	}
}

func TestOCISourceWithoutRef(t *testing.T) {
	d := &instance.Decisions{Instance: "proj-dev", Env: "dev", InfraSource: instance.SourceOCI}
	_, err := (&OCIDevSource{}).Desired(context.Background(), d)
	wantErrCode(t, err, "E_DECISION_MISSING")
}

func TestSourceForGitDefault(t *testing.T) {
	d := &instance.Decisions{Instance: "proj-dev", Env: "dev"}
	if _, ok := SourceFor(d, Deps{}).(*DevSource); !ok {
		t.Fatal("empty InfraSource must select the git DevSource")
	}
}

type fakeContracts struct {
	mu      sync.Mutex
	fetched [][2]string
	by      map[string]map[string][]byte
}

func (f *fakeContracts) fetch(_ context.Context, ref, digest string) (*infraoci.Contract, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetched = append(f.fetched, [2]string{ref, digest})
	files, ok := f.by[digest]
	if !ok {
		return nil, &output.Error{Code: "E_INFRA_ARTIFACT", Message: "unknown digest " + digest}
	}
	return &infraoci.Contract{Digest: digest, Files: files}, nil
}

func ociRig(t *testing.T) (*testRig, *fakeContracts, string, string) {
	t.Helper()
	rig := setupTestRig(t, "dev", "demo-dev")
	env := sampleCvctlEnv + "CVCTL_INFRA_SOURCE=oci\nCVCTL_INFRA_REF=ghcr.io/org/demo-infra\n"
	if err := os.WriteFile(filepath.Join(rig.instDir, "cvctl.env"), []byte(env), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(rig.infraDir); err != nil {
		t.Fatal(err)
	}
	dgA := "sha256:" + strings.Repeat("a", 64)
	dgB := "sha256:" + strings.Repeat("b", 64)
	fc := &fakeContracts{by: map[string]map[string][]byte{
		dgA: {"compose.yml": []byte(sampleComposeYAML), "env.spec.yml": []byte(sampleEnvSpecYAML)},
		dgB: {"compose.yml": []byte(sampleComposeYAML + "# b\n"), "env.spec.yml": []byte(sampleEnvSpecYAML)},
	}}
	rig.deployer.Contracts = fc.fetch
	return rig, fc, dgA, dgB
}

func ociDesired(dg string) state.Desired {
	d := defaultDesired()
	d.InfraCommit = ""
	d.InfraDigest = dg
	return d
}

func TestDeployOCIFetchesContractByDigest(t *testing.T) {
	rig, fc, dgA, dgB := ociRig(t)
	res, err := rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgA), Options{Actor: "agent"})
	if err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy A: %+v %v", res, err)
	}
	if len(rig.repo.checkouts) != 0 {
		t.Fatalf("git checkout used for an OCI source: %v", rig.repo.checkouts)
	}
	if len(fc.fetched) != 1 || fc.fetched[0] != [2]string{"ghcr.io/org/demo-infra", dgA} {
		t.Fatalf("fetched = %v", fc.fetched)
	}
	got, err := os.ReadFile(filepath.Join(rig.infraDir, "compose.yml"))
	if err != nil || string(got) != sampleComposeYAML {
		t.Fatalf("infra/compose.yml = %q, %v", got, err)
	}

	if res, err := rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgB), Options{Actor: "agent"}); err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy B: %+v %v", res, err)
	}
	res, err = rig.deployer.Rollback(context.Background(), "demo-dev", nil, "cli")
	if err != nil || res.Status != state.StatusPaused {
		t.Fatalf("rollback: %+v %v", res, err)
	}
	if last := fc.fetched[len(fc.fetched)-1]; last[1] != dgA {
		t.Fatalf("rollback fetched %v, want %s", last, dgA)
	}
	got, _ = os.ReadFile(filepath.Join(rig.infraDir, "compose.yml"))
	if string(got) != sampleComposeYAML {
		t.Fatalf("rollback left compose.yml = %q", got)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil || st.Current == nil || st.Current.InfraDigest != dgA {
		t.Fatalf("state after rollback: %+v %v", st, err)
	}
}

func TestDeployOCIFailureRestoresCurrentDigest(t *testing.T) {
	rig, fc, dgA, _ := ociRig(t)
	if res, err := rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgA), Options{Actor: "agent"}); err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy A: %+v %v", res, err)
	}
	dgBad := "sha256:" + strings.Repeat("c", 64)
	fc.by[dgBad] = map[string][]byte{"compose.yml": []byte("services: {}\n"), "env.spec.yml": []byte(sampleEnvSpecYAML)}
	res, _ := rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgBad), Options{Actor: "agent"})
	if res.Status == state.StatusOK {
		t.Fatalf("invalid contract deployed: %+v", res)
	}
	if last := fc.fetched[len(fc.fetched)-1]; last[1] != dgA {
		t.Fatalf("restore fetched %v, want %s", fc.fetched, dgA)
	}
	got, _ := os.ReadFile(filepath.Join(rig.infraDir, "compose.yml"))
	if string(got) != sampleComposeYAML {
		t.Fatalf("infra not restored: %q", got)
	}
}

func TestDeployOCIWithoutFetcher(t *testing.T) {
	rig, _, dgA, _ := ociRig(t)
	rig.deployer.Contracts = nil
	_, err := rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgA), Options{Actor: "agent"})
	wantErrCode(t, err, "E_DEPLOY_MISCONFIGURED")
}

func TestDeployOCIHealthFailureRollsBackByDigest(t *testing.T) {
	rig, fc, dgA, dgB := ociRig(t)
	if res, err := rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgA), Options{Actor: "agent"}); err != nil || res.Status != state.StatusOK {
		t.Fatalf("deploy A: %+v %v", res, err)
	}
	rig.deployer.Health = func(context.Context, string, string) error {
		if string(mustRead(t, filepath.Join(rig.infraDir, "compose.yml"))) != sampleComposeYAML {
			return errors.New("unhealthy")
		}
		return nil
	}
	_, _ = rig.deployer.Deploy(context.Background(), "demo-dev", ociDesired(dgB), Options{Actor: "agent"})
	if last := fc.fetched[len(fc.fetched)-1]; last[1] != dgA {
		t.Fatalf("auto-rollback fetched %v, want %s last", fc.fetched, dgA)
	}
	st, err := rig.store.Load("demo-dev")
	if err != nil || st.Current == nil || st.Current.InfraDigest != dgA || st.RollbackFailed {
		t.Fatalf("state after auto-rollback: %+v %v", st, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
