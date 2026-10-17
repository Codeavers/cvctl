package backup

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/state"
	"github.com/codeavers/cvctl/kinds"
)

const (
	restorePollInterval = 5 * time.Second
	restoreReadyTimeout = 2 * time.Hour
	restoreStopTimeout  = "120"

	// targetNotReachedMsg is what postgres logs when WAL ends before the
	// configured recovery target.
	targetNotReachedMsg = "recovery ended before configured recovery target was reached"

	codeRestoreTarget = "E_RESTORE_TARGET"
)

var (
	walgBackupName = regexp.MustCompile(`^base_[A-Za-z0-9_]+$`)
	resticSnapID   = regexp.MustCompile(`^[0-9a-f]{6,64}$`)
	instanceName   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// restoreScript runs as root in the recovery container and drops to the
// postgres user because the image entrypoint is bypassed. The target comes
// from RESTORE_TARGET_TIME / RESTORE_TARGET_NAME (both empty means replay all
// archived WAL); they are validated by the caller before reaching the
// container environment.
const restoreScript = `set -e
mkdir -p "$PGDATA"
chown postgres:postgres "$PGDATA"
chmod 700 "$PGDATA"
gosu postgres wal-g backup-fetch "$PGDATA" "$RESTORE_BACKUP"
gosu postgres touch "$PGDATA/recovery.signal"
{
  echo "restore_command = 'wal-g wal-fetch %f %p'"
  echo "recovery_target_action = 'promote'"
  if [ -n "$RESTORE_TARGET_TIME" ]; then
    echo "recovery_target_inclusive = true"
    echo "recovery_target_time = '$RESTORE_TARGET_TIME'"
  fi
  if [ -n "$RESTORE_TARGET_NAME" ]; then
    echo "recovery_target_inclusive = true"
    echo "recovery_target_name = '$RESTORE_TARGET_NAME'"
  fi
} | gosu postgres tee -a "$PGDATA/postgresql.auto.conf" >/dev/null
exec gosu postgres postgres -c listen_addresses=
`

// RestoreResult describes a finished restore.
type RestoreResult struct {
	Instance       string `json:"instance"`
	Service        string `json:"service"`
	Target         string `json:"target"`
	Volume         string `json:"volume"`
	PreviousVolume string `json:"previous_volume"`
}

type restoreTarget struct {
	raw  string
	kind string // latest | time | name | snapshot
	at   time.Time
	name string
}

func restoreInvalid(msg, hint string) *output.Error {
	return &output.Error{Code: "E_RESTORE_TARGET_INVALID", Message: msg, Hint: hint}
}

func parseRestoreTarget(to, strategy string, now time.Time) (restoreTarget, error) {
	t := restoreTarget{raw: to}
	if to == "latest" {
		t.kind = "latest"
		return t, nil
	}
	if strategy == StrategyVolume {
		if !resticSnapID.MatchString(to) {
			return t, restoreInvalid(fmt.Sprintf("invalid restore target %q for a volume backup", to),
				"use --to latest or a restic snapshot id from cvctl backup list")
		}
		t.kind, t.name = "snapshot", to
		return t, nil
	}
	if restorePointID.MatchString(to) {
		t.kind, t.name = "name", to
		return t, nil
	}
	at, err := time.Parse(time.RFC3339, to)
	if err != nil {
		return t, restoreInvalid(fmt.Sprintf("invalid restore target %q", to),
			"use --to latest, an RFC3339 timestamp (2026-10-17T10:30:00Z) or a restore point deploy-<id>")
	}
	if !at.Before(now) {
		return t, restoreInvalid(fmt.Sprintf("restore target %s is not in the past", to),
			"pick a timestamp earlier than now, or use --to latest")
	}
	t.kind, t.at = "time", at.UTC()
	return t, nil
}

// boundTime is the latest instant the base backup may have finished at, and
// false when any base backup works.
func (t restoreTarget) boundTime() (time.Time, bool) {
	switch t.kind {
	case "time":
		return t.at, true
	case "name":
		// deploy ids are the deploy start in unix nanoseconds
		n, err := strconv.ParseInt(strings.TrimPrefix(t.name, "deploy-"), 10, 64)
		if err != nil || n < 1e17 {
			return time.Time{}, false
		}
		return time.Unix(0, n).UTC(), true
	}
	return time.Time{}, false
}

func restoreFailedErr(code, msg, hint string) *output.Error {
	return &output.Error{Code: code, Message: msg, Hint: hint}
}

func (r *Runner) pickBackup(ctx context.Context, inst string, t Target, tgt restoreTarget) (string, error) {
	bound, ok := tgt.boundTime()
	if !ok {
		return "LATEST", nil
	}
	snaps, err := r.walgList(ctx, inst, t)
	if err != nil {
		return "", err
	}
	best, bestAt := "", time.Time{}
	for _, s := range snaps {
		at, err := time.Parse(time.RFC3339Nano, s.Time)
		if err != nil || at.After(bound) || !walgBackupName.MatchString(s.ID) {
			continue
		}
		if best == "" || at.After(bestAt) {
			best, bestAt = s.ID, at
		}
	}
	if best == "" {
		return "", restoreFailedErr("E_RESTORE_NO_BACKUP",
			fmt.Sprintf("no base backup of %s finished before %s", t.Service, bound.Format(time.RFC3339)),
			"pick a later target or check cvctl backup list")
	}
	return best, nil
}

func envKey(svc string) string {
	return "CVCTL_SVC_" + contract.Normalize(svc) + "_VOLUME"
}

func (r *Runner) readRaw(path string) (map[string]string, error) {
	data, err := r.FS.ReadFile(path)
	if err != nil {
		return nil, &output.Error{
			Code:    "E_DECISION_READ",
			Message: fmt.Sprintf("cannot read %s: %v", path, err),
			Hint:    "check that the instance exists and cvctl.env is readable",
		}
	}
	m, err := godotenv.Unmarshal(string(data))
	if err != nil {
		return nil, &output.Error{
			Code:    "E_DECISION_READ",
			Message: fmt.Sprintf("%s is malformed (details hidden: the file holds secrets)", path),
			Hint:    "fix the KEY=value syntax in cvctl.env",
		}
	}
	return m, nil
}

// setEnvKey rewrites cvctl.env keeping every other key; present=false removes
// the key.
func (r *Runner) setEnvKey(path, key, val string, present bool) error {
	m, err := r.readRaw(path)
	if err != nil {
		return err
	}
	if present {
		m[key] = val
	} else {
		delete(m, key)
	}
	if err := r.FS.WriteFile(path, instance.FormatEnv(m), 0o640, deployOwner); err != nil {
		return &output.Error{
			Code:    "E_FILE_WRITE",
			Message: fmt.Sprintf("cannot write %s: %v", path, err),
			Hint:    "check permissions and free space of the instance directory",
		}
	}
	return nil
}

func (r *Runner) volumeExists(ctx context.Context, name string) (bool, error) {
	res, err := r.Run.Run(ctx, r.docker("volume", "inspect", name))
	if err != nil {
		return false, r.cmdFailed(r.docker("volume", "inspect", name), res, err)
	}
	return res.ExitCode == 0, nil
}

func (r *Runner) stopServices(ctx context.Context, inst string, p *contract.Project, svc string) error {
	proj := r.Compose(inst)
	if len(p.Apps) > 0 {
		args := []string{"stop"}
		for _, a := range p.Apps {
			args = append(args, a.Name)
		}
		if _, err := r.exec(ctx, proj.Cmd(args...)); err != nil {
			return err
		}
	}
	_, err := r.exec(ctx, proj.Cmd("stop", svc))
	return err
}

// Restore recovers a data service into a NEW volume, switches the instance to
// it and rolls back to the previous volume when anything fails after the
// services were stopped. It never deletes a volume. The instance lock is held
// throughout so the agent cannot deploy mid-restore.
func (r *Runner) Restore(ctx context.Context, inst, svc, to string) (*RestoreResult, error) {
	if r.Host == nil {
		return nil, notConfigured()
	}
	if r.Store == nil || r.Apply == nil || r.FS == nil || r.Compose == nil {
		return nil, &output.Error{
			Code:    "E_RESTORE_MISCONFIGURED",
			Message: "restore is not wired (store, filesystem, compose or apply missing)",
			Hint:    "this is a cvctl bug; report it",
		}
	}
	p, d, err := r.Load(inst)
	if err != nil {
		return nil, err
	}
	var target *Target
	for _, t := range Targets(p, d) {
		if t.Service == svc {
			t := t
			target = &t
		}
	}
	if target == nil {
		return nil, &output.Error{
			Code:    "E_BACKUP_NO_TARGET",
			Message: fmt.Sprintf("service %q of %s has no backup (not a local data service with backups)", svc, inst),
			Hint:    "run cvctl backup list <instance> to see which services are backed up",
		}
	}
	now := r.now()
	tgt, err := parseRestoreTarget(to, target.Strategy, now)
	if err != nil {
		return nil, err
	}

	unlock, err := r.Store.Lock(inst)
	if err != nil {
		return nil, err
	}
	defer unlock()

	st, err := r.Store.Load(inst)
	if err != nil {
		return nil, err
	}
	if st.Current == nil {
		return nil, &output.Error{
			Code:    "E_RESTORE_NO_STATE",
			Message: fmt.Sprintf("instance %q has never been deployed", inst),
			Hint:    "deploy the instance first; there is nothing to restore into",
		}
	}

	envPath := r.instanceDir(inst) + "/cvctl.env"
	raw, err := r.readRaw(envPath)
	if err != nil {
		return nil, err
	}
	key := envKey(svc)
	prevRaw, hadPrev := raw[key]
	res := &RestoreResult{
		Instance: inst, Service: svc, Target: to,
		PreviousVolume: render.VolumeName(d, svc),
		Volume:         fmt.Sprintf("%s_%s-data-r%d", inst, svc, now.Unix()),
	}

	// docker volume create is idempotent: refuse to recover onto a volume
	// that already holds data.
	exists, err := r.volumeExists(ctx, res.Volume)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, &output.Error{
			Code:    "E_RESTORE_VOLUME_EXISTS",
			Message: fmt.Sprintf("volume %s already exists", res.Volume),
			Hint:    "wait a second and retry; names carry the unix time of the attempt",
		}
	}

	var backupName string
	if target.Strategy == StrategyWalG {
		// listing needs the running data container, so it precedes the stop
		if backupName, err = r.pickBackup(ctx, inst, *target, tgt); err != nil {
			return nil, err
		}
	}

	var created, switched bool
	runErr := func() error {
		if err := r.stopServices(ctx, inst, p, svc); err != nil {
			return err
		}
		if _, err := r.exec(ctx, r.docker("volume", "create", res.Volume)); err != nil {
			return err
		}
		created = true
		if target.Strategy == StrategyWalG {
			if err := r.walgRecover(ctx, inst, svc, p, d, backupName, tgt, res.Volume); err != nil {
				return err
			}
		} else if err := r.resticRecover(ctx, inst, svc, tgt, res.Volume); err != nil {
			return err
		}
		if err := r.setEnvKey(envPath, key, res.Volume, true); err != nil {
			return err
		}
		switched = true
		return r.Apply(ctx, inst, st.Current)
	}()
	if runErr != nil {
		return nil, r.rollbackRestore(ctx, inst, svc, res, envPath, key, prevRaw, hadPrev, switched, created, runErr, st)
	}

	text := fmt.Sprintf("%s of %s restored to %s into volume %s; previous volume %s is kept (cvctl db cleanup removes unused volumes)",
		svc, inst, to, res.Volume, res.PreviousVolume)
	if r.Notify != nil {
		_ = r.Notify.Send(ctx, notify.Message{Host: r.HostName, Level: "info", Title: fmt.Sprintf("%s restore of %s done", inst, svc), Text: text})
	}
	if err := r.Store.Append(inst, state.Event{
		At: now, ID: fmt.Sprintf("restore-%d", now.Unix()), Kind: "restore", From: st.Current, To: st.Current,
		Result: "ok", Detail: text, Actor: "cli",
	}); err != nil {
		return res, err
	}
	return res, nil
}

