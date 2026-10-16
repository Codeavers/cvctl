package host

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/compose-spec/compose-go/v2/loader"
	"github.com/compose-spec/compose-go/v2/types"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/objstore"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/internal/sys/systest"
	"github.com/codeavers/cvctl/kinds"
)

type fakeObjStore struct {
	objects   map[string][]byte
	bucketOK  bool
	bucketErr error
	putErr    error
	getErr    error
	delErr    error
}

func newFakeObjStore() *fakeObjStore {
	return &fakeObjStore{objects: make(map[string][]byte)}
}

func (f *fakeObjStore) EnsureBucket(_ context.Context) error {
	if f.bucketErr != nil {
		return f.bucketErr
	}
	f.bucketOK = true
	return nil
}

func (f *fakeObjStore) Put(_ context.Context, key string, b []byte) error {
	if f.putErr != nil {
		return f.putErr
	}
	f.objects[key] = append([]byte(nil), b...)
	return nil
}

func (f *fakeObjStore) Get(_ context.Context, key string) ([]byte, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	b, ok := f.objects[key]
	if !ok {
		return nil, &output.Error{Code: "E_STORAGE_CHECK", Message: "storage get: NoSuchKey", Hint: "check S3"}
	}
	return append([]byte(nil), b...), nil
}

func (f *fakeObjStore) Delete(_ context.Context, key string) error {
	if f.delErr != nil {
		return f.delErr
	}
	delete(f.objects, key)
	return nil
}

var alnum20Re = regexp.MustCompile(`^[A-Za-z0-9]{20}$`)
var alnum40Re = regexp.MustCompile(`^[A-Za-z0-9]{40}$`)

func TestStorageLocalGeneratesKeysOnce(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("STORAGE", "local"); err != nil {
		t.Fatal(err)
	}
	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	r.OnPrefix("docker compose -p cvctl-storage -f /etc/cvctl/storage/compose.yml up -d", sys.Result{ExitCode: 0})

	store := newFakeObjStore()
	// First Check fails Probe (before Apply brings up local storage), then succeeds after EnsureBucket.
	store.putErr = &output.Error{Code: "E_STORAGE_CHECK", Message: "cannot reach storage endpoint http://127.0.0.1:9100", Hint: "check storage"}
	oldNewStore := newStore
	t.Cleanup(func() { newStore = oldNewStore })
	newStore = func(c objstore.Config) (objstore.Store, error) {
		return &hookedStore{
			fakeObjStore: store,
			onEnsure: func() {
				store.putErr = nil
			},
		}, nil
	}

	step := StorageStep{PollInterval: time.Millisecond, PollTimeout: 20 * time.Millisecond}
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

	ak1, okAK := sec.Get("s3_access_key")
	sk1, okSK := sec.Get("s3_secret_key")
	if !okAK || !alnum20Re.MatchString(ak1) {
		t.Fatalf("s3_access_key = %q (ok=%v), want 20 alphanumeric chars", ak1, okAK)
	}
	if !okSK || !alnum40Re.MatchString(sk1) {
		t.Fatalf("s3_secret_key = %q (ok=%v), want 40 alphanumeric chars", sk1, okSK)
	}
	if cfg.S3Endpoint != "http://127.0.0.1:9100" || cfg.S3Bucket != "cvctl" || cfg.S3Region != "us-east-1" {
		t.Fatalf("unexpected local cfg: endpoint=%q bucket=%q region=%q", cfg.S3Endpoint, cfg.S3Bucket, cfg.S3Region)
	}

	// Verify keys and config were saved to FS via hostcfg.Save.
	loadedCfg, loadedSec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatalf("hostcfg.Load after Apply: %v", err)
	}
	if v, _ := loadedSec.Get("s3_access_key"); v != ak1 {
		t.Fatalf("persisted s3_access_key = %q, want %q", v, ak1)
	}
	if v, _ := loadedSec.Get("s3_secret_key"); v != sk1 {
		t.Fatalf("persisted s3_secret_key = %q, want %q", v, sk1)
	}
	if loadedCfg.S3Endpoint != "http://127.0.0.1:9100" || loadedCfg.S3Bucket != "cvctl" {
		t.Fatalf("persisted cfg endpoint=%q bucket=%q", loadedCfg.S3Endpoint, loadedCfg.S3Bucket)
	}

	// Second run: even if Probe fails and triggers Apply again, keys must NOT be regenerated.
	store.putErr = &output.Error{Code: "E_STORAGE_CHECK", Message: "cannot reach storage endpoint http://127.0.0.1:9100", Hint: "check storage"}
	plan2, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("second Check error: %v", err)
	}
	if _, err := step.Apply(context.Background(), env, plan2); err != nil {
		t.Fatalf("second Apply error: %v", err)
	}

	ak2, _ := sec.Get("s3_access_key")
	sk2, _ := sec.Get("s3_secret_key")
	if ak2 != ak1 || sk2 != sk1 {
		t.Fatalf("keys changed on second run: (%q, %q) -> (%q, %q)", ak1, sk1, ak2, sk2)
	}
}

