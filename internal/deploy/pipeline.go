package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}

// ComposeAPI abstracts docker compose operations for an instance.
type ComposeAPI interface {
	Config(ctx context.Context) error
	Pull(ctx context.Context, services ...string) error
	UpData(ctx context.Context, services []string) error
	RunHook(ctx context.Context, service, script string) (sys.Result, error)
	Up(ctx context.Context) error
	Logs(ctx context.Context, service string, tail int) (string, error)
}

// CheckoutAPI defines git checkout capabilities.
type CheckoutAPI interface {
	Checkout(ctx context.Context, commit string) error
}

// RestorePointer manages snapshot/restore points (e.g. WAL-G/Postgres).
type RestorePointer interface {
	Create(ctx context.Context, inst, id string) error
}

// NoopRestore is a dummy RestorePointer implementation.
type NoopRestore struct{}

func (NoopRestore) Create(ctx context.Context, inst, id string) error {
	return nil
}

// Options configures deployment behavior.
type Options struct {
	Force bool
	Actor string
}

// Result describes the outcome of a deploy or rollback operation.
type Result struct {
	Status  state.Status
	ID      string
	Missing []string
	Detail  string
}

// Deployer coordinates instance deployment, promotion, health checks, and rollback.
type Deployer struct {
	DeployRoot  string
	TraefikDir  string
	FS          sys.FS
	Store       *state.Store
	Compose     func(override string) ComposeAPI
	Repo        CheckoutAPI
	Notify      notify.Notifier
	Restore     RestorePointer
	Host        *hostcfg.Config
	Catalog     kinds.Catalog
	Clock       func() time.Time
	Health      func(ctx context.Context, inst string, override string) error
	HealthCheck func(ctx context.Context, inst string, override string) error
}

func (d *Deployer) deployRoot() string {
	if d.DeployRoot != "" {
		return d.DeployRoot
	}
	return "/srv/deploy"
}

func (d *Deployer) traefikDir() string {
	if d.TraefikDir != "" {
		return d.TraefikDir
	}
	return "/etc/traefik/dynamic"
}

func (d *Deployer) fs() sys.FS {
	if d.FS != nil {
		return d.FS
	}
	return sys.NewOSFS()
}

func (d *Deployer) clock() time.Time {
	if d.Clock != nil {
		return d.Clock()
	}
	return time.Now()
}

func (d *Deployer) restore() RestorePointer {
	if d.Restore != nil {
		return d.Restore
	}
	return NoopRestore{}
}

func (d *Deployer) notifier() notify.Notifier {
	if d.Notify != nil {
		return d.Notify
	}
	return notify.NoopNotifier{}
}

func (d *Deployer) catalog() (kinds.Catalog, error) {
	if d.Catalog != nil {
		return d.Catalog, nil
	}
	return kinds.Load()
}

func (d *Deployer) edge() string {
	if d.Host != nil && d.Host.EdgeMode != "" {
		return d.Host.EdgeMode
	}
	return "edge"
}

func (d *Deployer) hostName() string {
	if d.Host != nil {
		return d.Host.HostName
	}
	return ""
}

func (d *Deployer) checkHealth(ctx context.Context, inst string, override string) error {
	if d.Health != nil {
		return d.Health(ctx, inst, override)
	}
	if d.HealthCheck != nil {
		return d.HealthCheck(ctx, inst, override)
	}
	return nil
}

func (d *Deployer) getCompose(override string) ComposeAPI {
	if d.Compose != nil {
		return d.Compose(override)
	}
	return nil
}

func (d *Deployer) ensureDiskFile(path string) {
	if d.FS == nil {
		return
	}
	if _, err := os.Stat(path); err == nil {
		return
	}
	data, err := d.FS.ReadFile(path)
	if err == nil {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
}

func (d *Deployer) readAppEnv(appEnvPath string) (map[string]string, error) {
	var data []byte
	var err error
	if d.FS != nil {
		data, err = d.FS.ReadFile(appEnvPath)
	}
	if d.FS == nil || err != nil {
		data, err = os.ReadFile(appEnvPath)
	}
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, nil
		}
		return map[string]string{}, nil
	}
	return godotenv.Unmarshal(string(data))
}