func (r *Runner) rollbackRestore(ctx context.Context, inst, svc string, res *RestoreResult, envPath, key, prevRaw string, hadPrev, switched, created bool, cause error, st *state.State) error {
	// A cancelled ctx must not leave the instance stopped.
	rctx := context.WithoutCancel(ctx)
	var back []string
	if switched {
		if err := r.setEnvKey(envPath, key, prevRaw, hadPrev); err != nil {
			back = append(back, "could not restore the previous volume setting: "+err.Error())
		}
	}
	if len(back) == 0 {
		if err := r.Apply(rctx, inst, st.Current); err != nil {
			back = append(back, "could not restart the instance on the previous volume: "+err.Error())
		}
	}
	stateMsg := fmt.Sprintf("the instance is back on volume %s", res.PreviousVolume)
	if len(back) > 0 {
		stateMsg = "ROLLBACK FAILED, intervene manually: " + strings.Join(back, "; ")
	}
	kept := ""
	if created {
		kept = fmt.Sprintf(" The recovered volume %s was kept for inspection.", res.Volume)
	}
	code := "E_RESTORE_FAILED"
	var oe *output.Error
	if errors.As(cause, &oe) && oe.Code == codeRestoreTarget {
		code = codeRestoreTarget
	}
	msg := r.redact(fmt.Sprintf("restore of %s in %s failed: %s; %s.%s", svc, inst, cause.Error(), stateMsg, kept))
	hint := "inspect the kept volume, fix the cause and retry; cvctl db cleanup removes unused volumes"
	if code == codeRestoreTarget {
		hint = "the WAL archive ends before the target; pick an earlier target or --to latest"
	}
	now := r.now()
	if r.Notify != nil {
		_ = r.Notify.Send(rctx, notify.Message{Host: r.HostName, Level: "error", Title: fmt.Sprintf("%s restore of %s FAILED", inst, svc), Text: msg})
	}
	e := &output.Error{Code: code, Message: msg, Hint: hint}
	if err := r.Store.Append(inst, state.Event{
		At: now, ID: fmt.Sprintf("restore-%d", now.Unix()), Kind: "restore", From: st.Current, To: st.Current,
		Result: "restore_failed", Detail: msg, Actor: "cli",
	}); err != nil {
		return errors.Join(e, err)
	}
	return e
}

