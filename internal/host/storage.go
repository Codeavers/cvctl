package host

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"slices"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/objstore"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

const (
	storageDir         = "/etc/cvctl/storage"
	storageComposePath = "/etc/cvctl/storage/compose.yml"
	storageSecretsPath = "/etc/cvctl/secrets/storage.env"
	storageProject     = "cvctl-storage"
	storageNetwork     = "cvctl-storage"
	storageVolume      = "cvctl-storage-data"

	localS3Endpoint = "http://127.0.0.1:9100"
	localS3Bucket   = "cvctl"
	localS3Region   = "us-east-1"

	defaultStoragePollInterval = 2 * time.Second
	defaultStoragePollTimeout  = 60 * time.Second

	alphanumChars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
)

var newStore = objstore.New

type StorageStep struct {
	PollInterval time.Duration
	PollTimeout  time.Duration
}

func (StorageStep) Name() string { return "storage" }

type storageComposeConfig struct {
	Services map[string]storageComposeService `yaml:"services"`
	Networks map[string]map[string]any        `yaml:"networks"`
	Volumes  map[string]map[string]any        `yaml:"volumes"`
}

type storageComposeService struct {
	Image    string   `yaml:"image"`
	Command  []string `yaml:"command"`
	Restart  string   `yaml:"restart"`
	Ports    []string `yaml:"ports"`
	EnvFile  []string `yaml:"env_file"`
	Volumes  []string `yaml:"volumes"`
	Networks []string `yaml:"networks"`
}

func effectiveStorageMode(cfg *hostcfg.Config) string {
	if cfg != nil && cfg.Storage != "" {
		return cfg.Storage
	}
	return "local"
}

func renderStorageCompose() ([]byte, error) {
	cat, err := kinds.Load()
	if err != nil {
		return nil, err
	}
	k, ok := cat["s3"]
	if !ok {
		return nil, &output.Error{
			Code:    "E_STORAGE_FAILED",
			Message: "s3 kind not found in catalog",
			Hint:    "verify kinds/s3.yml is embedded",
		}
	}
	img, err := k.ImageFor(k.DefaultVersion)
	if err != nil {
		return nil, err
	}

	doc := storageComposeConfig{
		Services: map[string]storageComposeService{
			"s3": {
				Image:    img,
				Command:  []string{"server", "/data"},
				Restart:  "unless-stopped",
				Ports:    []string{"127.0.0.1:9100:9000"},
				EnvFile:  []string{storageSecretsPath},
				Volumes:  []string{storageVolume + ":/data"},
				Networks: []string{storageNetwork},
			},
		},
		Networks: map[string]map[string]any{
			storageNetwork: {},
		},
		Volumes: map[string]map[string]any{
			storageVolume: {},
		},
	}
	return marshalYAML(doc)
}

func renderStorageEnv(ak, sk string) []byte {
	return instance.FormatEnv(map[string]string{
		"MINIO_ROOT_USER":     ak,
		"MINIO_ROOT_PASSWORD": sk,
	})
}

func storageConfigFor(cfg *hostcfg.Config, mode, ak, sk string) objstore.Config {
	var ep, bucket, region string
	if cfg != nil {
		ep = cfg.S3Endpoint
		bucket = cfg.S3Bucket
		region = cfg.S3Region
	}
	if mode == "local" {
		if ep == "" {
			ep = localS3Endpoint
		}
		if bucket == "" {
			bucket = localS3Bucket
		}
		if region == "" {
			region = localS3Region
		}
	}
	return objstore.Config{
		Endpoint:  ep,
		Bucket:    bucket,
		Region:    region,
		AccessKey: ak,
		SecretKey: sk,
	}
}

