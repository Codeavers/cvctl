package project

import (
	"bytes"
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
)

const keyInfraURL = "CVCTL_INFRA_URL"

// DataRestorer recovers backed-up data on a host that lost it.
type DataRestorer interface {
	// RestoreFresh restores the latest backup of svc into a volume that must not exist yet.
	RestoreFresh(ctx context.Context, inst, svc string, p *contract.Project, d *instance.Decisions, volume string) error
	// BaseBackup pushes a wal-g base backup of the running service.
	BaseBackup(ctx context.Context, inst, svc string) error
}

type RestoreOptions struct {
	// Repo overrides CVCTL_INFRA_URL; instances created before it was recorded need it.
	Repo string
}

type RestoreResult struct {
	Instance string        `json:"instance"`
	Deploy   deploy.Result `json:"-"`
	Volumes  []string      `json:"volumes"`
	Warnings []string      `json:"warnings,omitempty"`
}

func geni(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// Restore rebuilds an instance on a new host from its imported secrets and the
// off-site backups. The instance lock is held throughout so the agent cannot
// deploy a half-restored instance.
func Restore(ctx context.Context, e *Env, inst string, opts RestoreOptions) (*RestoreResult, error) {
	if err := ValidateInstanceName(e.deployRoot(), e.stateRoot(), inst); err != nil {
		return nil, err
	}
	if e.Host == nil || e.Host.Storage != "s3" {
		return nil, &output.Error{
			Code:    "E_RESTORE_LOCAL_STORAGE",
			Message: "project restore needs off-site backups (CVCTL_STORAGE=s3)",
			Hint:    "local backups died with the host; configure S3 storage with cvctl init and import the secrets first",
		}
	}
	instDir := filepath.Join(e.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	if _, err := e.fs().Stat(cvctlEnvPath); err != nil {
		return nil, &output.Error{
			Code:    "E_RESTORE_NO_SECRETS",
			Message: fmt.Sprintf("%s is missing", cvctlEnvPath),
			Hint:    "run cvctl secrets import <bundle> first",
		}
	}
	if e.Data == nil || e.Deployer == nil || e.Deployer.Store == nil {
		return nil, &output.Error{
			Code:    "E_RESTORE_MISCONFIGURED",
			Message: "project restore is not wired (data restorer, deployer or state store missing)",
			Hint:    "this is a cvctl bug; report it",
		}
	}

	unlock, err := e.Deployer.Store.Lock(inst)
	if err != nil {
		return nil, err
	}
	defer unlock()

	rawEnv, err := readRawEnv(e.fs(), cvctlEnvPath)
	if err != nil {
		return nil, err
	}
	repoURL := opts.Repo
	if repoURL == "" {
		repoURL = rawEnv[keyInfraURL]
	}
	if repoURL == "" {
		return nil, &output.Error{
			Code:    "E_RESTORE_NO_REPO",
			Message: fmt.Sprintf("the infra repository of %s is unknown (cvctl.env has no %s)", inst, keyInfraURL),
			Hint:    "pass --repo <infra-url>",
		}
	}
	if err := validateRepoURL(repoURL); err != nil {
		return nil, err
	}
	branch := rawEnv["CVCTL_INFRA_BRANCH"]
	if branch == "" {
		branch = "develop"
		if rawEnv["CVCTL_ENV"] == "prod" {
			branch = "main"
		}
	}

	keyPath := ""
	if kp := filepath.Join(e.stateRoot(), inst, "deploy_key"); fileExists(e.fs(), kp) {
		keyPath = kp
	}
	infraDir := filepath.Join(instDir, "infra")
	if fileExists(e.fs(), infraDir) {
		return nil, &output.Error{
			Code:    "E_RESTORE_INFRA_EXISTS",
			Message: fmt.Sprintf("%s already exists", infraDir),
			Hint:    "restore runs on a host without the instance; use cvctl project update, or remove the directory if it is a leftover",
		}
	}
	repo := e.repo(infraDir, keyPath)
	if err := repo.Clone(ctx, repoURL, branch); err != nil {
		return nil, err
	}

	cat, err := e.catalog()
	if err != nil {
		return nil, err
	}
	p, _, err := loadContractAndSpec(e.fs(), infraDir, cat)
	if err != nil {
		return nil, err
	}
	d, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return nil, err
	}

	res := &RestoreResult{Instance: inst}
	targets := backup.Targets(p, d)
	for _, t := range targets {
		vol := render.VolumeName(d, t.Service)
		if err := e.Data.RestoreFresh(ctx, inst, t.Service, p, d, vol); err != nil {
			return nil, err
		}
		res.Volumes = append(res.Volumes, vol)
	}

	if err := e.Deployer.Store.Save(&state.State{
		Instance:  inst,
		Status:    state.StatusFailed,
		Detail:    "restore pending",
		UpdatedAt: time.Now(),
	}); err != nil {
		return nil, err
	}
	res.Deploy, err = runDeploy(ctx, e, repo, d, "cli:project-restore", true)
	if err != nil {
		return res, err
	}
	if res.Deploy.Status != state.StatusOK {
		return res, nil
	}

	// The restored cluster starts a new timeline: it must archive into a new
	// generation, never into the one it was restored from.
	var walg []string
	updates := map[string]string{}
	for _, t := range targets {
		if t.Strategy != backup.StrategyWalG {
			continue
		}
		sd := d.Services[t.Service]
		next := max(sd.BackupGenMax, sd.BackupGen) + 1
		sd.BackupGen, sd.BackupGenMax = next, next
		d.Services[t.Service] = sd
		key := "CVCTL_SVC_" + contract.Normalize(t.Service) + "_BACKUP_GEN"
		updates[key] = strconv.Itoa(next)
		updates[key+"_MAX"] = strconv.Itoa(next)
		walg = append(walg, t.Service)
	}
	if len(walg) == 0 {
		return res, nil
	}
	raw, err := readRawEnv(e.fs(), cvctlEnvPath)
	if err != nil {
		return res, err
	}
	for k, v := range updates {
		raw[k] = v
	}
	if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(raw), 0o640, deployOwner); err != nil {
		return res, &output.Error{
			Code:    "E_FILE_WRITE",
			Message: fmt.Sprintf("cannot write %s: %v", cvctlEnvPath, err),
			Hint:    "check permissions and free space of the instance directory",
		}
	}
	st, err := e.Deployer.Store.Load(inst)
	if err != nil {
		return res, err
	}
	if st.Current == nil {
		return res, nil
	}
	if err := e.Deployer.Reapply(ctx, inst, st.Current); err != nil {
		return res, err
	}
	for _, svc := range walg {
		if err := e.Data.BaseBackup(ctx, inst, svc); err != nil {
			res.Warnings = append(res.Warnings, fmt.Sprintf("base backup of %s failed: %v; run cvctl backup now %s %s", svc, err, inst, svc))
		}
	}
	return res, nil
}

func fileExists(fsys sys.FS, p string) bool {
	_, err := fsys.Lstat(p)
	return err == nil
}

// recordedInfraURL asks git for the origin of an existing checkout; empty when
// unknown.
func recordedInfraURL(ctx context.Context, e *Env, infraDir string) string {
	res, err := e.runner().Run(ctx, sys.Cmd{Name: "git", Args: []string{"-C", infraDir, "remote", "get-url", "origin"}})
	if err != nil || res.ExitCode != 0 {
		return ""
	}
	url := string(bytes.TrimSpace(res.Stdout))
	if validateRepoURL(url) != nil {
		return ""
	}
	return url
}
