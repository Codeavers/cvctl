// Package backup runs and records WAL-G and restic backups of local data
// services, and creates postgres restore points.
package backup

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/compose"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

const (
	StrategyWalG   = "wal-g"
	StrategyVolume = "volume"

	// pgData is the image default; the render step keeps PGDATA at the volume root.
	pgData         = "/var/lib/postgresql/data"
	resticImage    = "restic/restic:0.19.1"
	dockerConfig   = "DOCKER_CONFIG=/var/lib/cvctl/docker"
	walgRetain     = "7"
	walgRetainDays = 7
	backupTimeout  = 3 * time.Hour
	verifyTimeout  = 2 * time.Hour
	maxErrBytes    = 2048
)

var deployOwner = sys.Owner{User: "deployer", Group: "deploy"}

// Target is one local data service that has a backup strategy.
type Target struct {
	Service  string `json:"service"`
	Strategy string `json:"strategy"` // wal-g | volume
	Volume   string `json:"volume,omitempty"`
}

// Targets lists the local data services of an instance with backup != none.
func Targets(p *contract.Project, d *instance.Decisions) []Target {
	var out []Target
	for _, ds := range p.Data {
		if d.Services[ds.Name].Mode != "local" {
			continue
		}
		if ds.Backup != StrategyWalG && ds.Backup != StrategyVolume {
			continue
		}
		out = append(out, Target{Service: ds.Name, Strategy: ds.Backup, Volume: render.VolumeName(d, ds.Name)})
	}
	return out
}

// Loader reads the contract and decisions of an instance.
type Loader func(inst string) (*contract.Project, *instance.Decisions, error)

// LoadInstance reads the checked-out infra and cvctl.env under deployRoot.
func LoadInstance(deployRoot string, cat kinds.Catalog) Loader {
	return func(inst string) (*contract.Project, *instance.Decisions, error) {
		envPath := filepath.Join(deployRoot, inst, "cvctl.env")
		d0, err := instance.Read(envPath, &contract.Project{})
		if err != nil {
			return nil, nil, err
		}
		if err := instance.CheckDir(d0, envPath); err != nil {
			return nil, nil, err
		}
		infra := d0.InfraPath
		if infra == "" {
			infra = "."
		}
		c := cat
		if c == nil {
			if c, err = kinds.Load(); err != nil {
				return nil, nil, err
			}
		}
		p, err := contract.Load(filepath.Join(deployRoot, inst, "infra", infra), c)
		if err != nil {
			return nil, nil, err
		}
		d, err := instance.Read(envPath, p)
		if err != nil {
			return nil, nil, err
		}
		if err := instance.CheckDir(d, envPath); err != nil {
			return nil, nil, err
		}
		return p, d, nil
	}
}

