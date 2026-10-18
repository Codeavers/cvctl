package project

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ggcrreg "github.com/google/go-containerregistry/pkg/registry"

	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/infraoci"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
)

// gitForbidden fails the test on any git operation.
type gitForbidden struct{ t *testing.T }

func (g gitForbidden) Clone(context.Context, string, string) error {
	g.t.Fatal("git clone on an OCI source")
	return nil
}
func (g gitForbidden) LsRemote(context.Context, string) error {
	g.t.Fatal("git ls-remote on an OCI source")
	return nil
}
func (g gitForbidden) Fetch(context.Context, string) (string, error) {
	g.t.Fatal("git fetch on an OCI source")
	return "", nil
}
func (g gitForbidden) Show(context.Context, string, string) ([]byte, error) {
	g.t.Fatal("git show on an OCI source")
	return nil, nil
}
func (g gitForbidden) Checkout(context.Context, string) error {
	g.t.Fatal("git checkout on an OCI source")
	return nil
}

type ociRig struct {
	env     *Env
	run     *systest.Runner
	store   *state.Store
	tmp     string
	host    string
	auth    infraoci.Auth
	ref     string
	instDir string
}

func newOCIRig(t *testing.T) *ociRig {
	t.Helper()
	srv := httptest.NewServer(ggcrreg.New(ggcrreg.Logger(log.New(io.Discard, "", 0))))
	t.Cleanup(srv.Close)
	host := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("CVCTL_INSECURE_REGISTRIES", host)

	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, "traefik"), 0o755); err != nil {
		t.Fatal(err)
	}
	fl := systest.NewFakeLocker()
	oldLock := sys.LockFile
	t.Cleanup(func() { sys.LockFile = oldLock })
	sys.LockFile = fl.LockFile

	osfs := sys.NewOSFS()
	stateRoot := filepath.Join(tmp, "state")
	store := state.NewStore(osfs, stateRoot)
	cat := testCatalog(t)
	run := &systest.Runner{}
	run.OnPrefix("docker network", sys.Result{})
	deployer := &deploy.Deployer{
		Run:        run,
		DeployRoot: tmp,
		TraefikDir: filepath.Join(tmp, "traefik"),
		StateRoot:  stateRoot,
		FS:         osfs,
		Store:      store,
		Catalog:    cat,
		Compose:    func(_, _, _ string) deploy.ComposeAPI { return noopCompose{} },
		Health:     func(context.Context, string, string) error { return nil },
	}
	e := &Env{
		Run:         run,
		FS:          osfs,
		DeployRoot:  tmp,
		StateRoot:   stateRoot,
		Catalog:     cat,
		Host:        &hostcfg.Config{Registries: "ghcr.io/codeavers"},
		Deployer:    deployer,
		Resolver:    fixedResolver{},
		RepoFactory: func(string, string) RepoLike { return gitForbidden{t} },
	}
	return &ociRig{
		env: e, run: run, store: store, tmp: tmp, host: host,
		auth:    infraoci.Auth{Insecure: []string{host}},
		ref:     host + "/org/demo-infra",
		instDir: filepath.Join(tmp, "demo-dev"),
	}
}

func (r *ociRig) publish(t *testing.T, tag, compose string) string {
	t.Helper()
	dg, err := infraoci.Publish(context.Background(), r.auth, r.ref+":"+tag, map[string][]byte{
		"compose.yml":  []byte(compose),
		"env.spec.yml": []byte(validSpec),
	})
	if err != nil {
		t.Fatal(err)
	}
	return dg
}

func (r *ociRig) noGit(t *testing.T) {
	t.Helper()
	for _, c := range r.run.Calls {
		if s := fmt.Sprint(c); strings.Contains(s, "git") {
			t.Fatalf("git command run for an OCI source: %s", s)
		}
	}
}

func (r *ociRig) current(t *testing.T) string {
	t.Helper()
	st, err := r.store.Load("demo-dev")
	if err != nil || st.Current == nil {
		t.Fatalf("state: %+v %v", st, err)
	}
	return st.Current.InfraDigest
}

