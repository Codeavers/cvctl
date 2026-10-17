package project

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
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
	// RemoveVolume removes a volume created by RestoreFresh in this run.
	RemoveVolume(ctx context.Context, volume string) error
	// BaseBackup pushes a wal-g base backup of the running service.
	BaseBackup(ctx context.Context, inst, svc string) error
	// BucketGenerations returns the highest wal-g generation of svc holding
	// any data and the highest holding a base backup (-1: none).
	BucketGenerations(ctx context.Context, inst, svc string) (highestUsed, highestWithBase int, err error)
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

// cleanupReport tells what a failed run undid and what it could not undo, so a
// re-run starts from a known state. Key names only: values may be secrets.
type cleanupReport struct {
	cleaned, notRemoved []string
	manual              string
}

func (c cleanupReport) apply(err error) error {
	if len(c.cleaned) == 0 && len(c.notRemoved) == 0 && c.manual == "" {
		return err
	}
	var oe *output.Error
	var out output.Error
	if errors.As(err, &oe) {
		out = *oe
	} else {
		out = output.Error{Code: "E_RESTORE_FAILED", Message: err.Error(), Hint: "fix the cause and re-run cvctl project restore"}
	}
	if len(c.cleaned) > 0 {
		out.Message += "; cleaned up: " + strings.Join(c.cleaned, ", ")
	}
	if len(c.notRemoved) > 0 {
		out.Message += "; not removed: " + strings.Join(c.notRemoved, ", ")
	}
	if c.manual != "" {
		out.Message += "; " + c.manual
	}
	return &out
}

// revertHint names the cvctl.env keys to set back by hand when the revert write fails.
func revertHint(updates, orig map[string]string) string {
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		if v, ok := orig[k]; ok {
			parts = append(parts, fmt.Sprintf("set %s back to %s", k, v))
		} else {
			parts = append(parts, fmt.Sprintf("remove %s", k))
		}
	}
	return "could not revert backup generations in cvctl.env; " + strings.Join(parts, " and ") + " by hand"
}

func deployFailedErr(err error, detail, inst string, vols []string) error {
	hint := fmt.Sprintf("data restored into %s; fix the cause, run cvctl project update %s, then cvctl backup now %s <service> for each wal-g service", strings.Join(vols, ", "), inst, inst)
	var oe *output.Error
	if err != nil && errors.As(err, &oe) {
		return &output.Error{Code: oe.Code, Message: oe.Message, Hint: hint}
	}
	msg := fmt.Sprintf("deploy failed: %s", detail)
	if err != nil {
		msg = fmt.Sprintf("deploy failed: %v", err)
	}
	return &output.Error{Code: "E_DEPLOY_FAILED", Message: msg, Hint: hint}
}