// Result is the outcome of backing up one target.
type Result struct {
	Instance string `json:"instance"`
	Service  string `json:"service"`
	Strategy string `json:"strategy"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
}

// Snapshot is one backup as listed by the backup tool.
type Snapshot struct {
	Service  string `json:"service"`
	Strategy string `json:"strategy"`
	ID       string `json:"id"`
	Time     string `json:"time"`
}

type Runner struct {
	Run     sys.Runner
	FS      sys.FS
	Compose func(inst string) *compose.Project
	Load    Loader
	Host    *render.BackupTarget
	Store   *state.Store
	Notify  notify.Notifier
	Clock   func() time.Time
	// Suffix makes temp container and volume names unique within a second;
	// nil means 4 random bytes in hex.
	Suffix     func() string
	DeployRoot string
	HostName   string
	Catalog    kinds.Catalog
	// Apply re-renders and starts target with the current decisions and runs
	// the health check; restore uses it to switch a volume and to roll back.
	// ArchiveTimeout bounds the wait for the last WAL segment to be archived
	// before a restore; zero means 120s.
	ArchiveTimeout time.Duration
	Apply          func(ctx context.Context, inst string, target *state.Desired) error
	// Lock retry: deploys hold the instance lock for minutes, so a status
	// write waits for them instead of losing the record.
	LockInterval time.Duration
	LockTimeout  time.Duration
	Sleep        func(time.Duration)
}

const (
	defaultLockInterval = 15 * time.Second
	defaultLockTimeout  = 30 * time.Minute

	dataLockInterval = 30 * time.Second
	dataLockWait     = 3 * time.Hour
)

// lockData takes the host-wide data lock. Backups and verifies wait for it
// (a restore or the other timer may be running); restores pass wait=false
// and fail at once, since an operator is waiting on them.
func (r *Runner) lockData(ctx context.Context, wait bool) (func(), error) {
	if r.Store == nil {
		return func() {}, nil
	}
	var waited time.Duration
	for {
		unlock, err := r.Store.LockData()
		if err == nil {
			return unlock, nil
		}
		var oe *output.Error
		if !wait || !errors.As(err, &oe) || oe.Code != "E_DATA_BUSY" || waited >= dataLockWait {
			return nil, err
		}
		if err := r.sleepCtx(ctx, dataLockInterval); err != nil {
			return nil, err
		}
		waited += dataLockInterval
	}
}

func (r *Runner) lock(ctx context.Context, inst string) (func(), error) {
	interval, timeout := r.LockInterval, r.LockTimeout
	if interval <= 0 {
		interval = defaultLockInterval
	}
	if timeout <= 0 {
		timeout = defaultLockTimeout
	}
	var waited time.Duration
	for {
		unlock, err := r.Store.Lock(inst)
		if err == nil {
			return unlock, nil
		}
		var oe *output.Error
		if !errors.As(err, &oe) || oe.Code != "E_INSTANCE_LOCKED" || waited >= timeout {
			return nil, err
		}
		if err := r.sleepCtx(ctx, interval); err != nil {
			return nil, err
		}
		waited += interval
	}
}

func (r *Runner) suffix() string {
	if r.Suffix != nil {
		return r.Suffix()
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// tempName names a throwaway container or volume; the suffix keeps two runs in
// the same second from cleaning up each other's objects.
func (r *Runner) tempName(kind, inst, svc string) string {
	return fmt.Sprintf("cvctl-%s-%s-%s-%d-%s", kind, inst, svc, r.now().Unix(), r.suffix())
}

func (r *Runner) now() time.Time {
	if r.Clock != nil {
		return r.Clock()
	}
	return time.Now()
}

func (r *Runner) instanceDir(inst string) string {
	root := r.DeployRoot
	if root == "" {
		root = "/srv/deploy"
	}
	return filepath.Join(root, inst)
}

func (r *Runner) secrets() []string {
	if r.Host == nil {
		return nil
	}
	var out []string
	for _, s := range []string{r.Host.AccessKey, r.Host.SecretKey, r.Host.LibsodiumKey, r.Host.ResticPassword, r.Host.HostLibsodiumKey, r.Host.HostResticPassword} {
		if len(s) >= 4 {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b string) int { return len(b) - len(a) })
	return out
}

// redact removes every known secret from text and caps its size.
func (r *Runner) redact(text string) string {
	for _, s := range r.secrets() {
		text = strings.ReplaceAll(text, s, "***")
	}
	if len(text) > maxErrBytes {
		text = text[:maxErrBytes] + "…[truncated]"
	}
	return text
}

func notConfigured() error {
	return &output.Error{
		Code:    "E_BACKUP_NOT_CONFIGURED",
		Message: "no backup destination is configured on this host",
		Hint:    "set CVCTL_S3_* in /etc/cvctl/host.env and the backup keys under /etc/cvctl/secrets (run cvctl init)",
	}
}

// cmdFailed builds a redacted failure; argv never holds secrets, but tool
// output might echo them.
func (r *Runner) cmdFailed(cmd sys.Cmd, res sys.Result, runErr error) error {
	e := sys.CmdErr("E_BACKUP_FAILED", cmd, res, runErr, "check the backup storage endpoint, keys and that the data service is running")
	e.Message = r.redact(e.Message)
	return e
}

func (r *Runner) exec(ctx context.Context, cmd sys.Cmd) (sys.Result, error) {
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		return res, r.cmdFailed(cmd, res, err)
	}
	return res, nil
}

// forInstance returns a copy of r whose Host is inst's own backup target. On
// the local silo it first makes sure the instance's backup user exists, so a
// backup or verify works before the instance's first deploy with its own keys.
func (r *Runner) forInstance(ctx context.Context, inst string, p *contract.Project, d *instance.Decisions) (*Runner, error) {
	rr := *r
	rr.Host = render.BackupTargetFor(r.Host, d)
	if rr.Host == nil || !rr.Host.Local || rr.Host.Instance == "" || len(Targets(p, d)) == 0 {
		return &rr, nil
	}
	if _, err := rr.backupNet(ctx, inst); err != nil {
		return nil, err
	}
	if err := ProvisionUser(ctx, rr.Run, rr.Host, inst); err != nil {
		var oe *output.Error
		if errors.As(err, &oe) {
			oe.Message = rr.redact(oe.Message)
			oe.Hint = "deploy the instance first: cvctl deploy " + inst
		}
		return nil, err
	}
	return &rr, nil
}

func (r *Runner) targets(ctx context.Context, inst, only string) (*Runner, []Target, error) {
	p, d, err := r.Load(inst)
	if err != nil {
		return nil, nil, err
	}
	rr, err := r.forInstance(ctx, inst, p, d)
	if err != nil {
		return nil, nil, err
	}
	all := Targets(p, d)
	if only == "" {
		return rr, all, nil
	}
	for _, t := range all {
		if t.Service == only {
			return rr, []Target{t}, nil
		}
	}
	return nil, nil, &output.Error{
		Code:    "E_BACKUP_NO_TARGET",
		Message: fmt.Sprintf("service %q of %s has no backup (not a local data service with backups)", only, inst),
		Hint:    "run cvctl backup list <instance> to see which services are backed up",
	}
}

// BackupNow backs up the targets of inst (only limits it to one service).
// It always attempts every target; when any failed it returns the results
// together with an E_BACKUP_FAILED error.
func (r *Runner) BackupNow(ctx context.Context, inst, only string) ([]Result, error) {
	if r.Host == nil {
		return nil, notConfigured()
	}
	r, ts, err := r.targets(ctx, inst, only)
	if err != nil {
		return nil, err
	}
	unlock, err := r.lockData(ctx, true)
	if err != nil {
		return nil, err
	}
	defer unlock()
	var results []Result
	var failed, statusErrs []string
	for _, t := range ts {
		runErr := func() error {
			// a hung wal-g or restic must not hold the remaining services
			tctx, cancel := context.WithTimeout(ctx, backupTimeout)
			defer cancel()
			if t.Strategy == StrategyWalG {
				return r.walgBackup(tctx, inst, t)
			}
			return r.resticBackup(tctx, inst, t)
		}()
		res := Result{Instance: inst, Service: t.Service, Strategy: t.Strategy, OK: runErr == nil}
		if runErr != nil {
			res.Error = r.redact(runErr.Error())
			failed = append(failed, t.Service)
		}
		results = append(results, res)
		if err := r.record(ctx, inst, t.Service, res); err != nil {
			statusErrs = append(statusErrs, fmt.Sprintf("%s: %s", t.Service, r.redact(err.Error())))
		}
	}
	if len(failed) > 0 {
		return results, &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: fmt.Sprintf("backup of %s failed for: %s", inst, strings.Join(failed, ", ")),
			Hint:    "see cvctl backup list and the per-service error; fix the storage or service and retry",
		}
	}
	if len(statusErrs) > 0 {
		return results, &output.Error{
			Code:    "E_BACKUP_STATUS_WRITE",
			Message: fmt.Sprintf("backups of %s ran but the status could not be recorded: %s", inst, strings.Join(statusErrs, "; ")),
			Hint:    "the instance stayed locked (deploy in progress?); the backups themselves succeeded",
		}
	}
	return results, nil
}

// List returns the backups of one service, or of every target when svc is empty.
func (r *Runner) List(ctx context.Context, inst, svc string) ([]Snapshot, error) {
	if r.Host == nil {
		return nil, notConfigured()
	}
	r, ts, err := r.targets(ctx, inst, svc)
	if err != nil {
		return nil, err
	}
	out := []Snapshot{}
	for _, t := range ts {
		var snaps []Snapshot
		switch t.Strategy {
		case StrategyWalG:
			snaps, err = r.walgList(ctx, inst, t)
		default:
			snaps, err = r.resticList(ctx, inst, t)
		}
		if err != nil {
			return nil, err
		}
		out = append(out, snaps...)
	}
	return out, nil
}

// record stores the outcome under the instance lock. A failure notifies only
// when its message differs from the previously recorded one, so a persistent
// outage does not page on every timer run.
func (r *Runner) record(ctx context.Context, inst, svc string, res Result) error {
	if r.Store == nil {
		return nil
	}
	unlock, err := r.lock(ctx, inst)
	if err != nil {
		return err
	}
	defer unlock()
	st, err := r.Store.Load(inst)
	if err != nil {
		return err
	}
	if st.Backups == nil {
		st.Backups = map[string]state.BackupStatus{}
	}
	bs := st.Backups[svc]
	prev := bs.LastError
	now := r.now()
	bs.LastAttempt = now
	if res.OK {
		bs.LastOK = now
		bs.LastError = ""
	} else {
		bs.LastError = res.Error
	}
	st.Backups[svc] = bs
	if err := r.Store.Save(st); err != nil {
		return err
	}
	if !res.OK && prev != res.Error && r.Notify != nil {
		_ = r.Notify.Send(ctx, notify.Message{
			Host:  r.HostName,
			Level: "error",
			Title: fmt.Sprintf("%s backup failed (%s)", inst, svc),
			Text:  res.Error,
		})
	}
	return nil
}

func envFileBody(m map[string]string) []byte {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + m[k] + "\n")
	}
	return []byte(b.String())
}
