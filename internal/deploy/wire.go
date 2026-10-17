package deploy

import (
	"context"
	"os"
	"path/filepath"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/sys"
)

// Wire sets up the production Compose and Health factories on the Deployer.
func Wire(d *Deployer, run sys.Runner) {
	d.Compose = func(inst, override, envFile string) ComposeAPI {
		instDir := filepath.Join(d.deployRoot(), inst)
		infraPath := readInfraPath(d.FS, d.deployRoot(), inst)
		return &compose.Project{
			Run:       run,
			Instance:  inst,
			Dir:       instDir,
			InfraPath: infraPath,
			Override:  override,
			EnvFile:   envFile,
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
			Run:       run,
			Instance:  inst,
			Dir:       instDir,
			InfraPath: infraPath,
			Override:  override,
			EnvFile:   filepath.Join(instDir, "app.env"),
		}
		return compose.Health(ctx, run, proj, p.X.Health, port, nil)
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
