package deploy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/envspec"
	"github.com/codeavers/cvctl/internal/hostcfg"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/objstore"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/registry"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/s3prov"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}

// ComposeAPI abstracts docker compose operations for an instance.
type ComposeAPI interface {
	Config(ctx context.Context) error
	Pull(ctx context.Context, services ...string) error
	UpData(ctx context.Context, services []string, waitTimeout time.Duration) error
	RunHook(ctx context.Context, service, script string) (sys.Result, error)
	ExecScript(ctx context.Context, service, script string) (sys.Result, error)
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
	// Run executes the docker network commands; Wire sets it.
	Run sys.Runner
	// Contracts pulls the contract artifact of OCI-sourced instances.
	Contracts ContractFetcher
	// Allowed lists permitted image registries. Nil disables the post-checkout
	// image check; Wire sets it (non-nil) from the host configuration.
	Allowed []string
	// Secrets feeds Backup in Wire; Backup nil means no backups configured.
	Secrets        *hostcfg.Secrets
	Backup         *render.BackupTarget
	RenderOverride func(p *contract.Project, d *instance.Decisions, cat kinds.Catalog, o render.Options) ([]byte, error)
	RenderTraefik  func(p *contract.Project, d *instance.Decisions, o render.Options) ([]byte, error)
	ProbeS3        func(ctx context.Context, c objstore.Config, b []objstore.BucketCheck) error
}

// traefikContainer is the host proxy container; it joins each instance's edge
// network after the instance is up.
const traefikContainer = "cvctl-traefik"

// ensureNetworks creates the external networks the generated override refers
// to. The backup network exists only when a local service is backed up, the
// same condition the override uses to declare it.
func (d *Deployer) ensureNetworks(ctx context.Context, inst string, p *contract.Project, dec *instance.Decisions) error {
	if d.Run == nil {
		return errNoRunner()
	}
	if err := sys.EnsureNetwork(ctx, d.Run, render.NetworkEdge(inst), false); err != nil {
		return err
	}
	if d.Backup == nil {
		return nil
	}
	for _, ds := range p.Data {
		if dec.Services[ds.Name].Mode == "local" && (ds.Backup == "wal-g" || ds.Backup == "volume") {
			// Internal only with the host silo: external storage (R2, S3) is
			// reached over the internet, so the network needs egress.
			return sys.EnsureNetwork(ctx, d.Run, render.NetworkBackup(inst), d.Backup.Local)
		}
	}
	return nil
}

func errNoRunner() error {
	return &output.Error{
		Code:    "E_DEPLOY_MISCONFIGURED",
		Message: "deployer misconfigured: no command runner",
		Hint:    "deployer has no command runner",
	}
}