func (r *Runner) walgRecover(ctx context.Context, inst, svc string, p *contract.Project, d *instance.Decisions, backupName string, tgt restoreTarget, volume string) error {
	if backupName != "LATEST" && !walgBackupName.MatchString(backupName) {
		return restoreInvalid(fmt.Sprintf("invalid base backup name %q", backupName), "check cvctl backup list")
	}
	cat, err := r.catalog()
	if err != nil {
		return err
	}
	var ds contract.DataService
	for _, x := range p.Data {
		if x.Name == svc {
			ds = x
		}
	}
	sd := d.Services[svc]
	image, err := cat[ds.Kind].ImageFor(sd.Version)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("cvctl-restore-%s-%s-%d", inst, svc, r.now().Unix())
	defer r.cleanup(ctx, name, "")

	args := []string{
		"run", "-d", "--name", name, "--network", backupNetwork,
		"--env-file", render.BackupEnvPath(r.instanceDir(inst), svc),
		"-v", volume + ":" + pgData,
		"-e", "PGDATA=" + pgData,
		"-e", "PGUSER=" + sd.Creds.User,
		"-e", "PGDATABASE=" + sd.Creds.DB,
		"-e", "PGHOST=/var/run/postgresql",
		"-e", "RESTORE_BACKUP=" + backupName,
	}
	switch tgt.kind {
	case "time":
		args = append(args, "-e", "RESTORE_TARGET_TIME="+tgt.at.Format(time.RFC3339))
	case "name":
		args = append(args, "-e", "RESTORE_TARGET_NAME="+tgt.name)
	}
	args = append(args, "--entrypoint", "sh", image, "-c", restoreScript)
	if _, err := r.exec(ctx, r.docker(args...)); err != nil {
		return err
	}
	if err := r.waitRestored(ctx, name, sd.Creds); err != nil {
		return err
	}
	// a clean shutdown leaves the volume ready for the real container
	_, err = r.exec(ctx, r.docker("stop", "-t", restoreStopTimeout, name))
	return err
}