type hookedStore struct {
	*fakeObjStore
	onEnsure func()
}

func (h *hookedStore) EnsureBucket(ctx context.Context) error {
	if h.onEnsure != nil {
		h.onEnsure()
	}
	return h.fakeObjStore.EnsureBucket(ctx)
}

func TestStorageLocalComposeNoInlineSecrets(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set("STORAGE", "local"); err != nil {
		t.Fatal(err)
	}
	const wantAK = "LOCALACCESSKEY123456"
	const wantSK = "LOCALSECRETKEY12345678901234567890123456"
	sec.Set("s3_access_key", wantAK)
	sec.Set("s3_secret_key", wantSK)

	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}
	r.On("docker compose -p cvctl-storage -f /etc/cvctl/storage/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})

	store := newFakeObjStore()
	oldNewStore := newStore
	t.Cleanup(func() { newStore = oldNewStore })
	newStore = func(c objstore.Config) (objstore.Store, error) {
		return store, nil
	}

	step := StorageStep{PollInterval: time.Millisecond, PollTimeout: 20 * time.Millisecond}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if _, err := step.Apply(context.Background(), env, plan); err != nil {
		t.Fatalf("Apply error: %v", err)
	}

	composeBytes, err := fs.ReadFile("/etc/cvctl/storage/compose.yml")
	if err != nil {
		t.Fatalf("read compose.yml: %v", err)
	}
	composeStr := string(composeBytes)
	if strings.Contains(composeStr, wantAK) || strings.Contains(composeStr, wantSK) {
		t.Fatalf("compose.yml must not contain inline credentials:\n%s", composeStr)
	}
	if strings.Contains(composeStr, "MINIO_ROOT_USER") || strings.Contains(composeStr, "MINIO_ROOT_PASSWORD") {
		t.Fatalf("compose.yml must not inline MINIO_ROOT_* vars:\n%s", composeStr)
	}

	cat, err := kinds.Load()
	if err != nil {
		t.Fatal(err)
	}
	s3Kind := cat["s3"]
	wantImage, err := s3Kind.ImageFor(s3Kind.DefaultVersion)
	if err != nil {
		t.Fatal(err)
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
		o.SetProjectName("cvctl-storage", true)
	})
	if err != nil {
		t.Fatalf("compose-go load failed: %v", err)
	}
	if len(cp.Services) != 1 {
		t.Fatalf("expected 1 service in compose.yml, got %d", len(cp.Services))
	}
	for _, svc := range cp.Services {
		if svc.Image != wantImage {
			t.Fatalf("service image = %q, want %q", svc.Image, wantImage)
		}
		if len(svc.Environment) != 0 {
			t.Fatalf("expected empty inline environment on service, got %v", svc.Environment)
		}
		if len(svc.EnvFiles) != 1 || svc.EnvFiles[0].Path != "/etc/cvctl/secrets/storage.env" {
			t.Fatalf("expected env_file /etc/cvctl/secrets/storage.env, got %+v", svc.EnvFiles)
		}
		if svc.Restart != "unless-stopped" {
			t.Fatalf("expected restart unless-stopped, got %q", svc.Restart)
		}
	}

	dirInfo, err := fs.Stat("/etc/cvctl/storage")
	if err != nil || !dirInfo.IsDir || dirInfo.Mode != 0o750 || dirInfo.User != "root" || dirInfo.Group != "deploy" {
		t.Fatalf("/etc/cvctl/storage stat = %+v err=%v, want dir 0750 root:deploy", dirInfo, err)
	}
	compInfo, err := fs.Stat("/etc/cvctl/storage/compose.yml")
	if err != nil || compInfo.Mode != 0o640 || compInfo.User != "root" || compInfo.Group != "deploy" {
		t.Fatalf("compose.yml stat = %+v err=%v, want 0640 root:deploy", compInfo, err)
	}

	envBytes, err := fs.ReadFile("/etc/cvctl/secrets/storage.env")
	if err != nil {
		t.Fatalf("read storage.env: %v", err)
	}
	wantEnv := "MINIO_ROOT_PASSWORD=" + wantSK + "\nMINIO_ROOT_USER=" + wantAK + "\n"
	if string(envBytes) != wantEnv {
		t.Fatalf("storage.env = %q, want %q", string(envBytes), wantEnv)
	}
	envInfo, err := fs.Stat("/etc/cvctl/secrets/storage.env")
	if err != nil || envInfo.Mode != 0o640 || envInfo.User != "root" || envInfo.Group != "deploy" {
		t.Fatalf("storage.env stat = %+v err=%v, want 0640 root:deploy", envInfo, err)
	}
}