// Restore rebuilds an instance on a new host from its imported secrets and the
// off-site backups. The instance lock is held throughout so the agent cannot
// deploy a half-restored instance. A failure before the deploy removes what
// this run created (clone, volumes, generation change) so a re-run starts
// clean; after the deploy starts, data and generations stay in place.
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

	origEnv, err := e.fs().ReadFile(cvctlEnvPath)
	if err != nil {
		return nil, err
	}
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
			Hint:    fmt.Sprintf("if a previous project restore failed after deploy, use cvctl project update %s; otherwise remove %s and re-run", inst, infraDir),
		}
	}

	res := &RestoreResult{Instance: inst}
	var created []string
	cloned, envWritten := false, false
	var genUpdates map[string]string
	fail := func(err error) (*RestoreResult, error) {
		cctx := context.WithoutCancel(ctx)
		var rep cleanupReport
		if envWritten {
			if werr := e.fs().WriteFile(cvctlEnvPath, origEnv, 0o640, deployOwner); werr == nil {
				rep.cleaned = append(rep.cleaned, "backup generations in cvctl.env")
			} else {
				rep.manual = revertHint(genUpdates, rawEnv)
			}
		}
		for _, v := range created {
			if rerr := e.Data.RemoveVolume(cctx, v); rerr == nil {
				rep.cleaned = append(rep.cleaned, "volume "+v)
			} else {
				rep.notRemoved = append(rep.notRemoved, "volume "+v)
			}
		}
		if cloned {
			if removeAll(e.fs(), infraDir) == nil {
				rep.cleaned = append(rep.cleaned, infraDir)
			} else {
				rep.notRemoved = append(rep.notRemoved, infraDir)
			}
		}
		return nil, rep.apply(err)
	}

	repo := e.repo(infraDir, keyPath)
	cloned = true
	if err := repo.Clone(ctx, repoURL, branch); err != nil {
		return fail(err)
	}
	cat, err := e.catalog()
	if err != nil {
		return fail(err)
	}
	p, _, err := loadContractAndSpec(e.fs(), infraDir, cat)
	if err != nil {
		return fail(err)
	}
	d, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return fail(err)
	}

	targets := backup.Targets(p, d)
	// The bucket, not the bundle, knows the newest generation: a bundle
	// exported before a db restore names an older one. Every new generation
	// goes above anything the bucket holds.
	usedGen := map[string]int{}
	for _, t := range targets {
		if t.Strategy != backup.StrategyWalG {
			continue
		}
		used, withBase, err := e.Data.BucketGenerations(ctx, inst, t.Service)
		if err != nil {
			return fail(err)
		}
		usedGen[t.Service] = used
		sd := d.Services[t.Service]
		if withBase >= 0 && withBase != sd.BackupGen {
			var w string
			if withBase > sd.BackupGen {
				w = fmt.Sprintf("%s: secrets bundle was stale: restoring generation %d from the bucket (the bundle names %d); re-run cvctl secrets export after the restore", t.Service, withBase, sd.BackupGen)
			} else {
				w = fmt.Sprintf("%s: generation %d named by the secrets bundle has no base backup: restoring generation %d from the bucket", t.Service, sd.BackupGen, withBase)
			}
			res.Warnings = append(res.Warnings, w)
			if e.Deployer.Notify != nil {
				_ = e.Deployer.Notify.Send(ctx, notify.Message{
					Host: e.Host.HostName, Level: "warn", Title: fmt.Sprintf("%s restore of %s: generation from the bucket", inst, t.Service), Text: w,
				})
			}
			sd.BackupGen = withBase
			d.Services[t.Service] = sd
		}
	}
	for _, t := range targets {
		vol := render.VolumeName(d, t.Service)
		if err := e.Data.RestoreFresh(ctx, inst, t.Service, p, d, vol); err != nil {
			return fail(err)
		}
		created = append(created, vol)
	}
	res.Volumes = created

	// The restored cluster starts a new timeline, so it must archive into a new
	// generation from its first start; advancing before the deploy makes the
	// rendered backup env point there, never at the generation restored from.
	var walg []string
	updates := map[string]string{}
	genUpdates = updates
	for _, t := range targets {
		if t.Strategy != backup.StrategyWalG {
			continue
		}
		sd := d.Services[t.Service]
		next := max(sd.BackupGenMax, sd.BackupGen, usedGen[t.Service]) + 1
		sd.BackupGen, sd.BackupGenMax = next, next
		d.Services[t.Service] = sd
		key := "CVCTL_SVC_" + contract.Normalize(t.Service) + "_BACKUP_GEN"
		updates[key] = strconv.Itoa(next)
		updates[key+"_MAX"] = strconv.Itoa(next)
		walg = append(walg, t.Service)
	}
	if len(updates) > 0 {
		merged := make(map[string]string, len(rawEnv)+len(updates))
		for k, v := range rawEnv {
			merged[k] = v
		}
		for k, v := range updates {
			merged[k] = v
		}
		if err := e.fs().WriteFile(cvctlEnvPath, instance.FormatEnv(merged), 0o640, deployOwner); err != nil {
			return fail(&output.Error{
				Code:    "E_FILE_WRITE",
				Message: fmt.Sprintf("cannot write %s: %v", cvctlEnvPath, err),
				Hint:    "check permissions and free space of the instance directory",
			})
		}
		envWritten = true
	}

	if err := e.Deployer.Store.Save(&state.State{
		Instance:  inst,
		Status:    state.StatusFailed,
		Detail:    "restore pending",
		UpdatedAt: time.Now(),
	}); err != nil {
		return fail(err)
	}
	res.Deploy, err = runDeploy(ctx, e, repo, d, "cli:project-restore", true)
	if err != nil {
		res.Deploy.Status = state.StatusFailed
		return res, deployFailedErr(err, "", inst, res.Volumes)
	}
	if res.Deploy.Status != state.StatusOK {
		return res, deployFailedErr(nil, res.Deploy.Detail, inst, res.Volumes)
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

// storableInfraURL drops userinfo from http(s) URLs, which would otherwise put
// a token into cvctl.env, and drops an ssh password; ssh user names are not
// secrets and stay.
func storableInfraURL(raw string) (string, bool) {
	if rest, ok := strings.CutPrefix(raw, "ssh://"); ok {
		authority := rest
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			authority = rest[:i]
		}
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			if user, _, hasPass := strings.Cut(authority[:at], ":"); hasPass {
				if user == "" {
					return "ssh://" + rest[at+1:], true
				}
				return "ssh://" + user + rest[at:], true
			}
		}
		return raw, false
	}
	for _, pre := range []string{"https://", "http://"} {
		rest, ok := strings.CutPrefix(raw, pre)
		if !ok {
			continue
		}
		authority := rest
		if i := strings.IndexAny(rest, "/?#"); i >= 0 {
			authority = rest[:i]
		}
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			return pre + rest[at+1:], true
		}
	}
	return raw, false
}

const strippedURLWarning = "credentials in the infra URL were not stored in cvctl.env; project restore on a new host needs a deploy key or --repo"

// recordedInfraURL asks git for the origin of an existing checkout; empty when
// unknown. The second result is true when credentials were stripped.
func recordedInfraURL(ctx context.Context, e *Env, infraDir string) (string, bool) {
	res, err := e.runner().Run(ctx, sys.Cmd{Name: "git", Args: []string{"-C", infraDir, "remote", "get-url", "origin"}})
	if err != nil || res.ExitCode != 0 {
		return "", false
	}
	url := string(bytes.TrimSpace(res.Stdout))
	if validateRepoURL(url) != nil {
		return "", false
	}
	return storableInfraURL(url)
}
