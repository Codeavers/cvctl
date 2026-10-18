package deploy

import (
	"context"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/gitrepo"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/sys"
)

// Wire sets up the production Compose, Health, and Repo factories on the Deployer.
func Wire(d *Deployer, run sys.Runner) {
	d.Run = run
	d.Allowed = AllowedRegistries(d.Host)
	if d.Backup == nil {
		d.Backup = BackupTargetFrom(d.Host, d.Secrets)
	}

	if d.Contracts == nil {
		d.Contracts = OCIFetcher(OCIAuth(d.Host, d.Secrets))
	}

	d.Repo = func(inst string) CheckoutAPI {
		keyPath := filepath.Join(d.stateRoot(), inst, "deploy_key")
		exists := false
		if d.FS != nil {
			if fi, err := d.FS.Stat(keyPath); err == nil && !fi.IsDir {
				exists = true
			}
		} else {
			if fi, err := os.Stat(keyPath); err == nil && !fi.IsDir() {
				exists = true
			}
		}
		if !exists {
			keyPath = ""
		}
		return &gitrepo.Repo{
			Run:     run,
			Dir:     filepath.Join(d.deployRoot(), inst, "infra"),
			KeyPath: keyPath,
		}
	}

	d.Compose = func(inst, override, envFile string) ComposeAPI {
		instDir := filepath.Join(d.deployRoot(), inst)
		return &compose.Project{
			Run:      run,
			Instance: inst,
			Dir:      instDir,
			Override: override,
			EnvFile:  envFile,
		}
	}

	if d.Restore == nil {
		d.Restore = &backup.RestorePoint{
			Compose: func(inst string) *compose.Project {
				return d.ComposeProject(run, inst)
			},
			Load: func(inst string) (*contract.Project, *instance.Decisions, error) {
				return backup.LoadInstance(d.deployRoot(), d.Catalog)(inst)
			},
		}
	}

	d.Health = func(ctx context.Context, inst, override string) error {
		instDir := filepath.Join(d.deployRoot(), inst)
		infraPath := readInfraPath(d.FS, d.deployRoot(), inst)
		infraDir := filepath.Join(instDir, "infra", infraPath)

		cat, err := d.catalog()
		if err != nil {
			return err
		}

		p, err := contract.Load(infraDir, cat)
		if err != nil {
			return err
		}

		port := p.HealthPort()
		proj := compose.Project{
			Run:      run,
			Instance: inst,
			Dir:      instDir,
			Override: override,
			EnvFile:  filepath.Join(instDir, "app.env"),
		}
		return compose.Health(ctx, run, proj, p.X.Health, port, nil)
	}
}

// ComposeProject is the compose project of an instance with the default
// override and app env paths.
func (d *Deployer) ComposeProject(run sys.Runner, inst string) *compose.Project {
	return &compose.Project{
		Run:      run,
		Instance: inst,
		Dir:      filepath.Join(d.deployRoot(), inst),
	}
}

func readInfraPath(fsys sys.FS, deployRoot, inst string) string {
	cvctlEnvPath := filepath.Join(deployRoot, inst, "cvctl.env")
	var raw []byte
	var err error
	if fsys != nil {
		raw, err = fsys.ReadFile(cvctlEnvPath)
	} else {
		raw, err = os.ReadFile(cvctlEnvPath)
	}
	if err != nil {
		return ""
	}
	m, err := godotenv.Unmarshal(string(raw))
	if err != nil {
		return ""
	}
	return m["CVCTL_INFRA_PATH"]
}

// BackupTargetFrom builds the wal-g destination from the host config and
// secrets. It returns nil (no backups) when any required piece is missing.
func BackupTargetFrom(cfg *hostcfg.Config, sec *hostcfg.Secrets) *render.BackupTarget {
	if cfg == nil || sec == nil {
		return nil
	}
	endpoint := cfg.S3EndpointInternal
	if endpoint == "" {
		endpoint = cfg.S3Endpoint
	}
	ak, _ := sec.Get("s3_access_key")
	sk, _ := sec.Get("s3_secret_key")
	key, _ := sec.Get("walg_libsodium_key")
	pass, _ := sec.Get("restic_password")
	if endpoint == "" || cfg.S3Bucket == "" || ak == "" || sk == "" || key == "" {
		return nil
	}
	region := cfg.S3Region
	if region == "" {
		region = "us-east-1"
	}
	return &render.BackupTarget{
		Host: cfg.HostName, Endpoint: endpoint, Bucket: cfg.S3Bucket, Region: region,
		AccessKey: ak, SecretKey: sk, LibsodiumKey: key, ResticPassword: pass,
		Local: cfg.Storage != "s3" && cfg.S3EndpointInternal != "",
	}
}
