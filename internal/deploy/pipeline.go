package deploy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
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
	"github.com/codeavers/cvctl/internal/registry"
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
	DeployRoot string
	StateRoot  string
	TraefikDir string
	FS         sys.FS
	Store      *state.Store
	Compose    func(inst, override, envFile string) ComposeAPI
	Repo       func(inst string) CheckoutAPI
	Notify     notify.Notifier
	Restore    RestorePointer
	Host       *hostcfg.Config
	Catalog    kinds.Catalog
	Clock      func() time.Time
	Health     func(ctx context.Context, inst, override string) error
	// Allowed lists permitted image registries. Nil disables the post-checkout
	// image check; Wire sets it (non-nil) from the host configuration.
	Allowed        []string
	RenderOverride func(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o render.Options) ([]byte, error)
	RenderTraefik  func(p *contract.Project, d *instance.Decisions, o render.Options) ([]byte, error)
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

func (d *Deployer) stateRoot() string {
	if d.StateRoot != "" {
		return d.StateRoot
	}
	if d.Store != nil && d.Store.Root() != "" {
		return d.Store.Root()
	}
	return "/var/lib/cvctl"
}

func (d *Deployer) repo(inst string) CheckoutAPI {
	if d.Repo != nil {
		return d.Repo(inst)
	}
	return nil
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

func (d *Deployer) renderOverride(p *contract.Project, dec *instance.Decisions, cat kinds.Catalog, o render.Options) ([]byte, error) {
	if d.RenderOverride != nil {
		return d.RenderOverride(p, dec, cat, o)
	}
	return render.Override(p, dec, cat, o)
}

func (d *Deployer) renderTraefik(p *contract.Project, dec *instance.Decisions, o render.Options) ([]byte, error) {
	if d.RenderTraefik != nil {
		return d.RenderTraefik(p, dec, o)
	}
	return render.Traefik(p, dec, o)
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

func (d *Deployer) readAppEnv(appEnvPath string) (map[string]string, bool, error) {
	data, err := d.fs().ReadFile(appEnvPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]string{}, true, nil
		}
		return nil, false, &output.Error{
			Code:    "E_APPENV_READ",
			Message: fmt.Sprintf("cannot read %s: %v", appEnvPath, err),
			Hint:    "check file permissions on app.env",
		}
	}
	vals, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, false, &output.Error{
			Code:    "E_APPENV_READ",
			Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", appEnvPath),
			Hint:    "fix syntax in app.env",
		}
	}
	return vals, false, nil
}

func (d *Deployer) cleanupStaging(stagingDir, override, appEnv, route string) {
	fsys := d.fs()
	_ = fsys.Remove(override)
	_ = fsys.Remove(appEnv)
	_ = fsys.Remove(route)
	_ = fsys.Remove(stagingDir)
}