func (r *Runner) waitRestored(ctx context.Context, name string, c kinds.Credentials) error {
	var waited time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return restoreFailedErr("E_RESTORE_FAILED", "restore cancelled: "+err.Error(), "retry cvctl db restore")
		}
		running, _ := r.Run.Run(ctx, r.docker("inspect", "-f", "{{.State.Running}}", name))
		if running.ExitCode == 0 && strings.TrimSpace(string(running.Stdout)) == "false" {
			logs, _ := r.Run.Run(ctx, r.docker("logs", "--tail", "50", name))
			text := string(logs.Stderr) + string(logs.Stdout)
			if strings.Contains(text, targetNotReachedMsg) {
				return restoreFailedErr(codeRestoreTarget, "the WAL archive ends before the requested recovery target", "pick an earlier target or --to latest")
			}
			return restoreFailedErr("E_RESTORE_FAILED", "recovery container exited before postgres was ready: "+r.redact(strings.TrimSpace(text)),
				"the base backup or its WAL could not be fetched or replayed; check backup-list and the storage")
		}
		if res, err := r.Run.Run(ctx, r.docker("exec", name, "pg_isready", "-U", c.User, "-d", c.DB)); err == nil && res.ExitCode == 0 {
			q, err := r.Run.Run(ctx, r.docker("exec", name, "psql", "-U", c.User, "-d", c.DB, "-tA", "-c", "select pg_is_in_recovery()"))
			if err == nil && q.ExitCode == 0 && strings.TrimSpace(string(q.Stdout)) == "f" {
				return nil
			}
		}
		if waited >= restoreReadyTimeout {
			return restoreFailedErr("E_RESTORE_FAILED", fmt.Sprintf("recovery did not finish after %s", restoreReadyTimeout), "check the WAL archive; the recovered volume is kept")
		}
		r.sleep(restorePollInterval)
		waited += restorePollInterval
	}
}