func (StorageStep) Check(ctx context.Context, e *Env) (Plan, error) {
	mode := effectiveStorageMode(e.Cfg)
	var ak, sk string
	if e.Secrets != nil {
		ak, _ = e.Secrets.Get("s3_access_key")
		sk, _ = e.Secrets.Get("s3_secret_key")
	}

	var changes []string
	if mode == "local" {
		composeBytes, err := renderStorageCompose()
		if err != nil {
			return Plan{}, err
		}
		composeNeedsWrite, err := fileNeedsWrite(e.FS, storageComposePath, composeBytes, 0o640, rootDeployOwner)
		if err != nil {
			return Plan{}, storageFSErr(storageComposePath, err)
		}
		if composeNeedsWrite {
			changes = append(changes, "configure "+storageComposePath)
		}

		if ak == "" || sk == "" {
			changes = append(changes, "configure "+storageSecretsPath)
		} else {
			envBytes := renderStorageEnv(ak, sk)
			envNeedsWrite, err := fileNeedsWrite(e.FS, storageSecretsPath, envBytes, 0o640, rootDeployOwner)
			if err != nil {
				return Plan{}, storageFSErr(storageSecretsPath, err)
			}
			if envNeedsWrite {
				changes = append(changes, "configure "+storageSecretsPath)
			}
		}

		if e.Cfg == nil || e.Cfg.S3Endpoint == "" || e.Cfg.S3Bucket == "" || e.Cfg.S3Region == "" {
			if !slices.Contains(changes, "configure "+storageSecretsPath) {
				changes = append(changes, "configure local storage settings")
			}
		}

		if ak == "" || sk == "" {
			changes = append(changes, "verify storage")
			return Plan{Changes: changes}, nil
		}
	}

	store, err := newStore(storageConfigFor(e.Cfg, mode, ak, sk))
	if err != nil || objstore.Probe(ctx, store) != nil {
		changes = append(changes, "verify storage")
	}

	return Plan{Changes: changes}, nil
}

func (s StorageStep) Apply(ctx context.Context, e *Env, p Plan) (string, error) {
	if len(p.Changes) == 0 {
		return "", nil
	}

	mode := effectiveStorageMode(e.Cfg)
	if mode == "s3" {
		var ak, sk string
		if e.Secrets != nil {
			ak, _ = e.Secrets.Get("s3_access_key")
			sk, _ = e.Secrets.Get("s3_secret_key")
		}
		scfg := storageConfigFor(e.Cfg, mode, ak, sk)
		store, err := newStore(scfg)
		if err != nil {
			return "", err
		}
		if err := store.EnsureBucket(ctx); err != nil {
			return "", &output.Error{
				Code:    "E_STORAGE_BUCKET",
				Message: fmt.Sprintf("cannot ensure bucket %s: %v", scfg.Bucket, err),
				Hint:    fmt.Sprintf("create bucket %s in your provider", scfg.Bucket),
			}
		}
		if err := objstore.Probe(ctx, store); err != nil {
			return "", err
		}
		return "", nil
	}

	if e.Cfg == nil {
		e.Cfg = hostcfg.NewConfig()
	}
	if e.Secrets == nil {
		e.Secrets = hostcfg.NewSecrets()
	}

	ak, okAK := e.Secrets.Get("s3_access_key")
	if !okAK {
		var err error
		ak, err = randomAlnum(20)
		if err != nil {
			return "", err
		}
		e.Secrets.Set("s3_access_key", ak)
	}
	sk, okSK := e.Secrets.Get("s3_secret_key")
	if !okSK {
		var err error
		sk, err = randomAlnum(40)
		if err != nil {
			return "", err
		}
		e.Secrets.Set("s3_secret_key", sk)
	}

	if e.Cfg.S3Endpoint == "" {
		if err := e.Cfg.Set("S3_ENDPOINT", localS3Endpoint); err != nil {
			return "", err
		}
	}
	if e.Cfg.S3Bucket == "" {
		if err := e.Cfg.Set("S3_BUCKET", localS3Bucket); err != nil {
			return "", err
		}
	}
	if e.Cfg.S3Region == "" {
		if err := e.Cfg.Set("S3_REGION", localS3Region); err != nil {
			return "", err
		}
	}

	if err := hostcfg.Save(e.FS, "/etc/cvctl", e.Cfg, e.Secrets); err != nil {
		return "", err
	}
	if err := fixSavedCfgPermissions(e.FS); err != nil {
		return "", err
	}

	composeBytes, err := renderStorageCompose()
	if err != nil {
		return "", err
	}
	envBytes := renderStorageEnv(ak, sk)

	composeChanged, err := fileNeedsWrite(e.FS, storageComposePath, composeBytes, 0o640, rootDeployOwner)
	if err != nil {
		return "", storageFSErr(storageComposePath, err)
	}
	envChanged, err := fileNeedsWrite(e.FS, storageSecretsPath, envBytes, 0o640, rootDeployOwner)
	if err != nil {
		return "", storageFSErr(storageSecretsPath, err)
	}

	if composeChanged {
		if err := e.FS.MkdirAll(storageDir, 0o750, rootDeployOwner); err != nil {
			return "", storageFSErr(storageDir, err)
		}
		if err := e.FS.WriteFile(storageComposePath, composeBytes, 0o640, rootDeployOwner); err != nil {
			return "", storageFSErr(storageComposePath, err)
		}
	}

	if envChanged {
		if err := e.FS.MkdirAll("/etc/cvctl/secrets", 0o750, rootDeployOwner); err != nil {
			return "", storageFSErr("/etc/cvctl/secrets", err)
		}
		if err := e.FS.WriteFile(storageSecretsPath, envBytes, 0o640, rootDeployOwner); err != nil {
			return "", storageFSErr(storageSecretsPath, err)
		}
	}

	upArgs := []string{"compose", "-p", storageProject, "-f", storageComposePath, "up", "-d"}
	if composeChanged || envChanged {
		upArgs = append(upArgs, "--force-recreate")
	}
	if err := runStorageCmd(ctx, e, sys.Cmd{
		Name: "docker",
		Args: upArgs,
	}); err != nil {
		return "", err
	}

	store, err := newStore(storageConfigFor(e.Cfg, mode, ak, sk))
	if err != nil {
		return "", err
	}

	if err := s.waitForBucket(ctx, store); err != nil {
		return "", err
	}

	if err := objstore.Probe(ctx, store); err != nil {
		return "", err
	}

	return "", nil
}