func redact(text string, envValues map[string]string) string {
	if len(envValues) == 0 || text == "" {
		return text
	}
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

// withSecrets returns a new map holding base plus extra, for redaction only.
func withSecrets(base, extra map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range extra {
		out[k] = v
	}
	for k, v := range base {
		out[k] = v
	}
	return out
}

// cvctlSecrets collects the secret values stored in cvctl.env (service
// passwords and external-service settings) so they are redacted like app.env
// values. Unreadable or malformed files yield no secrets: callers report that
// failure separately.
func (d *Deployer) cvctlSecrets(path string) map[string]string {
	data, err := d.fs().ReadFile(path)
	if err != nil {
		return nil
	}
	m, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range m {
		if !strings.HasPrefix(k, "CVCTL_SVC_") {
			continue
		}
		if strings.HasSuffix(k, "_PASSWORD") || strings.Contains(k, "_EXT_") {
			if len(v) >= 8 {
				out[k] = v
			}
		}
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

func (d *Deployer) failPermanent(ctx context.Context, inst string, st *state.State, want state.Desired, deployID, actor string, now time.Time, errText string, envVals map[string]string) (Result, error) {
	redacted := redact(errText, envVals)
	st.Status = state.StatusFailed
	st.FailedHash = want.Hash()
	st.UpdatedAt = now
	res := Result{
		Status: state.StatusFailed,
		ID:     deployID,
		Detail: redacted,
	}
	_ = d.notifier().Send(ctx, notify.Message{
		Host:  d.hostName(),
		Level: "error",
		Title: fmt.Sprintf("%s deploy FAILED", inst),
		Text:  redacted,
	})
	var errs []error
	if err := d.Store.Save(st); err != nil {
		errs = append(errs, err)
	}
	if err := d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "failed",
		From:   st.Current,
		To:     &want,
		Result: string(state.StatusFailed),
		Detail: redacted,
		Actor:  actor,
	}); err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

func (d *Deployer) failTransient(ctx context.Context, inst string, st *state.State, want state.Desired, deployID, actor string, now time.Time, errText string, envVals map[string]string) (Result, error) {
	redacted := redact(errText, envVals)
	detail := strings.TrimRight(redacted, ".")
	if !strings.HasSuffix(detail, "will retry next cycle") {
		detail = detail + ": will retry next cycle"
	}
	// Transient failures repeat every poll cycle; only a changed cause is worth a message.
	repeated := st.LastTransient == detail
	st.Status = state.StatusFailed
	st.FailedHash = ""
	st.LastTransient = detail
	st.UpdatedAt = now
	res := Result{
		Status: state.StatusFailed,
		ID:     deployID,
		Detail: detail,
	}
	if !repeated {
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "error",
			Title: fmt.Sprintf("%s deploy FAILED", inst),
			Text:  detail,
		})
	}
	var errs []error
	if err := d.Store.Save(st); err != nil {
		errs = append(errs, err)
	}
	if err := d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "failed",
		From:   st.Current,
		To:     &want,
		Result: string(state.StatusFailed),
		Detail: detail,
		Actor:  actor,
	}); err != nil {
		errs = append(errs, err)
	}
	return res, errors.Join(errs...)
}

func (d *Deployer) handlePromoteFailure(ctx context.Context, inst string, st *state.State, want state.Desired, deployID, actor string, now time.Time, existingEnv, resEnvVals map[string]string, appEnvAbsent bool, stagingDir, stagingOverride, stagingAppEnv, stagingRoute, failedMsg string) (Result, error) {
	d.cleanupStaging(stagingDir, stagingOverride, stagingAppEnv, stagingRoute)
	restoreMsg := ""
	if st.Current != nil {
		if rErr := d.executeSwap(ctx, inst, st.Current, existingEnv, appEnvAbsent); rErr != nil {
			restoreMsg = fmt.Sprintf("; restore to %s failed: %v", commitShort(st.Current.InfraCommit), rErr)
		} else {
			restoreMsg = fmt.Sprintf("; restored to %s", commitShort(st.Current.InfraCommit))
		}
	}
	return d.failPermanent(ctx, inst, st, want, deployID, actor, now, failedMsg+restoreMsg, resEnvVals)
}

// Deploy executes the deployment pipeline for an instance following the recreate strategy.
func (d *Deployer) Deploy(ctx context.Context, inst string, want state.Desired, opts Options) (Result, error) {
	if d.Compose == nil || d.Health == nil {
		return Result{}, misconfiguredErr()
	}
	unlock, err := d.Store.Lock(inst)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	return d.DeployLocked(ctx, inst, want, opts)
}

func misconfiguredErr() error {
	return &output.Error{
		Code:    "E_DEPLOY_MISCONFIGURED",
		Message: "deployer misconfigured: Compose and Health are required",
		Hint:    "configure Compose and Health before running deploy",
	}
}