func (d *Deployer) connectTraefik(ctx context.Context, inst string) error {
	if d.Run == nil {
		return errNoRunner()
	}
	return sys.ConnectNetwork(ctx, d.Run, render.NetworkEdge(inst), traefikContainer)
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

// WriteRoute re-renders the instance's Traefik route with pin applied and
// writes it atomically (sys.FS.WriteFile is temp file + rename). A nil pin
// restores the alias form; no exposed service removes the file.
func (d *Deployer) WriteRoute(p *contract.Project, dec *instance.Decisions, pin map[string]string) error {
	b, err := d.renderTraefik(p, dec, render.Options{Edge: d.edge(), Pin: pin})
	if err != nil {
		return err
	}
	path := filepath.Join(d.traefikDir(), dec.Instance+".yml")
	if b == nil {
		return d.fs().Remove(path)
	}
	return d.fs().WriteFile(path, b, 0o640, deployOwner)
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
	if entries, err := fsys.ReadDir(stagingDir); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "backup-") && strings.HasSuffix(e.Name(), ".env") {
				_ = fsys.Remove(filepath.Join(stagingDir, e.Name()))
			}
		}
	}
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
		if strings.HasPrefix(k, "CVCTL_BACKUP_") {
			switch k {
			case "CVCTL_BACKUP_EXT_BUCKET", "CVCTL_BACKUP_EXT_ENDPOINT", "CVCTL_BACKUP_EXT_REGION":
			default:
				if len(v) >= 8 {
					out[k] = v
				}
			}
			continue
		}
		if !strings.HasPrefix(k, "CVCTL_SVC_") {
			continue
		}
		// Bucket names and public URLs are not secret, and an E_S3_PROBE naming
		// the bucket is what makes it actionable.
		if strings.Contains(k, "_EXT_BUCKET") || strings.Contains(k, "_EXT_PUBLIC_URL_") {
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

type rollbackKey struct{}

// rollbackInfo carries what a rollback needs from DeployLocked to interrupted.
type rollbackInfo struct {
	env    map[string]string
	absent bool
	cap    time.Duration
}

// rollbackCap bounds a rollback run after the deploy context is gone: the
// health timeout plus 2 minutes, at least 5 minutes.
func rollbackCap(p *contract.Project) time.Duration {
	h := p.X.Health.Timeout
	if h <= 0 {
		h = 30 * time.Second
	}
	return max(h+2*time.Minute, 5*time.Minute)
}

// waitTimeout is the --wait-timeout of up --wait: health timeout plus 2 minutes.
func waitTimeout(p *contract.Project) time.Duration {
	h := p.X.Health.Timeout
	if h <= 0 {
		h = 30 * time.Second
	}
	return h + 2*time.Minute
}

// interrupted returns an instance whose deploy was cancelled (shutdown) to its
// last stable status and records "interrupted". It is not a failure: nothing is
// notified and the same desired state is retried. After promotion the previous
// release is restored first, on a context that outlives the cancellation.
func (d *Deployer) interrupted(ctx context.Context, inst string, st *state.State, want state.Desired, deployID, actor string, now time.Time) (Result, error) {
	if st.Promoted && hasInfra(st.Current) {
		rb, _ := ctx.Value(rollbackKey{}).(*rollbackInfo)
		if rb == nil {
			rb = &rollbackInfo{cap: 5 * time.Minute}
		}
		rbCtx, cancel := withStepTimeout(context.WithoutCancel(ctx), rb.cap, "rollback")
		defer cancel()
		target := *st.Current
		if err := d.swapOrRestore(rbCtx, inst, &target, st.Current, rb.env, rb.absent); err != nil {
			detail := stepErrText(rbCtx, err)
			st.Status = state.StatusFailed
			st.Promoted = false
			st.RollbackFailed = true
			st.FailedHash = ""
			st.UpdatedAt = now
			var errs []error
			if serr := d.Store.Save(st); serr != nil {
				errs = append(errs, serr)
			}
			if aerr := d.Store.Append(inst, state.Event{
				ID: deployID, At: now, Kind: "rollback", From: st.Current, To: &want,
				Result: "rollback_failed", Detail: "shutdown after promotion; rollback failed: " + detail, Actor: actor,
			}); aerr != nil {
				errs = append(errs, aerr)
			}
			return Result{Status: state.StatusFailed, ID: deployID, Detail: "rollback failed: " + detail}, errors.Join(errs...)
		}
		st.Promoted = false
	}
	err := d.Store.Interrupt(st, &want, deployID, actor, "deploy interrupted by shutdown; will retry", now)
	return Result{Status: st.Status, ID: deployID, Detail: "deploy interrupted"}, err
}

func (d *Deployer) failPermanent(ctx context.Context, inst string, st *state.State, want state.Desired, deployID, actor string, now time.Time, errText string, envVals map[string]string) (Result, error) {
	if cancelled(ctx) && st.Status == state.StatusDeploying {
		return d.interrupted(ctx, inst, st, want, deployID, actor, now)
	}
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
	if cancelled(ctx) && st.Status == state.StatusDeploying {
		return d.interrupted(ctx, inst, st, want, deployID, actor, now)
	}
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
		if rErr := d.swapOrRestore(ctx, inst, st.Current, st.Current, existingEnv, appEnvAbsent); rErr != nil {
			restoreMsg = fmt.Sprintf("; restore to %s failed: %v", RevShort(*st.Current), rErr)
		} else {
			restoreMsg = fmt.Sprintf("; restored to %s", RevShort(*st.Current))
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
	ctx, cancelDeploy := withStepTimeout(ctx, d.Host.TimeoutDeploy(), "deploy")
	defer cancelDeploy()
	rb := &rollbackInfo{}
	ctx = context.WithValue(ctx, rollbackKey{}, rb)

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

	// Step 2: Checkout want (commit or contract digest), load contract, spec, decisions, Validate + Check
	promoted := false
	defer func() {
		if !promoted && hasInfra(st.Current) {
			// The restore must also run after a cancel or timeout of ctx.
			rctx, rcancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
			defer rcancel()
			if rErr := d.checkout(rctx, inst, *st.Current); rErr != nil {
				errMsg := fmt.Sprintf("restore checkout failed: %v", rErr)
				if res.Detail != "" {
					res.Detail += "; " + errMsg
				} else {
					res.Detail = errMsg
				}
			}
		}
	}()

	instDir := filepath.Join(d.deployRoot(), inst)
	cvctlEnvPath := filepath.Join(instDir, "cvctl.env")

	cvSecrets := d.cvctlSecrets(cvctlEnvPath)
	if err := d.checkout(ctx, inst, want); err != nil {
		var ice *invalidContractError
		if errors.As(err, &ice) {
			return d.failPermanent(ctx, inst, st, want, deployID, actor, now, ice.Error(), cvSecrets)
		}
		return Result{}, err
	}
	d0, err := instance.Read(cvctlEnvPath, &contract.Project{})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("cannot read %s: %v", cvctlEnvPath, err), cvSecrets)
	}
	if err := instance.CheckDir(d0, cvctlEnvPath); err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), cvSecrets)
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
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, instance.WrapLoadErr(err).Error(), cvSecrets)
	}

	dec, err := instance.Read(cvctlEnvPath, p)
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("cannot read %s: %v", cvctlEnvPath, err), cvSecrets)
	}
	if err := instance.CheckDir(dec, cvctlEnvPath); err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, err.Error(), cvSecrets)
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
	if st.Status != state.StatusDeploying {
		st.PriorStatus = st.Status
	}
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

	secrets := withSecrets(withSecrets(resEnv.Values, cvSecrets), d.backupSecrets())
	backupTarget := render.BackupTargetFor(d.Backup, dec)
	appEnvBytes := instance.FormatEnv(resEnv.Values)
	stagingOverrideBytes, err := d.renderOverride(p, dec, cat, render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  stagingAppEnvPath,

		BackupEnvDir: stagingDir,
		Backup:       backupTarget,
	})
	if err != nil {
		return d.failPermanent(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("render staging override failed: %v", err), secrets)
	}
	finalOverrideBytes, err := d.renderOverride(p, dec, cat, render.Options{
		InstanceDir: instDir,
		Edge:        d.edge(),
		AppEnvPath:  finalAppEnvPath,
		Backup:      backupTarget,
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
	backupEnvs := render.BackupEnvs(p, dec, cat, render.Options{Backup: backupTarget})
	for svc, env := range backupEnvs {
		path := render.BackupEnvPath(stagingDir, svc)
		if err := fsys.WriteFile(path, instance.FormatEnv(env), 0o640, deployOwner); err != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("write staging backup env failed: %v", err), secrets)
		}
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
		if err := d.restore().Create(ctx, inst, deployID); errors.Is(err, backup.ErrFreshInstance) {
			if aerr := d.Store.Append(inst, state.Event{
				ID: deployID, At: now, Kind: "restore_point", Result: "skipped",
				Detail: "no restore point: fresh instance", Actor: actor,
			}); aerr != nil {
				d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
				return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("recording the skipped restore point failed: %v", aerr), secrets)
			}
		} else if err != nil {
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

	if cancelled(ctx) {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.interrupted(ctx, inst, st, want, deployID, actor, now)
	}
	pullCtx, pullCancel := withStepTimeout(ctx, d.Host.TimeoutPull(), "pull")
	pullErr := stagingCompose.Pull(pullCtx, appServices...)
	pullText := ""
	if pullErr != nil {
		pullText = stepErrText(pullCtx, pullErr)
	}
	pullCancel()
	if pullErr != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, pullText, secrets)
	}
	if err := d.ensureNetworks(ctx, inst, p, dec); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, stepErrText(ctx, err), secrets)
	}
	if err := d.provisionBackupUser(ctx, inst, p, dec, backupTarget); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, stepErrText(ctx, err), secrets)
	}
	if err := stagingCompose.UpData(ctx, localDataServices, waitTimeout(p)); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, stepErrText(ctx, err), secrets)
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

	rb.env, rb.absent, rb.cap = rollbackEnv, rollbackEnvAbsent, rollbackCap(p)

	// Step 6b: buckets and the scoped app user of local bucket services. It runs
	// before the hooks because a migration may already write objects.
	if err := d.provisionBuckets(ctx, stagingCompose, inst, p, dec, cat, secrets); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, stepErrText(ctx, err), secrets)
	}
	if err := d.probeExternalBuckets(ctx, inst, p, dec, cat, secrets); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, stepErrText(ctx, err), secrets)
	}

	// Step 7: Pre-deploy hooks
	preHooksRan := false
	for _, hook := range p.X.Hooks.PreDeploy {
		if cancelled(ctx) {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			return d.interrupted(ctx, inst, st, want, deployID, actor, now)
		}
		preHooksRan = true
		hookCtx, hookCancel := withStepTimeout(ctx, d.hookTimeout(hook), "pre-deploy hook "+hook.Service)
		resHook, err := stagingCompose.RunHook(hookCtx, hook.Service, hook.Run)
		timeoutErr := TimeoutError(hookCtx)
		hookCancel()
		if timeoutErr != nil {
			d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
			if TimeoutError(ctx) != nil {
				return d.failTransient(ctx, inst, st, want, deployID, actor, now, timeoutErr.Error(), secrets)
			}
			// The hook's own timeout is permanent for this desired state:
			// retrying it would spend the agent's time on every tick.
			return d.failPermanent(ctx, inst, st, want, deployID, actor, now, timeoutErr.Error(), secrets)
		}
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

	if cancelled(ctx) {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.interrupted(ctx, inst, st, want, deployID, actor, now)
	}

	// From here files and containers may no longer match Current: a crash or
	// shutdown must roll back (or, after a crash, force a redeploy).
	st.Promoted = true
	if err := d.Store.Save(st); err != nil {
		d.cleanupStaging(stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath)
		return d.failTransient(ctx, inst, st, want, deployID, actor, now, fmt.Sprintf("record promotion failed: %v", err), secrets)
	}

	// Step 8: Promote staging -> final (order: app.env -> backup envs -> route -> override)
	if err := fsys.WriteFile(finalAppEnvPath, appEnvBytes, 0o640, deployOwner); err != nil {
		return d.handlePromoteFailure(ctx, inst, st, want, deployID, actor, now, rollbackEnv, secrets, rollbackEnvAbsent, stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath, fmt.Sprintf("promote app.env failed: %v", err))
	}
	for svc, env := range backupEnvs {
		if err := fsys.WriteFile(render.BackupEnvPath(instDir, svc), instance.FormatEnv(env), 0o640, deployOwner); err != nil {
			return d.handlePromoteFailure(ctx, inst, st, want, deployID, actor, now, rollbackEnv, secrets, rollbackEnvAbsent, stagingDir, stagingOverridePath, stagingAppEnvPath, stagingRoutePath, fmt.Sprintf("promote backup env failed: %v", err))
		}
	}
	resticKeep := map[string]bool{}
	for _, t := range backup.Targets(p, dec) {
		if t.Strategy == backup.StrategyVolume {
			resticKeep[t.Service] = true
		}
	}
	d.removeStaleBackupEnvs(instDir, backupEnvs, resticKeep)
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
	if upErr == nil {
		// Traefik being unreachable says nothing about the new release: retry, do not roll back.
		if err := d.connectTraefik(ctx, inst); err != nil {
			return d.failTransient(ctx, inst, st, want, deployID, actor, now, stepErrText(ctx, err), secrets)
		}
	}

	// Step 9: HealthCheck
	var healthErr error
	if upErr == nil {
		healthErr = d.Health(ctx, inst, finalOverridePath)
	}

	if upErr == nil && healthErr == nil {
		// Post-deploy hooks
		for _, hook := range p.X.Hooks.PostDeploy {
			hookCtx, hookCancel := withStepTimeout(ctx, d.hookTimeout(hook), "post-deploy hook "+hook.Service)
			hRes, hErr := finalCompose.RunHook(hookCtx, hook.Service, hook.Run)
			if te := TimeoutError(hookCtx); te != nil {
				hErr = te
			}
			hookCancel()
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

		okDetail := fmt.Sprintf("%s %s deployed", revWord(want), RevShort(want))
		_ = d.notifier().Send(ctx, notify.Message{
			Host:  d.hostName(),
			Level: "info",
			Title: fmt.Sprintf("%s deploy OK", inst),
			Text:  fmt.Sprintf("%s %s deployed successfully", revWord(want), RevShort(want)),
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
		} else {
			d.pruneContracts(inst, st)
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
	if cancelled(ctx) {
		return d.interrupted(ctx, inst, st, want, deployID, actor, now)
	}
	// After a deploy timeout ctx is dead, but the rollback still has to run.
	rbCtx := ctx
	timeoutPrefix := ""
	if te := TimeoutError(ctx); te != nil {
		timeoutPrefix = te.Error() + "\n"
		var rbCancel context.CancelFunc
		rbCtx, rbCancel = withStepTimeout(context.WithoutCancel(ctx), rb.cap, "rollback")
		defer rbCancel()
	}
	var logs string
	if finalCompose != nil {
		logs, _ = finalCompose.Logs(rbCtx, "", 20)
	}
	if strings.TrimSpace(logs) == "" {
		if upErr != nil {
			logs = upErr.Error()
		} else if healthErr != nil {
			logs = healthErr.Error()
		}
	}
	redactedLogs := lastLines(redact(timeoutPrefix+logs, secrets), 20)

	if !hasInfra(st.Current) {
		st.Status = state.StatusFailed
		st.FailedHash = want.Hash()
		if timeoutPrefix != "" {
			// A timeout is transient: retry instead of waiting for a new desired state.
			st.FailedHash = ""
		}
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
	rollbackErr := d.swapOrRestore(rbCtx, inst, &rollbackTarget, st.Current, rollbackEnv, false)

	st.Status = state.StatusFailed
	st.FailedHash = want.Hash()
	if timeoutPrefix != "" {
		st.FailedHash = ""
	}
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

// provisionBackupUser creates the instance's silo user and attaches the silo
// to the instance's backup network, when the instance backs up a local
// service: the same condition under which ensureNetworks creates that network.
func (d *Deployer) provisionBackupUser(ctx context.Context, inst string, p *contract.Project, dec *instance.Decisions, t *render.BackupTarget) error {
	if t == nil || len(backup.Targets(p, dec)) == 0 {
		return nil
	}
	return backup.ProvisionUser(ctx, d.Run, t, inst)
}

// backupSecrets lists the backup credential values so error text redacts them.
func (d *Deployer) backupSecrets() map[string]string {
	if d.Backup == nil {
		return nil
	}
	out := map[string]string{}
	for k, v := range map[string]string{
		"backup_secret_key": d.Backup.SecretKey, "backup_libsodium_key": d.Backup.LibsodiumKey,
		"backup_restic_password": d.Backup.ResticPassword, "backup_access_key": d.Backup.AccessKey,
	} {
		if len(v) >= 8 {
			out[k] = v
		}
	}
	return out
}

// removeStaleBackupEnvs deletes backup-<svc>.env files of services that are no
// longer wired for wal-g and restic-<svc>.env files of services that are no
// longer restic volume targets, so old credentials do not linger on disk.
// restic-env.env belongs to the host-level env backup and stays.
func (d *Deployer) removeStaleBackupEnvs(instDir string, keep map[string]map[string]string, resticKeep map[string]bool) {
	fsys := d.fs()
	entries, err := fsys.ReadDir(instDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".env") {
			continue
		}
		switch {
		case strings.HasPrefix(n, "backup-"):
			if _, ok := keep[strings.TrimSuffix(strings.TrimPrefix(n, "backup-"), ".env")]; !ok {
				_ = fsys.Remove(filepath.Join(instDir, n))
			}
		case strings.HasPrefix(n, "restic-") && n != "restic-env.env":
			if !resticKeep[strings.TrimSuffix(strings.TrimPrefix(n, "restic-"), ".env")] {
				_ = fsys.Remove(filepath.Join(instDir, n))
			}
		}
	}
}

// provisionBuckets runs the s3prov script in every local data service whose
// kind supports buckets and records the buckets it finds but the contract no
// longer declares in <state>/<inst>/s3-orphans.json.
func (d *Deployer) provisionBuckets(ctx context.Context, c ComposeAPI, inst string, p *contract.Project, dec *instance.Decisions, cat kinds.Catalog, secrets map[string]string) error {
	orphans := map[string][]string{}
	for _, ds := range p.Data {
		sd := dec.Services[ds.Name]
		if sd.Mode != "local" || !cat[ds.Kind].Buckets {
			continue
		}
		buckets := p.Buckets(ds.Name)
		if len(buckets) == 0 {
			buckets = []contract.Bucket{{Name: sd.Creds.DB}}
		}
		script := s3prov.Script(buckets, sd.Creds.AppUser, sd.Creds.AppPassword, s3prov.PolicyName)
		res, err := c.ExecScript(ctx, ds.Name, script)
		if err != nil {
			detail := strings.TrimSpace(string(res.Stderr))
			if detail == "" {
				detail = err.Error()
			}
			redacted := lastLines(redact(detail, secrets), 20)
			return &output.Error{
				Code:    "E_S3_PROVISION",
				Message: ds.Name + ": " + redacted,
				Hint:    "check the " + ds.Name + " container: cvctl logs " + inst,
			}
		}
		if o := s3prov.Orphans(res.Stdout, buckets); len(o) > 0 {
			orphans[ds.Name] = o
		}
	}
	path := filepath.Join(d.stateRoot(), inst, "s3-orphans.json")
	if len(orphans) == 0 {
		if err := d.fs().Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return orphansErr(path, err)
		}
		return nil
	}
	data, err := json.Marshal(orphans)
	if err == nil {
		err = d.fs().WriteFile(path, append(data, '\n'), 0o640, deployOwner)
	}
	if err != nil {
		return orphansErr(path, err)
	}
	return nil
}

func orphansErr(path string, err error) error {
	return &output.Error{
		Code:    "E_S3_PROVISION",
		Message: fmt.Sprintf("recording orphan buckets in %s: %v", path, err),
		Hint:    "check the permissions of " + filepath.Dir(path),
	}
}

func (d *Deployer) probeS3(ctx context.Context, c objstore.Config, b []objstore.BucketCheck) error {
	if d.ProbeS3 != nil {
		return d.ProbeS3(ctx, c, b)
	}
	hc := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	return objstore.ProbeBuckets(ctx, c, b, hc)
}

func (d *Deployer) probeExternalBuckets(ctx context.Context, inst string, p *contract.Project, dec *instance.Decisions, cat kinds.Catalog, secrets map[string]string) error {
	for _, ds := range p.Data {
		sd := dec.Services[ds.Name]
		if sd.Mode != "external" || !cat[ds.Kind].Buckets {
			continue
		}
		cfg := objstore.Config{
			Endpoint:  sd.External["endpoint"],
			Region:    sd.External["region"],
			AccessKey: sd.External["access_key"],
			SecretKey: sd.External["secret_key"],
		}
		var checks []objstore.BucketCheck
		buckets := p.Buckets(ds.Name)
		if len(buckets) == 0 {
			checks = append(checks, objstore.BucketCheck{
				Name: sd.External["bucket"],
			})
		} else {
			for _, b := range buckets {
				low := strings.ToLower(b.Name)
				check := objstore.BucketCheck{
					Name: sd.External["bucket_"+low],
				}
				if b.Public {
					check.PublicURL = sd.External["public_url_"+low]
				}
				checks = append(checks, check)
			}
		}
		if err := d.probeS3(ctx, cfg, checks); err != nil {
			return err
		}
	}
	return nil
}

// hookTimeout is the hook's own timeout clamped to the host's
// CVCTL_TIMEOUT_HOOK: a contract cannot buy more agent time than the host
// allows.
func (d *Deployer) hookTimeout(h contract.Hook) time.Duration {
	host := d.Host.TimeoutHook()
	if h.Timeout != "" {
		if t, err := time.ParseDuration(h.Timeout); err == nil && t > 0 && t < host {
			return t
		}
	}
	return host
}