func (r *ociRig) add(t *testing.T) *AddResult {
	t.Helper()
	res, err := Add(context.Background(), r.env, "oci://"+r.ref, AddOptions{
		Project: "demo", Env: "dev", Domains: map[string]string{"web": "web.example.com"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	if res.Deploy.Status != state.StatusOK {
		t.Fatalf("deploy: %+v", res.Deploy)
	}
	return res
}

func TestAddOCINoDeployKeyNoGit(t *testing.T) {
	rig := newOCIRig(t)
	dg := rig.publish(t, "develop", validCompose)
	res := rig.add(t)
	if res.DeployKeyPub != "" {
		t.Fatalf("deploy key for an OCI source: %q", res.DeployKeyPub)
	}
	if _, err := os.Stat(filepath.Join(rig.tmp, "state", "demo-dev", "deploy_key")); err == nil {
		t.Fatal("deploy_key written for an OCI source")
	}
	rig.noGit(t)
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	for _, w := range []string{"CVCTL_INFRA_SOURCE=oci\n", "CVCTL_INFRA_REF=" + rig.ref + "\n"} {
		if !strings.Contains(string(raw), w) {
			t.Fatalf("cvctl.env missing %q:\n%s", w, raw)
		}
	}
	for _, no := range []string{"CVCTL_INFRA_URL", "CVCTL_INFRA_BRANCH"} {
		if strings.Contains(string(raw), no) {
			t.Fatalf("cvctl.env has %s for an OCI source:\n%s", no, raw)
		}
	}
	if got, _ := os.ReadFile(filepath.Join(rig.instDir, "infra", "compose.yml")); string(got) != validCompose {
		t.Fatalf("infra/compose.yml = %q", got)
	}
	if got := rig.current(t); got != dg {
		t.Fatalf("current InfraDigest = %s, want %s", got, dg)
	}
}

func TestUpdateOCIRefetchesTag(t *testing.T) {
	rig := newOCIRig(t)
	rig.publish(t, "develop", validCompose)
	rig.add(t)
	v2 := validCompose + "# v2\n"
	dg2 := rig.publish(t, "develop", v2)
	res, err := Update(context.Background(), rig.env, "demo-dev", UpdateOptions{})
	if err != nil || res.Deploy.Status != state.StatusOK {
		t.Fatalf("update: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(rig.instDir, "infra", "compose.yml")); string(got) != v2 {
		t.Fatalf("infra/compose.yml = %q", got)
	}
	if got := rig.current(t); got != dg2 {
		t.Fatalf("current InfraDigest = %s, want %s", got, dg2)
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	if strings.Contains(string(raw), "CVCTL_INFRA_URL") {
		t.Fatalf("update recorded a git URL for an OCI source:\n%s", raw)
	}
	rig.noGit(t)
}

func TestAddOCIRejectsBadRefsAndInfraPath(t *testing.T) {
	for _, u := range []string{"oci://", "oci://ghcr.io", "oci://ghcr.io/org/x:develop", "oci://ghcr.io/org/x@sha256:" + strings.Repeat("a", 64), "oci://-x/y"} {
		if err := validateRepoURL(u); err == nil {
			t.Errorf("validateRepoURL(%q) accepted", u)
		}
	}
	if err := validateRepoURL("oci://ghcr.io/org/demo-infra"); err != nil {
		t.Fatal(err)
	}
	if err := validateRepoURL("oci://localhost:5000/org/demo-infra"); err != nil {
		t.Fatal(err)
	}
	_, err := Add(context.Background(), &Env{}, "oci://ghcr.io/org/demo-infra", AddOptions{InfraPath: "sub"})
	if err == nil || !strings.Contains(err.Error(), "git sources only") {
		t.Fatalf("--infra-path with oci: %v", err)
	}
}

func TestRestoreOCIFromRecordedRef(t *testing.T) {
	rig := newRestoreRig(t, "")
	o := newOCIRig(t)
	dg := o.publish(t, "develop", validCompose)
	body := strings.Replace(restoreEnvBase, "CVCTL_INFRA_URL=git@github.com:org/demo-infra.git\n",
		"CVCTL_INFRA_SOURCE=oci\nCVCTL_INFRA_REF="+o.ref+"\n", 1)
	if err := os.WriteFile(filepath.Join(rig.instDir, "cvctl.env"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	rig.env.RepoFactory = func(string, string) RepoLike { return gitForbidden{t} }
	res, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{})
	if err != nil || res.Deploy.Status != state.StatusOK {
		t.Fatalf("restore: %+v %v", res, err)
	}
	if got, _ := os.ReadFile(filepath.Join(rig.instDir, "infra", "compose.yml")); string(got) != validCompose {
		t.Fatalf("infra/compose.yml = %q", got)
	}
	st, _ := rig.store.Load("demo-dev")
	if st == nil || st.Current == nil || st.Current.InfraDigest != dg {
		t.Fatalf("state: %+v", st)
	}
}

func TestRestoreOCIFromRepoFlagRecordsSource(t *testing.T) {
	rig := newRestoreRig(t, restoreEnvBase)
	o := newOCIRig(t)
	dg := o.publish(t, "develop", validCompose)
	rig.env.RepoFactory = func(string, string) RepoLike { return gitForbidden{t} }
	res, err := Restore(context.Background(), rig.env, "demo-dev", RestoreOptions{Repo: "oci://" + o.ref})
	if err != nil || res.Deploy.Status != state.StatusOK {
		t.Fatalf("restore: %+v %v", res, err)
	}
	raw, _ := os.ReadFile(filepath.Join(rig.instDir, "cvctl.env"))
	for _, w := range []string{"CVCTL_INFRA_SOURCE=oci\n", "CVCTL_INFRA_REF=" + o.ref + "\n"} {
		if !strings.Contains(string(raw), w) {
			t.Fatalf("cvctl.env missing %q:\n%s", w, raw)
		}
	}
	st, _ := rig.store.Load("demo-dev")
	if st == nil || st.Current == nil || st.Current.InfraDigest != dg {
		t.Fatalf("state: %+v", st)
	}
}

// subdirRepo clones the contract into <dir>/<sub>, like a monorepo.
type subdirRepo struct {
	stubRepo
	t   *testing.T
	sub string
}

func (s *subdirRepo) Clone(_ context.Context, _, _ string) error {
	writeTestInfra(s.t, filepath.Join(s.dir, s.sub), validCompose, validSpec)
	return nil
}

func TestAddInfraPath(t *testing.T) {
	tmp := t.TempDir()
	e := &Env{
		FS:         sys.NewOSFS(),
		DeployRoot: tmp,
		StateRoot:  filepath.Join(tmp, "state"),
		Catalog:    testCatalog(t),
	}
	e.RepoFactory = func(dir, _ string) RepoLike {
		return &subdirRepo{stubRepo: stubRepo{dir: dir}, t: t, sub: "deploy/infra"}
	}
	_, err := Add(context.Background(), e, "git@github.com:org/mono.git", AddOptions{
		Project: "demo", Env: "dev", InfraPath: "deploy/infra", Domains: map[string]string{"web": "web.example.com"},
	})
	if err != nil {
		t.Fatalf("add: %v", err)
	}
	raw, _ := os.ReadFile(filepath.Join(tmp, "demo-dev", "cvctl.env"))
	if !strings.Contains(string(raw), "CVCTL_INFRA_PATH=deploy/infra\n") {
		t.Fatalf("cvctl.env:\n%s", raw)
	}
	for _, bad := range []string{"/abs", "../up", "a/../b", "a//b", "a b", "-x"} {
		_, err := Add(context.Background(), e, "git@github.com:org/mono.git", AddOptions{Project: "x" + fmt.Sprint(len(bad)), InfraPath: bad})
		if err == nil || !strings.Contains(err.Error(), "infra-path") {
			t.Errorf("--infra-path %q: %v", bad, err)
		}
	}
}