// DeployLocked runs the pipeline assuming the caller already holds the
// instance lock (Store.Lock).
func (d *Deployer) DeployLocked(ctx context.Context, inst string, want state.Desired, opts Options) (res Result, retErr error) {
	actor := opts.Actor
	if actor == "" {
		actor = "agent"
	}
	now := d.clock()
	deployID := fmt.Sprintf("deploy-%d", now.UnixNano())

	if d.Compose == nil || d.Health == nil {
		return Result{}, misconfiguredErr()
	}

	// Step 1: load state (the instance lock is held by the caller)
	st, err := d.Store.Load(inst)
	if err != nil {
		return Result{}, err
	}

	pausedAutoResume := false
	if st.Status == state.StatusPaused && !opts.Force {
		if st.PausedWantHash == "" || want.Hash() == st.PausedWantHash || (st.FailedHash != "" && want.Hash() == st.FailedHash) {
			return Result{
				Status: state.StatusPaused,
				ID:     deployID,
				Detail: st.PausedReason,
			}, nil
		}
		pausedAutoResume = true
	}
	if st.FailedHash != "" && want.Hash() == st.FailedHash && !opts.Force {
		return Result{
			Status: state.StatusFailed,
			ID:     deployID,
			Detail: "desired state previously failed",
		}, nil
	}
	// A pending rollback failure means files, checkout and containers may not match
	// Current, so equality of hashes proves nothing: run the full pipeline.
	if st.Current != nil && want.Hash() == st.Current.Hash() && !opts.Force && !st.RollbackFailed {
		if st.Status == state.StatusBlocked || st.Status == state.StatusFailed {
			// The running state already matches; a stale BLOCKED/FAILED status
			// (e.g. after the cause was fixed by hand) must not stick forever.
			st.Status = state.StatusOK
			st.Blocked = nil
			st.FailedHash = ""
			st.LastTransient = ""
			st.UpdatedAt = now
			var errs []error
			if err := d.Store.Save(st); err != nil {
				errs = append(errs, err)
			}
			if err := d.Store.Append(inst, state.Event{
				ID:     deployID,
				At:     now,
				Kind:   "recovered",
				From:   st.Current,
				To:     &want,
				Result: string(state.StatusOK),
				Detail: "desired state already running",
				Actor:  actor,
			}); err != nil {
				errs = append(errs, err)
			}
			return Result{Status: state.StatusOK, ID: deployID, Detail: "already at desired state"}, errors.Join(errs...)
		}
		return Result{
			Status: state.StatusOK,
			ID:     deployID,
			Detail: "already at desired state",
		}, nil
	}

	if pausedAutoResume {
		// Auto-resume on new desired state
		st.Status = state.StatusOK
		st.PausedReason = ""
		st.PausedWantHash = ""
		st.UpdatedAt = now
		if err := d.Store.Save(st); err != nil {
			return Result{}, err
		}
		if err := d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "resume",
			From:   st.Current,
			To:     &want,
			Result: string(state.StatusOK),
			Detail: "auto-resumed by new desired state",
			Actor:  actor,
		}); err != nil {
			return Result{}, err
		}
	}

	// Step 2: Checkout want.InfraCommit, load contract, spec, decisions, Validate + Check
	promoted := false
	defer func() {
		if !promoted && st.Current != nil && st.Current.InfraCommit != "" {
			if r := d.repo(inst); r != nil {
				if rErr := r.Checkout(ctx, st.Current.InfraCommit); rErr != nil {
					errMsg := fmt.Sprintf("restore checkout failed: %v", rErr)
					if res.Detail != "" {
						res.Detail += "; " + errMsg
					} else {
						res.Detail = errMsg
					}
				}
			}
		}
	}()

	if r := d.repo(inst); r != nil {
		if err := r.Checkout(ctx, want.InfraCommit); err != nil {
			return Result{}, err
		}
	}

	instDir := filepath.Join(d.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")

	cvSecrets := d.cvctlSecrets(cvctlEnvPath)
	d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("cannot read %s: %v", cvctlEnvPath, err), cvSecrets)
	}
	infraPath := d0.InfraPath
	if infraPath == "" {
		infraPath = "."
	}
	infraDir := filepath.Join(instDir, "infra", infraPath)

	cat, err := d.catalog()
	if err != nil {
		return Result{}, err
	}

	p, err := contract.Load(infraDir, cat)
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), cvSecrets)
	}

	dec, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("cannot read %s: %v", cvctlEnvPath, err), cvSecrets)
	}
	if dec.InfraPath == "" {
		dec.InfraPath = "."
	}

	if d.Allowed != nil {
		for _, a := range p.Apps {
			if aErr := registry.Allowed(a.Image, d.Allowed); aErr != nil {
				return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("E_REGISTRY_NOT_ALLOWED: %v", aErr), cvSecrets)
			}
		}
	}

	specPath := filepath.Join(infraDir, "env.spec.yml")
	spec, err := envspec.Load(specPath)
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), cvSecrets)
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
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, detail, cvSecrets)
	}

	// Step 3: ServiceVars -> Resolve with current app.env
	finalAppEnvPath := filepath.Join(instDir, "app.env")
	existingEnv, appEnvAbsent, err := d.readAppEnv(finalAppEnvPath)
	if err != nil {
		return Result{}, err
	}

	svcVars, err := render.ServiceVars(p, dec, cat)
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), withSecrets(existingEnv, cvSecrets))
	}

	resEnv, err := envspec.Resolve(spec, envspec.Input{
		Env:         dec.Env,
		Existing:    existingEnv,
		ServiceVars: svcVars,
		Domains:     dec.Domains,
	})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), withSecrets(existingEnv, cvSecrets))
	}

	if len(resEnv.Missing) > 0 {
		st.Status = state.StatusBlocked
		st.Blocked = resEnv.Missing
		st.UpdatedAt = now

		var setEnvCmds []string
		for _, k := range resEnv.Missing {
			setEnvCmds = append(setEnvCmds, fmt.Sprintf("cvctl set-env %s %s", inst, k))
		}
		warnText := strings.Join(setEnvCmds, "\n")
		res := Result{
			Status:  state.StatusBlocked,
			ID:      deployID,
			Missing: resEnv.Missing,
			Detail:  warnText,
		}
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "warn",
			Title: fmt.Sprintf("%s deploy BLOCKED", inst),
			Text:  warnText,
		})

		var errs []error
		if err := d.Store.Save(st); err != nil {
			errs = append(errs, err)
		}
		if err := d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "blocked",
			From:   st.Current,
			To:     &want,
			Result: string(state.StatusBlocked),
			Detail: warnText,
			Actor:  actor,
		}); err != nil {
			errs = append(errs, err)
		}

		return res, errors.Join(errs...)
	}

	st.Blocked = nil
	st.Status = state.StatusDeploying
	st.UpdatedAt = now
	if err := d.Store.Save(st); err != nil {
		return Result{}, err
	}

	// Step 4: Render into staging directory <dir>/.staging/
	stagingDir := filepath.Join(instDir, ".staging")
	stagingOverridePath := filepath.Join(stagingDir, "compose.cvctl.yml")
	stagingAppEnvPath := filepath.Join(stagingDir, "app.env")
	stagingRoutePath := filepath.Join(stagingDir, inst+".yml")
	finalOverridePath := filepath.Join(instDir, "compose.cvctl.yml")
	finalRoutePath := filepath.Join(d.traefikDir(), inst+".yml")

	secrets := withSecrets(resEnv.Values, cvSecrets)
	appEnvBytes := instance.FormatEnv(resEnv.Values)
	stagingOverrideBytes, err := d.renderOverride(p, dec, cat, render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  stagingAppEnvPath,
	})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("render staging override failed: %v", err), secrets)
	}
	finalOverrideBytes, err := d.renderOverride(p, dec, cat, render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  finalAppEnvPath,
	})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("render final override failed: %v", err), secrets)
	}
	routeBytes, err := d.renderTraefik(p, dec, render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  finalAppEnvPath,
	})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("render traefik route failed: %v", err), secrets)
	}

	fsys := d.fs()
	if err := fsys.MkdirAll(stagingDir, 0o2775, deployOwner); err != nil {
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("mkdir staging failed: %v", err), secrets)
	}
	if err := fsys.WriteFile(stagingAppEnvPath, appEnvBytes, 0o640, deployOwner); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("write staging app.env failed: %v", err), secrets)
	}
	if err := fsys.WriteFile(stagingOverridePath, stagingOverrideBytes, 0o640, deployOwner); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("write staging override failed: %v", err), secrets)
	}
	if routeBytes != nil {
		if err := fsys.WriteFile(stagingRoutePath, routeBytes, 0o640, deployOwner); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("write staging route failed: %v", err), secrets)
		}
	}

	stagingCompose := d.Compose(inst, stagingOverridePath, stagingAppEnvPath)
	if stagingCompose == nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, "deployer misconfigured: Compose returned nil", secrets)
	}

	if err := stagingCompose.Config(ctx); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), secrets)
	}

	// Step 5: If prod, create restore point
	if dec.Env == "prod" {
		if err := d.restore().Create(ctx, inst, deployID); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("restore point failed: %v", err), secrets)
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

	if err := stagingCompose.Pull(ctx, appServices...); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, err.Error(), secrets)
	}
	if err := stagingCompose.UpData(ctx, localDataServices); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, err.Error(), secrets)
	}

	// Generated randoms are persisted before hooks run: a migration may already
	// have consumed them, so a later retry or rollback must reuse the same values.
	rollbackEnv := existingEnv
	rollbackEnvAbsent := appEnvAbsent
	if len(resEnv.Generated) > 0 {
		rollbackEnv = make(map[string]string, len(existingEnv)+len(resEnv.Generated))
		for k, v := range existingEnv {
			rollbackEnv[k] = v
		}
		for _, k := range resEnv.Generated {
			rollbackEnv[k] = resEnv.Values[k]
		}
		rollbackEnvAbsent = false
		if err := fsys.WriteFile(finalAppEnvPath, instance.FormatEnv(rollbackEnv), 0o640, deployOwner); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("persist generated app.env keys failed: %v", err), secrets)
		}
	}

	// Step 7: Pre-deploy hooks
	preHooksRan := false
	for _, hook := range p.X.Hooks.PreDeploy {
		preHooksRan = true
		resHook, err := stagingCompose.RunHook(ctx, hook.Service, hook.Run)
		if err != nil || resHook.ExitCode != 0 {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)

			rawOutput := string(resHook.Stderr)
			if strings.TrimSpace(rawOutput) == "" {
				rawOutput = string(resHook.Stdout)
			}
			if strings.TrimSpace(rawOutput) == "" && err != nil {
				rawOutput = err.Error()
			}
			redactedOut := lastLines(redact(rawOutput, secrets), 20)
			return d.failPermanent(ctx, inst, st, want, deployID, actor, now, redactedOut, secrets)
		}
	}

	// Step 8: Promote staging -> final (order: app.env -> route -> override)
	if err := fsys.WriteFile(finalAppEnvPath, appEnvBytes, 0o640, deployOwner); err != nil {
		return d.handlePromoteFailure(ctx, inst, st, want, deployID, actor, now, rollbackEnv, secrets, rollbackEnvAbsent, stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath, fmt.Sprintf("promote app.env failed: %v", err))
	}
	if routeBytes != nil {
		if err := fsys.WriteFile(finalRoutePath, routeBytes, 0o640, deployOwner); err != nil {
			return d.handlePromoteFailure(ctx, inst, st, want, deployID, actor, now, rollbackEnv, secrets, rollbackEnvAbsent, stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath, fmt.Sprintf("promote route failed: %v", err))
		}
	} else {
		_ = fsys.Remove(finalRoutePath)
	}
	if err := fsys.WriteFile(finalOverridePath, finalOverrideBytes, 0o640, deployOwner); err != nil {
		return d.handlePromoteFailure(ctx, inst, st, want, deployID, actor, now, rollbackEnv, secrets, rollbackEnvAbsent, stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath, fmt.Sprintf("promote override failed: %v", err))
	}
	d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
	promoted = true

	finalCompose := d.Compose(inst, finalOverridePath, finalAppEnvPath)
	if finalCompose == nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, "deployer misconfigured: Compose returned nil", secrets)
	}
	upErr := finalCompose.Up(ctx)

	// Step 9: HealthCheck
	var healthErr error
	if upErr == nil {
		healthErr = d.Health(ctx, inst, finalOverridePath)
	}

	if upErr == nil && healthErr == nil {
		// Post-deploy hooks
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
				redactedHook := lastLines(redact(rawHook, secrets), 20)
				_ = d.notifier().Send(ctx, notify.Message{
					Host:  d.hostName(),
					Level: "warn",
					Title: fmt.Sprintf("%s deploy OK", inst),
					Text:  fmt.Sprintf("post-deploy hook failed: %s", redactedHook),
				})
			}
		}

		okDetail := fmt.Sprintf("commit %s deployed", commitShort(want.InfraCommit))
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "info",
			Title: fmt.Sprintf("%s deploy OK", inst),
			Text:  fmt.Sprintf("commit %s deployed successfully", commitShort(want.InfraCommit)),
		})

		st.Previous = st.Current
		st.Current = &want
		st.Status = state.StatusOK
		st.FailedHash = ""
		st.LastTransient = ""
		st.RollbackFailed = false
		st.Blocked = nil
		st.UpdatedAt = now

		var okErrs []error
		if err := d.Store.Save(st); err != nil {
			okErrs = append(okErrs, err)
		}
		if err := d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "deploy",
			From:   st.Previous,
			To:     st.Current,
			Result: string(state.StatusOK),
			Detail: okDetail,
			Actor:  actor,
		}); err != nil {
			okErrs = append(okErrs, err)
		}

		return Result{
			Status: state.StatusOK,
			ID:     deployID,
			Detail: okDetail,
		}, errors.Join(okErrs...)
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
	redactedLogs := lastLines(redact(logs, secrets), 20)

	if st.Current == nil || st.Current.InfraCommit == "" {
		st.Status = state.StatusFailed
		st.FailedHash = want.Hash()
		st.RollbackFailed = false
		st.UpdatedAt = now
		if err := d.Store.Save(st); err != nil {
			return Result{}, err
		}

		if err := d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "failed",
			From:   nil,
			To:     &want,
			Result: string(state.StatusFailed),
			Detail: redactedLogs,
			Actor:  actor,
		}); err != nil {
			return Result{}, err
		}
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
	rollbackErr := d.executeSwap(ctx, inst, &rollbackTarget, rollbackEnv, false)

	st.Status = state.StatusFailed
	st.FailedHash = want.Hash()
	st.UpdatedAt = now

	if rollbackErr != nil {
		st.RollbackFailed = true
		rbDetail := fmt.Sprintf("rollback failed: %v\noriginal error: %s", rollbackErr, redactedLogs)
		rbRedacted := redact(rbDetail, secrets)
		res := Result{
			Status: state.StatusFailed,
			ID:     deployID,
			Detail: rbRedacted,
		}
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "error",
			Title: fmt.Sprintf("%s ROLLBACK FAILED", inst),
			Text:  rbRedacted,
		})

		var errs []error
		if err := d.Store.Save(st); err != nil {
			errs = append(errs, err)
		}
		if err := d.Store.Append(inst, state.Event{
			ID:     deployID,
			At:     now,
			Kind:   "rollback",
			From:   st.Current,
			To:     &want,
			Result: "rollback_failed",
			Detail: rbRedacted,
			Actor:  actor,
		}); err != nil {
			errs = append(errs, err)
		}

		return res, errors.Join(errs...)
	}

	st.RollbackFailed = false

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

	res = Result{
		Status: state.StatusFailed,
		ID:     deployID,
		Detail: errText,
	}

	_ = d.notifier().Send(ctx, notify.Message{
		Host:  d.hostName(),
		Level: "error",
		Title: fmt.Sprintf("%s deploy FAILED", inst),
		Text:  errText,
	})

	var errs []error
	if err := d.Store.Save(st); err != nil {
		errs = append(errs, err)
	}
	if err := d.Store.Append(inst, state.Event{
		ID:     deployID,
		At:     now,
		Kind:   "rollback",
		From:   st.Current,
		To:     &want,
		Result: string(state.StatusFailed),
		Detail: errText,
		Actor:  actor,
	}); err != nil {
		errs = append(errs, err)
	}

	return res, errors.Join(errs...)
}