func (d *Deployer) cleanupStaging(stagingDir, override, appEnv, route string) {
	fsys := d.fs()
	_ = fsys.Remove(override)
	_ = fsys.Remove(appEnv)
	_ = fsys.Remove(route)
	_ = fsys.Remove(stagingDir)
}

func (d *Deployer) abortDeploy(ctx context.Context, inst string, st *state.State, want state.Desired, deployID, actor string, now time.Time, detail string) (Result, error) {
	st.Status = state.StatusFailed
	st.FailedHash = want.Hash()
	st.UpdatedAt = now
	_ = d.Store.Save(st)

	_ = d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "failed",
		From:   st.Current,
		To:     &want,
		Result: string(state.StatusFailed),
		Detail: detail,
		Actor:  actor,
	})
	_ = d.notifier().Send(ctx, notify.Message{
		Host:  d.hostName(),
		Level: "error",
		Title: fmt.Sprintf("%s deploy FAILED", inst),
		Text:  detail,
	})

	if st.Current != nil && st.Current.InfraCommit != "" && d.Repo != nil {
		_ = d.Repo.Checkout(ctx, st.Current.InfraCommit)
	}

	return Result{
		Status: state.StatusFailed,
		ID:     deployID,
		Detail: detail,
	}, nil
}

func redact(text string, envValues map[string]string) string {
	var secrets []string
	for _, v := range envValues {
		if len(v) >= 8 {
			secrets = append(secrets, v)
		}
	}
	slices.SortFunc(secrets, func(a, b string) int {
		if len(a) != len(b) {
			return len(b) - len(a) // longest first
		}
		return strings.Compare(a, b)
	})
	var unique []string
	seen := make(map[string]bool)
	for _, s := range secrets {
		if !seen[s] {
			seen[s] = true
			unique = append(unique, s)
		}
	}
	out := text
	for _, s := range unique {
		out = strings.ReplaceAll(out, s, "***")
	}
	return out
}

func lastLines(text string, n int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= n {
		return text
	}
	return strings.Join(lines[len(lines)-n:], "\n")
}