func TestStorageS3BucketDenied(t *testing.T) {
	r := &systest.Runner{}
	fs := systest.NewFS()
	cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
	if err != nil {
		t.Fatal(err)
	}
	_ = cfg.Set("STORAGE", "s3")
	_ = cfg.Set("S3_ENDPOINT", "https://s3.example.com")
	_ = cfg.Set("S3_BUCKET", "my-backups")
	_ = cfg.Set("S3_REGION", "auto")
	sec.Set("s3_access_key", "AKIA1234567890123456")
	sec.Set("s3_secret_key", "secret1234567890123456789012345678901234")

	env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

	store := newFakeObjStore()
	store.putErr = &output.Error{Code: "E_STORAGE_CHECK", Message: "storage put: NoSuchBucket", Hint: "check S3"}
	store.bucketErr = &output.Error{Code: "E_STORAGE_CHECK", Message: "storage ensure_bucket: AccessDenied", Hint: "check S3"}

	oldNewStore := newStore
	t.Cleanup(func() { newStore = oldNewStore })
	newStore = func(c objstore.Config) (objstore.Store, error) {
		return store, nil
	}

	step := StorageStep{}
	plan, err := step.Check(context.Background(), env)
	if err != nil {
		t.Fatalf("Check error: %v", err)
	}
	if len(plan.Changes) != 1 || plan.Changes[0] != "verify storage" {
		t.Fatalf("expected [verify storage], got %v", plan.Changes)
	}

	_, err = step.Apply(context.Background(), env, plan)
	if err == nil {
		t.Fatal("expected E_STORAGE_BUCKET error, got nil")
	}
	var oe *output.Error
	if !errors.As(err, &oe) {
		t.Fatalf("expected *output.Error, got %T (%v)", err, err)
	}
	if oe.Code != "E_STORAGE_BUCKET" {
		t.Fatalf("got code %q, want E_STORAGE_BUCKET", oe.Code)
	}
	wantHint := "create bucket my-backups in your provider"
	if oe.Hint != wantHint {
		t.Fatalf("got hint %q, want %q", oe.Hint, wantHint)
	}
}