func (r *Runner) resticRecover(ctx context.Context, inst, svc string, tgt restoreTarget, volume string) error {
	snap := "latest"
	if tgt.kind == "snapshot" {
		snap = tgt.name
	}
	envFile, err := r.writeResticEnv(inst, svc)
	if err != nil {
		return err
	}
	// <snapshot>:/data restores the backed-up directory's content at the volume root
	_, err = r.exec(ctx, r.docker("run", "--rm", "--network", backupNetwork, "-v", volume+":/restore",
		"--env-file", envFile, resticImage, "restore", snap+":/data", "--target", "/restore"))
	return err
}

// CleanupVolume is an unused data volume of an instance.
type CleanupVolume struct {
	Name    string `json:"name"`
	Removed bool   `json:"removed"`
}

// Cleanup lists the data volumes of inst that no cvctl.env decision (or
// default name) references and no container uses, and removes them when yes.
// Only volumes named like a data service's (<inst>_<svc>-data[-r<unix>]) are
// candidates, so app volumes and other instances are never touched.
func (r *Runner) Cleanup(ctx context.Context, inst string, yes bool) ([]CleanupVolume, error) {
	if !instanceName.MatchString(inst) {
		return nil, &output.Error{
			Code:    "E_INSTANCE_NOT_FOUND",
			Message: fmt.Sprintf("invalid instance name %q", inst),
			Hint:    "check the instance name with cvctl status",
		}
	}
	if r.Store != nil {
		unlock, err := r.Store.Lock(inst)
		if err != nil {
			return nil, err
		}
		defer unlock()
	}
	p, d, err := r.Load(inst)
	if err != nil {
		return nil, err
	}
	raw, err := r.readRaw(r.instanceDir(inst) + "/cvctl.env")
	if err != nil {
		return nil, err
	}
	referenced := map[string]bool{}
	for k, v := range raw {
		if strings.HasPrefix(k, "CVCTL_SVC_") && strings.HasSuffix(k, "_VOLUME") {
			referenced[v] = true
		}
	}
	var patterns []*regexp.Regexp
	for _, ds := range p.Data {
		referenced[render.VolumeName(d, ds.Name)] = true
		patterns = append(patterns, regexp.MustCompile("^"+regexp.QuoteMeta(inst+"_"+ds.Name+"-data")+`(-r[0-9]+)?$`))
	}
	res, err := r.exec(ctx, r.docker("volume", "ls", "-q"))
	if err != nil {
		return nil, err
	}
	out := []CleanupVolume{}
	for _, v := range strings.Fields(string(res.Stdout)) {
		if !strings.HasPrefix(v, inst+"_") || referenced[v] || !matchesAny(patterns, v) {
			continue
		}
		ps, err := r.exec(ctx, r.docker("ps", "-a", "-q", "--filter", "volume="+v))
		if err != nil {
			return out, err
		}
		if strings.TrimSpace(string(ps.Stdout)) != "" {
			continue
		}
		out = append(out, CleanupVolume{Name: v})
	}
	if !yes {
		return out, nil
	}
	var failed []string
	for i := range out {
		if _, err := r.exec(ctx, r.docker("volume", "rm", out[i].Name)); err != nil {
			failed = append(failed, out[i].Name)
			continue
		}
		out[i].Removed = true
	}
	if len(failed) > 0 {
		return out, &output.Error{
			Code:    "E_CLEANUP_FAILED",
			Message: fmt.Sprintf("could not remove: %s", strings.Join(failed, ", ")),
			Hint:    "a container may have started using the volume; retry cvctl db cleanup",
		}
	}
	return out, nil
}

func matchesAny(res []*regexp.Regexp, s string) bool {
	for _, re := range res {
		if re.MatchString(s) {
			return true
		}
	}
	return false
}