// Deploy executes the deployment pipeline for an instance following the recreate strategy.
func (d *Deployer) Deploy(ctx context.Context, inst string, want state.Desired, opts Options) (Result, error) {
	actor := opts.Actor
	if actor == "" {
		actor = "agent"
	}
	now := d.clock()
	deployID := fmt.Sprintf("deploy-%d", now.Unix())

	// Step 1: Lock instance and load state
	unlock, err := d.Store.Lock(inst)
	if err != nil {
		return Result{}, err
	}
	defer unlock()

	st, err := d.Store.Load(inst)
	if err != nil {
		return Result{}, err
	}

	if st.Status == state.StatusPaused && !opts.Force {
		return Result{
			Status: state.StatusPaused,
			ID:     deployID,
			Detail: st.PausedReason,
		}, nil
	}
	if st.FailedHash != "" && want.Hash() == st.FailedHash && !opts.Force {
		return Result{
			Status: state.StatusFailed,
			ID:     deployID,
			Detail: "desired state previously failed",
		}, nil
	}
	if st.Current != nil && want.Hash() == st.Current.Hash() && !opts.Force {
		return Result{
			Status: state.StatusOK,
			ID:     deployID,
			Detail: "already at desired state",
		}, nil
	}

	// Step 2: Checkout want.InfraCommit, load contract, spec, decisions, Validate + Check
	if d.Repo != nil {
		if err := d.Repo.Checkout(ctx, want.InfraCommit); err != nil {
			return Result{}, err
		}
	}

	instDir := filepath.Join(d.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")
	d.ensureDiskFile(cvctlEnvPath)

	d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
	if err != nil {
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("cannot read %s: %v", cvctlEnvPath, err))
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	infraDir := filepath.Join(instDir, "infra", infraPath)
	d.ensureDiskFile(filepath.Join(infraDir, "compose.yml"))
	d.ensureDiskFile(filepath.Join(infraDir, "env.spec.yml"))

	cat, err := d.catalog()
	if err != nil {
		return Result{}, err
	}

	p, err := contract.Load(infraDir, cat)
	if err != nil {
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
	}

	dec, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("cannot read %s: %v", cvctlEnvPath, err))
	}
	if dec.InfraPath == "" {
		dec.InfraPath = "."
	}

	specPath := filepath.Join(infraDir, "env.spec.yml")
	spec, err := envspec.Load(specPath)
	if err != nil {
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
	}

	// Digests from want.Digests are verified
	if dec.Digests == nil {
		dec.Digests = make(map[string]string)
	}
	for k, v := range want.Digests {
		dec.Digests[k] = v
	}

	issues := append(contract.Validate(p, cat, spec), instance.Check(dec, p, cat)...)
	if len(issues) > 0 {
		output.SortIssues(issues)
		var issueMsgs []string
		for _, iss := range issues {
			issueMsgs = append(issueMsgs, fmt.Sprintf("%s: %s", iss.Code, iss.Message))
		}
		detail := "E_DEPLOY_INVALID: " + strings.Join(issueMsgs, "; ")
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, detail)
	}

	// Step 3: ServiceVars -> Resolve with current app.env
	finalAppEnvPath := filepath.Join(instDir, "app.env")
	existingEnv, err := d.readAppEnv(finalAppEnvPath)
	if err != nil {
		return Result{}, err
	}

	svcVars, err := render.ServiceVars(p, dec, cat)
	if err != nil {
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
	}

	resEnv, err := envspec.Resolve(spec, envspec.Input{
		Env:         dec.Env,
		Existing:    existingEnv,
		ServiceVars: svcVars,
		Domains:     dec.Domains,
	})
	if err != nil {
		return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
	}

	if len(resEnv.Missing) > 0 {
		st.Status = state.StatusBlocked
		st.Blocked = resEnv.Missing
		st.UpdatedAt = now
		_ = d.Store.Save(st)

		var setEnvCmds []string
		for _, k := range resEnv.Missing {
			setEnvCmds = append(setEnvCmds, fmt.Sprintf("cvctl set-env %s %s", inst, k))
		}
		warnText := strings.Join(setEnvCmds, "\n")
		_ = d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "blocked",
			From:   st.Current,
			To:     &want,
			Result: string(state.StatusBlocked),
			Detail: warnText,
			Actor:  actor,
		})
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "warn",
			Title: fmt.Sprintf("%s deploy BLOCKED", inst),
			Text:  warnText,
		})

		return Result{
			Status:  state.StatusBlocked,
			ID:      deployID,
			Missing: resEnv.Missing,
			Detail:  warnText,
		}, nil
	}

	st.Blocked = nil

	// Step 4: Render into staging directory <dir>/.staging/
	stagingDir := filepath.Join(instDir, ".staging")
	stagingOverridePath := filepath.Join(stagingDir, "compose.cvctl.yml")
	stagingAppEnvPath := filepath.Join(stagingDir, "app.env")
	stagingRoutePath := filepath.Join(stagingDir, inst+".yml")
	finalOverridePath := filepath.Join(instDir, "compose.cvctl.yml")
	finalRoutePath := filepath.Join(d.traefikDir(), inst+".yml")

	appEnvBytes := instance.FormatEnv(resEnv.Values)
	renderOpts := render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  filepath.Join(instDir, "app.env"),
	}
	overrideBytes, err := render.Override(p, dec, cat, renderOpts)
	if err != nil {
		return Result{}, err
	}
	routeBytes, err := render.Traefik(p, dec, renderOpts)
	if err != nil {
		return Result{}, err
	}

	fsys := d.fs()
	if err := fsys.MkdirAll(stagingDir, 0o2775, deployOwner); err != nil {
		return Result{}, err
	}
	if err := fsys.WriteFile(stagingOverridePath, overrideBytes, 0o640, deployOwner); err != nil {
		return Result{}, err
	}
	if err := fsys.WriteFile(stagingAppEnvPath, appEnvBytes, 0o640, deployOwner); err != nil {
		return Result{}, err
	}
	if routeBytes != nil {
		if err := fsys.WriteFile(stagingRoutePath, routeBytes, 0o640, deployOwner); err != nil {
			return Result{}, err
		}
	}

	stagingCompose := d.getCompose(stagingOverridePath)
	if stagingCompose != nil {
		if err := stagingCompose.Config(ctx); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
		}
	}

	// Step 5: If prod, create restore point
	if dec.Env == "prod" {
		if err := d.restore().Create(ctx, inst, deployID); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("restore point failed: %v", err))
		}
	}

	// Step 6: Pull app services, then UpData local data services
	var appServices []string
	for _, a := range p.Apps {
		appServices = append(appServices, a.Name)
	}
	var localDataServices []string
	for _, ds := range p.Data {
		if dec.Services[ds.Name].Mode == "local" {
			localDataServices = append(localDataServices, ds.Name)
		}
	}

	if stagingCompose != nil {
		if err := stagingCompose.Pull(ctx, appServices...); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
		}
		if err := stagingCompose.UpData(ctx, localDataServices); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.abortDeploy(ctx, inst, st, want, deployID, actor, now, err.Error())
		}
	}

	// Step 7: Pre-deploy hooks
	preHooksRan := false
	if stagingCompose != nil {
		for _, hook := range p.X.Hooks.PreDeploy {
			preHooksRan = true
			res, err := stagingCompose.RunHook(ctx, hook.Service, hook.Run)
			if err != nil || res.ExitCode != 0 {
				d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)

				rawOutput := string(res.Stderr)
				if strings.TrimSpace(rawOutput) == "" {
					rawOutput = string(res.Stdout)
				}
				if strings.TrimSpace(rawOutput) == "" && err != nil {
					rawOutput = err.Error()
				}
				redactedOut := redact(lastLines(rawOutput, 20), resEnv.Values)

				st.Status = state.StatusFailed
				st.FailedHash = want.Hash()
				st.UpdatedAt = now
				_ = d.Store.Save(st)

				_ = d.Store.Append(inst, state.Event{
					ID:     deployID,
					At:     now,
					Kind:   "failed",
					From:   st.Current,
					To:     &want,
					Result: string(state.StatusFailed),
					Detail: redactedOut,
					Actor:  actor,
				})
				_ = d.notifier().Send(ctx, notify.Message{
					Host:  d.hostName(),
					Level: "error",
					Title: fmt.Sprintf("%s deploy FAILED", inst),
					Text:  redactedOut,
				})

				if st.Current != nil && st.Current.InfraCommit != "" && d.Repo != nil {
					_ = d.Repo.Checkout(ctx, st.Current.InfraCommit)
				}

				return Result{
					Status: state.StatusFailed,
					ID:     deployID,
					Detail: redactedOut,
				}, nil
			}
		}
	}

	// Step 8: Promote staging -> final
	if err := fsys.WriteFile(finalOverridePath, overrideBytes, 0o640, deployOwner); err != nil {
		return Result{}, err
	}
	if err := fsys.WriteFile(finalAppEnvPath, appEnvBytes, 0o640, deployOwner); err != nil {
		return Result{}, err
	}
	if routeBytes != nil {
		if err := fsys.WriteFile(finalRoutePath, routeBytes, 0o640, deployOwner); err != nil {
			return Result{}, err
		}
	} else {
		_ = fsys.Remove(finalRoutePath)
	}
	d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)

	finalCompose := d.getCompose(finalOverridePath)
	var upErr error
	if finalCompose != nil {
		upErr = finalCompose.Up(ctx)
	}

	// Step 9: HealthCheck
	var healthErr error
	if upErr == nil {
		healthErr = d.checkHealth(ctx, inst, finalOverridePath)
	}

	if upErr == nil && healthErr == nil {
		// Post-deploy hooks
		if finalCompose != nil {
			for _, hook := range p.X.Hooks.PostDeploy {
				hRes, hErr := finalCompose.RunHook(ctx, hook.Service, hook.Run)
				if hErr != nil || hRes.ExitCode != 0 {
					rawHook := string(hRes.Stderr)
					if strings.TrimSpace(rawHook) == "" {
						rawHook = string(hRes.Stdout)
					}
					if strings.TrimSpace(rawHook) == "" && hErr != nil {
						rawHook = hErr.Error()
					}
					redactedHook := redact(lastLines(rawHook, 20), resEnv.Values)
					_ = d.notifier().Send(ctx, notify.Message{
						Host:  d.hostName(),
						Level: "warn",
						Title: fmt.Sprintf("%s deploy OK", inst),
						Text:  fmt.Sprintf("post-deploy hook failed: %s", redactedHook),
					})
				}
			}
		}

		st.Previous = st.Current
		st.Current = &want
		st.Status = state.StatusOK
		st.FailedHash = ""
		st.RollbackFailed = false
		st.Blocked = nil
		st.UpdatedAt = now
		_ = d.Store.Save(st)

		_ = d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "deploy",
			From:   st.Previous,
			To:     st.Current,
			Result: string(state.StatusOK),
			Detail: fmt.Sprintf("commit %s deployed", commitShort(want.InfraCommit)),
			Actor:  actor,
		})
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "info",
			Title: fmt.Sprintf("%s deploy OK", inst),
			Text:  fmt.Sprintf("commit %s deployed successfully", commitShort(want.InfraCommit)),
		})

		return Result{
			Status: state.StatusOK,
			ID:     deployID,
			Detail: fmt.Sprintf("commit %s deployed", commitShort(want.InfraCommit)),
		}, nil
	}

	// Step 10: Rollback to st.Current
	var logs string
	if finalCompose != nil {
		logs, _ = finalCompose.Logs(ctx, "", 20)
	}
	if strings.TrimSpace(logs) == "" {
		if upErr != nil {
			logs = upErr.Error()
		} else if healthErr != nil {
			logs = healthErr.Error()
		}
	}
	redactedLogs := redact(lastLines(logs, 20), resEnv.Values)

	if st.Current == nil || st.Current.InfraCommit == "" {
		st.Status = state.StatusFailed
		st.FailedHash = want.Hash()
		st.RollbackFailed = false
		st.UpdatedAt = now
		_ = d.Store.Save(st)

		_ = d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "failed",
			From:   nil,
			To:     &want,
			Result: string(state.StatusFailed),
			Detail: redactedLogs,
			Actor:  actor,
		})
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "error",
			Title: fmt.Sprintf("%s deploy FAILED", inst),
			Text:  redactedLogs,
		})

		return Result{
			Status: state.StatusFailed,
			ID:     deployID,
			Detail: redactedLogs,
		}, nil
	}

	rollbackTarget := *st.Current
	rollbackErr := d.executeSwap(ctx, inst, &rollbackTarget, resEnv.Values)

	st.Status = state.StatusFailed
	st.FailedHash = want.Hash()
	if rollbackErr != nil {
		st.RollbackFailed = true
	} else {
		st.RollbackFailed = false
	}
	st.UpdatedAt = now
	_ = d.Store.Save(st)

	errText := redactedLogs
	if preHooksRan && dec.Env == "prod" {
		dataSvc := ""
		for _, ds := range p.Data {
			if ds.Backup == "wal-g" || ds.Kind == "postgres" {
				dataSvc = ds.Name
				break
			}
		}
		if dataSvc == "" && len(p.Data) > 0 {
			dataSvc = p.Data[0].Name
		}
		if dataSvc == "" && len(p.X.Hooks.PreDeploy) > 0 {
			dataSvc = p.X.Hooks.PreDeploy[0].Service
		}
		errText += fmt.Sprintf("\nmigrations ran; restore point %s: cvctl db restore %s %s --to %s", deployID, inst, dataSvc, deployID)
	}

	_ = d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "rollback",
		From:   st.Current,
		To:     &want,
		Result: string(state.StatusFailed),
		Detail: errText,
		Actor:  actor,
	})
	_ = d.notifier().Send(ctx, notify.Message{
		Host:  d.hostName(),
		Level: "error",
		Title: fmt.Sprintf("%s deploy FAILED", inst),
		Text:  errText,
	})

	return Result{
		Status: state.StatusFailed,
		ID:     deployID,
		Detail: errText,
	}, nil
}