func TestStorageUnchangedWhenProbeOK(t *testing.T) {
	t.Run("s3", func(t *testing.T) {
		r := &systest.Runner{}
		fs := systest.NewFS()
		cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
		if err != nil {
			t.Fatal(err)
		}
		_ = cfg.Set("STORAGE", "s3")
		_ = cfg.Set("S3_ENDPOINT", "https://s3.example.com")
		_ = cfg.Set("S3_BUCKET", "cvctl")
		_ = cfg.Set("S3_REGION", "auto")
		sec.Set("s3_access_key", "AKIA1234567890123456")
		sec.Set("s3_secret_key", "secret1234567890123456789012345678901234")

		env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

		store := newFakeObjStore()
		oldNewStore := newStore
		t.Cleanup(func() { newStore = oldNewStore })
		newStore = func(c objstore.Config) (objstore.Store, error) {
			return store, nil
		}

		step := StorageStep{}
		plan, err := step.Check(context.Background(), env)
		if err != nil {
			t.Fatalf("Check error: %v", err)
		}
		if len(plan.Changes) != 0 {
			t.Fatalf("expected no changes when Probe is OK, got %v", plan.Changes)
		}
		if len(r.Calls) != 0 {
			t.Fatalf("expected no runner calls, got %v", r.Calls)
		}
	})

	t.Run("local", func(t *testing.T) {
		r := &systest.Runner{}
		fs := systest.NewFS()
		cfg, sec, err := hostcfg.Load(fs, "/etc/cvctl")
		if err != nil {
			t.Fatal(err)
		}
		_ = cfg.Set("STORAGE", "local")
		env := &Env{Run: r, FS: fs, Cfg: cfg, Secrets: sec, EUID: 0}

		r.On("docker compose -p cvctl-storage -f /etc/cvctl/storage/compose.yml up -d --force-recreate", sys.Result{ExitCode: 0})

		store := newFakeObjStore()
		store.putErr = &output.Error{Code: "E_STORAGE_CHECK", Message: "cannot reach storage endpoint http://127.0.0.1:9100", Hint: "check storage"}
		oldNewStore := newStore
		t.Cleanup(func() { newStore = oldNewStore })
		newStore = func(c objstore.Config) (objstore.Store, error) {
			return &hookedStore{
				fakeObjStore: store,
				onEnsure: func() {
					store.putErr = nil
				},
			}, nil
		}

		step := StorageStep{PollInterval: time.Millisecond, PollTimeout: 20 * time.Millisecond}
		plan1, err := step.Check(context.Background(), env)
		if err != nil {
			t.Fatalf("first Check error: %v", err)
		}
		if _, err := step.Apply(context.Background(), env, plan1); err != nil {
			t.Fatalf("Apply error: %v", err)
		}

		plan2, err := step.Check(context.Background(), env)
		if err != nil {
			t.Fatalf("second Check error: %v", err)
		}
		if len(plan2.Changes) != 0 {
			t.Fatalf("expected no changes on second Check when local Probe is OK, got %v", plan2.Changes)
		}
	})
}

func TestStorageRegisteredInDefaultSteps(t *testing.T) {
	steps := DefaultSteps()
	if len(steps) < 10 {
		t.Fatalf("expected at least 10 steps in DefaultSteps, got %d", len(steps))
	}
	if steps[8].Name() != "traefik" {
		t.Fatalf("step 8: got %q, want traefik", steps[8].Name())
	}
	if steps[9].Name() != "storage" {
		t.Fatalf("step 9: got %q, want storage", steps[9].Name())
	}
}

func TestStorageWaitForBucketStopsOnAuthError(t *testing.T) {
	for _, code := range []string{"AccessDenied", "InvalidAccessKeyId", "SignatureDoesNotMatch"} {
		t.Run(code, func(t *testing.T) {
			calls := 0
			store := newFakeObjStore()
			store.bucketErr = &output.Error{Code: "E_STORAGE_CHECK", Message: "storage ensure_bucket: " + code, Hint: "check S3"}
			hooked := &hookedStore{
				fakeObjStore: store,
				onEnsure: func() {
					calls++
				},
			}
			step := StorageStep{PollInterval: 50 * time.Millisecond, PollTimeout: 2 * time.Second}
			start := time.Now()
			err := step.waitForBucket(context.Background(), hooked)
			elapsed := time.Since(start)
			if err == nil {
				t.Fatalf("expected error for %s, got nil", code)
			}
			if !strings.Contains(err.Error(), code) {
				t.Fatalf("expected error to contain %s, got %v", code, err)
			}
			if calls != 1 {
				t.Fatalf("expected exactly 1 call, got %d", calls)
			}
			if elapsed > 500*time.Millisecond {
				t.Fatalf("waitForBucket took %v, expected immediate return", elapsed)
			}
		})
	}
}
