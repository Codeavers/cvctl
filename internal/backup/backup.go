// Package backup runs and records WAL-G and restic backups of local data
// services, and creates postgres restore points.
package backup

import (
	"context"
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
	pgData        = "/var/lib/postgresql/data"
	resticImage   = "restic/restic:0.19.1"
	backupNetwork = "cvctl-backup"
	dockerConfig  = "DOCKER_CONFIG=/var/lib/cvctl/docker"
	walgRetain    = "7"
	maxErrBytes   = 2048
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
	Run        sys.Runner
	FS         sys.FS
	Compose    func(inst string) *compose.Project
	Load       Loader
	Host       *render.BackupTarget
	Store      *state.Store
	Notify     notify.Notifier
	Clock      func() time.Time
	DeployRoot string
	HostName   string
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
	for _, s := range []string{r.Host.AccessKey, r.Host.SecretKey, r.Host.LibsodiumKey, r.Host.ResticPassword} {
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

func (r *Runner) targets(inst, only string) ([]Target, error) {
	p, d, err := r.Load(inst)
	if err != nil {
		return nil, err
	}
	all := Targets(p, d)
	if only == "" {
		return all, nil
	}
	for _, t := range all {
		if t.Service == only {
			return []Target{t}, nil
		}
	}
	return nil, &output.Error{
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
	ts, err := r.targets(inst, only)
	if err != nil {
		return nil, err
	}
	var results []Result
	var failed []string
	for _, t := range ts {
		var runErr error
		switch t.Strategy {
		case StrategyWalG:
			runErr = r.walgBackup(ctx, inst, t)
		default:
			runErr = r.resticBackup(ctx, inst, t)
		}
		res := Result{Instance: inst, Service: t.Service, Strategy: t.Strategy, OK: runErr == nil}
		if runErr != nil {
			res.Error = r.redact(runErr.Error())
			failed = append(failed, t.Service)
		}
		results = append(results, res)
		if err := r.record(ctx, inst, t.Service, res); err != nil {
			return results, err
		}
	}
	if len(failed) > 0 {
		return results, &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: fmt.Sprintf("backup of %s failed for: %s", inst, strings.Join(failed, ", ")),
			Hint:    "see cvctl backup list and the per-service error; fix the storage or service and retry",
		}
	}
	return results, nil
}

// List returns the backups of one service, or of every target when svc is empty.
func (r *Runner) List(ctx context.Context, inst, svc string) ([]Snapshot, error) {
	if r.Host == nil {
		return nil, notConfigured()
	}
	ts, err := r.targets(inst, svc)
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
	unlock, err := r.Store.Lock(inst)
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