func (s StorageStep) waitForBucket(ctx context.Context, store objstore.Store) error {
	interval := s.PollInterval
	if interval <= 0 {
		interval = defaultStoragePollInterval
	}
	timeout := s.PollTimeout
	if timeout <= 0 {
		timeout = defaultStoragePollTimeout
	}

	deadline := time.Now().Add(timeout)
	for {
		err := store.EnsureBucket(ctx)
		if err == nil {
			return nil
		}
		if isStorageAuthError(err) {
			return err
		}
		if time.Now().After(deadline) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func isStorageAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "AccessDenied") ||
		strings.Contains(msg, "InvalidAccessKeyId") ||
		strings.Contains(msg, "SignatureDoesNotMatch")
}

func randomAlnum(n int) (string, error) {
	max := big.NewInt(int64(len(alphanumChars)))
	out := make([]byte, n)
	for i := range n {
		idx, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		out[i] = alphanumChars[idx.Int64()]
	}
	return string(out), nil
}

// fixSavedCfgPermissions restores root:deploy on /etc/cvctl, /etc/cvctl/secrets and
// files written by hostcfg.Save (which uses root:root so it can run before AccountsStep).
func fixSavedCfgPermissions(fsys sys.FS) error {
	if err := fsys.MkdirAll("/etc/cvctl", 0o750, rootDeployOwner); err != nil {
		return storageFSErr("/etc/cvctl", err)
	}
	if err := fsys.MkdirAll("/etc/cvctl/secrets", 0o750, rootDeployOwner); err != nil {
		return storageFSErr("/etc/cvctl/secrets", err)
	}
	for _, f := range accountFiles() {
		data, err := fsys.ReadFile(f)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return storageFSErr(f, err)
		}
		if err := fsys.WriteFile(f, data, 0o640, rootDeployOwner); err != nil {
			return storageFSErr(f, err)
		}
	}
	return nil
}

func runStorageCmd(ctx context.Context, e *Env, c sys.Cmd) error {
	res, err := e.Run.Run(ctx, c)
	if err != nil {
		return err
	}
	if res.ExitCode != 0 {
		stderr := strings.TrimSpace(string(res.Stderr))
		msg := fmt.Sprintf("%s failed (exit %d)", c.String(), res.ExitCode)
		if stderr != "" {
			msg += ": " + stderr
		}
		return &output.Error{
			Code:    "E_STORAGE_FAILED",
			Message: msg,
			Hint:    fmt.Sprintf("check `%s` output and docker status", c.String()),
		}
	}
	return nil
}

func storageFSErr(p string, err error) error {
	return &output.Error{
		Code:    "E_STORAGE_FAILED",
		Message: fmt.Sprintf("cannot write %s: %v", p, err),
		Hint:    fmt.Sprintf("check permissions and disk space for %s", p),
	}
}
