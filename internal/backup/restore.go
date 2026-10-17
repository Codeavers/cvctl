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

	archivePollInterval   = 2 * time.Second
	defaultArchiveTimeout = 120 * time.Second

	// targetNotReachedMsg is what postgres logs when WAL ends before the
	// configured recovery target.
	targetNotReachedMsg = "recovery ended before configured recovery target was reached"

	targetBeforeConsistentMsg = "requested recovery stop point is before consistent recovery point"

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
// recoveryTimeLayout uses a numeric UTC offset: postgres parses -c settings
// before timezone_abbreviations is loaded, so "Z" is rejected at startup.
const recoveryTimeLayout = "2006-01-02 15:04:05.999999-07"

const restoreScript = `set -e
mkdir -p "$PGDATA"
chown postgres:postgres "$PGDATA"
chmod 700 "$PGDATA"
gosu postgres wal-g backup-fetch "$PGDATA" "$RESTORE_BACKUP"
gosu postgres touch "$PGDATA/recovery.signal"
set -- -c listen_addresses= -c archive_mode=off -c "restore_command=wal-g wal-fetch %f %p" -c recovery_target_action=promote
if [ -n "$RESTORE_TARGET_TIME" ]; then
  set -- "$@" -c recovery_target_inclusive=true -c "recovery_target_time=$RESTORE_TARGET_TIME"
fi
if [ -n "$RESTORE_TARGET_NAME" ]; then
  set -- "$@" -c recovery_target_inclusive=true -c "recovery_target_name=$RESTORE_TARGET_NAME"
fi
exec gosu postgres postgres "$@"
`

// RestoreResult describes a finished restore.
type RestoreResult struct {
	Instance       string   `json:"instance"`
	Service        string   `json:"service"`
	Target         string   `json:"target"`
	Volume         string   `json:"volume"`
	PreviousVolume string   `json:"previous_volume"`
	Warnings       []string `json:"warnings,omitempty"`
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

func (r *Runner) pickBackup(ctx context.Context, inst string, t Target, gen int, tgt restoreTarget) (string, error) {
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
			fmt.Sprintf("pick a later target or check cvctl backup list; the target may predate the current backup generation (gen %d): older generations live under <prefix>/g<k> with k < %d and can be restored manually with wal-g", gen, gen))
	}
	return best, nil
}

func envKey(svc, suffix string) string {
	return "CVCTL_SVC_" + contract.Normalize(svc) + "_" + suffix
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

// patchEnv rewrites cvctl.env keeping every other key; a nil value removes
// the key.
func (r *Runner) patchEnv(path string, updates map[string]*string) error {
	m, err := r.readRaw(path)
	if err != nil {
		return err
	}
	for k, v := range updates {
		if v == nil {
			delete(m, k)
		} else {
			m[k] = *v
		}
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

func strp(s string) *string { return &s }

func optp(v string, present bool) *string {
	if !present {
		return nil
	}
	return &v
}

func (r *Runner) volumeExists(ctx context.Context, name string) (bool, error) {
	res, err := r.Run.Run(ctx, r.docker("volume", "inspect", name))
	if err != nil {
		return false, r.cmdFailed(r.docker("volume", "inspect", name), res, err)
	}
	return res.ExitCode == 0, nil
}

func (r *Runner) stopApps(ctx context.Context, inst string, p *contract.Project) error {
	if len(p.Apps) == 0 {
		return nil
	}
	args := []string{"stop"}
	for _, a := range p.Apps {
		args = append(args, a.Name)
	}
	_, err := r.exec(ctx, r.Compose(inst).Cmd(args...))
	return err
}

func (r *Runner) pgQuery(ctx context.Context, inst, svc, db string, c kinds.Credentials, sql string) (string, error) {
	res, err := r.exec(ctx, r.Compose(inst).Cmd("exec", "-T", "-u", "postgres", svc,
		"psql", "-U", c.User, "-d", db, "-v", "ON_ERROR_STOP=1", "-tA", "-c", sql))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(res.Stdout)), nil
}

var walFileName = regexp.MustCompile(`^[0-9A-F]{24}$`)

const codeWALFlush = "E_RESTORE_WAL_FLUSH"

func walFlushErr(cause error) error {
	return &output.Error{
		Code:    codeWALFlush,
		Message: "the data service is running but its WAL could not be flushed: " + cause.Error(),
		Hint:    "the cluster is running but WAL could not be flushed; fix it or re-run with --skip-wal-flush (WAL not yet archived stays in the kept old volume)",
	}
}

// flushWAL switches the WAL segment and waits until the archiver has shipped
// it, so the recovery of "latest" does not lose the last seconds of writes.
// The -1 keeps the answer correct when no switch was needed: the returned
// location is then the start of the current (empty) segment. A data container
// that is not running (it is often the reason for the restore) or an explicit
// skip yields a warning instead of a flush. A running cluster that cannot be
// flushed is a hard stop, never a silent skip: that would hide data loss.
// Queries use the postgres database because the application database may be
// the broken part.
func (r *Runner) flushWAL(ctx context.Context, inst, svc string, c kinds.Credentials, oldVolume string, skip bool) (string, error) {
	skipped := r.redact(fmt.Sprintf("could not flush WAL from the running cluster; WAL not yet archived stays in the kept old volume %s", oldVolume))
	if skip {
		return skipped, nil
	}
	_, running, err := serviceState(ctx, r.Compose(inst), svc)
	if err != nil {
		return "", walFlushErr(err)
	}
	if !running {
		return skipped, nil
	}
	want, err := r.pgQuery(ctx, inst, svc, "postgres", c, "select pg_walfile_name(pg_switch_wal() - 1)")
	if err != nil {
		return "", walFlushErr(err)
	}
	if !walFileName.MatchString(want) {
		return "", restoreFailedErr("E_RESTORE_ARCHIVE_LAG", fmt.Sprintf("unexpected WAL file name %q from postgres", want), "check that the data service is healthy and retry")
	}
	timeout := r.ArchiveTimeout
	if timeout <= 0 {
		timeout = defaultArchiveTimeout
	}
	var waited time.Duration
	for {
		got, err := r.pgQuery(ctx, inst, svc, "postgres", c, "select coalesce(last_archived_wal, '') from pg_stat_archiver")
		if err != nil {
			return "", walFlushErr(err)
		}
		// .backup, .history and .partial names also land here; only a plain
		// segment proves the WAL up to want is archived
		if walFileName.MatchString(got) && got >= want {
			return "", nil
		}
		if waited >= timeout {
			return "", restoreFailedErr("E_RESTORE_ARCHIVE_LAG",
				fmt.Sprintf("WAL segment %s was not archived within %s", want, timeout),
				"check archive_command, the storage endpoint and cvctl backup list; nothing was changed, retry later")
		}
		r.sleep(archivePollInterval)
		waited += archivePollInterval
	}
}

// verifyServing waits for the data service and proves it answers queries.
func (r *Runner) verifyServing(ctx context.Context, inst, svc string, strategy string, c kinds.Credentials) error {
	proj := r.Compose(inst)
	if err := proj.UpData(ctx, []string{svc}); err != nil {
		return err
	}
	if strategy != StrategyWalG {
		return nil
	}
	if _, err := r.exec(ctx, proj.Cmd("exec", "-T", "-u", "postgres", svc, "pg_isready", "-U", c.User, "-d", c.DB)); err != nil {
		return err
	}
	out, err := r.pgQuery(ctx, inst, svc, c.DB, c, "select 1")
	if err != nil {
		return err
	}
	if out != "1" {
		return restoreFailedErr("E_RESTORE_FAILED", fmt.Sprintf("restored postgres answered %q to select 1", out), "inspect the kept volume")
	}
	return nil
}

// Restore recovers a data service into a NEW volume, switches the instance to
// it and rolls back to the previous volume when anything fails after the
// services were stopped. It never deletes a volume. The instance lock is held
// throughout so the agent cannot deploy mid-restore.
func (r *Runner) Restore(ctx context.Context, inst, svc, to string) (*RestoreResult, error) {
	return r.RestoreWith(ctx, inst, svc, to, RestoreOptions{})
}

// RestoreOptions tunes a restore.
type RestoreOptions struct {
	// SkipWALFlush restores without flushing the running cluster's WAL first;
	// the WAL not yet archived stays in the kept old volume.
	SkipWALFlush bool
}

// RestoreWith is Restore with options.
func (r *Runner) RestoreWith(ctx context.Context, inst, svc, to string, opts RestoreOptions) (*RestoreResult, error) {
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
	unlock, err := r.Store.Lock(inst)
	if err != nil {
		return nil, err
	}
	defer unlock()

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
	volKey, genKey, maxKey := envKey(svc, "VOLUME"), envKey(svc, "BACKUP_GEN"), envKey(svc, "BACKUP_GEN_MAX")
	prevVol, hadVol := raw[volKey]
	prevGen, hadGen := raw[genKey]
	newMax := max(instance.AtoiNonNeg(raw[maxKey]), instance.AtoiNonNeg(prevGen)) + 1
	res := &RestoreResult{
		Instance: inst, Service: svc, Target: to,
		PreviousVolume: render.VolumeName(d, svc),
		Volume:         fmt.Sprintf("%s_%s-data-r%d", inst, svc, now.Unix()),
	}
	sd := d.Services[svc]

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
		if backupName, err = r.pickBackup(ctx, inst, *target, d.Services[svc].BackupGen, tgt); err != nil {
			return nil, err
		}
	}

	// Nothing but the app services is touched until the WAL is archived, so a
	// lag leaves volumes and decisions as they were.
	if err := r.stopApps(ctx, inst, p); err != nil {
		return nil, r.rollbackRestore(ctx, restoreRun{inst: inst, svc: svc, res: res, st: st, tgt: tgt}, err)
	}
	flushWarn := ""
	if target.Strategy == StrategyWalG {
		warn, err := r.flushWAL(ctx, inst, svc, sd.Creds, res.PreviousVolume, opts.SkipWALFlush)
		flushWarn = warn
		if err != nil {
			rctx := context.WithoutCancel(ctx)
			msg := r.redact(fmt.Sprintf("restore of %s in %s aborted: %s", svc, inst, err.Error()))
			if _, upErr := r.exec(rctx, r.Compose(inst).Cmd("up", "-d")); upErr != nil {
				msg += "; the app services could not be restarted: " + r.redact(upErr.Error())
			}
			if r.Notify != nil {
				_ = r.Notify.Send(rctx, notify.Message{Host: r.HostName, Level: "error", Title: fmt.Sprintf("%s restore of %s aborted", inst, svc), Text: msg})
			}
			code, hint := "E_RESTORE_FAILED", "check the data service and retry"
			var oe *output.Error
			if errors.As(err, &oe) && (oe.Code == "E_RESTORE_ARCHIVE_LAG" || oe.Code == codeWALFlush) {
				code, hint = oe.Code, oe.Hint
			}
			if aerr := r.Store.Append(inst, state.Event{
				At: now, ID: fmt.Sprintf("restore-%d", now.Unix()), Kind: "restore", From: st.Current, To: st.Current,
				Result: "restore_failed", Detail: code + ": " + msg, Actor: "cli",
			}); aerr != nil {
				return nil, errors.Join(&output.Error{Code: code, Message: msg, Hint: hint}, aerr)
			}
			return nil, &output.Error{Code: code, Message: msg, Hint: hint}
		}
	}

	run := restoreRun{
		inst: inst, svc: svc, res: res, st: st, tgt: tgt, envPath: envPath,
		volKey: volKey, genKey: genKey, prevVol: prevVol, hadVol: hadVol, prevGen: prevGen, hadGen: hadGen,
	}
	runErr := func() error {
		if _, err := r.exec(ctx, r.Compose(inst).Cmd("stop", svc)); err != nil {
			return err
		}
		if _, err := r.exec(ctx, r.docker("volume", "create", res.Volume)); err != nil {
			return err
		}
		run.created = true
		if target.Strategy == StrategyWalG {
			if err := r.walgRecover(ctx, inst, svc, p, d, backupName, tgt, res.Volume); err != nil {
				return err
			}
		} else if err := r.resticRecover(ctx, inst, svc, tgt, res.Volume); err != nil {
			return err
		}
		updates := map[string]*string{volKey: strp(res.Volume)}
		if target.Strategy == StrategyWalG {
			// the abandoned generation must never be reused, even after a rollback
			updates[genKey] = strp(strconv.Itoa(newMax))
			updates[maxKey] = strp(strconv.Itoa(newMax))
		}
		if err := r.patchEnv(envPath, updates); err != nil {
			return err
		}
		run.switched = true
		if err := r.Apply(ctx, inst, st.Current); err != nil {
			return err
		}
		return r.verifyServing(ctx, inst, svc, target.Strategy, sd.Creds)
	}()
	if runErr != nil {
		return nil, r.rollbackRestore(ctx, run, runErr)
	}

	if flushWarn != "" {
		res.Warnings = append(res.Warnings, flushWarn)
	}
	if target.Strategy == StrategyWalG {
		// a base backup in the new generation makes it restorable on its own
		if _, err := r.exec(ctx, r.Compose(inst).Cmd("exec", "-T", "-u", "postgres", svc, "wal-g", "backup-push", pgData)); err != nil {
			res.Warnings = append(res.Warnings, r.redact("base backup of the new generation failed: "+err.Error()+"; run cvctl backup now "+inst+" "+svc))
		}
	}
	text := fmt.Sprintf("%s of %s restored to %s into volume %s; previous volume %s is kept (cvctl db cleanup removes unused volumes)",
		svc, inst, to, res.Volume, res.PreviousVolume)
	if r.Notify != nil {
		_ = r.Notify.Send(ctx, notify.Message{Host: r.HostName, Level: "info", Title: fmt.Sprintf("%s restore of %s done", inst, svc), Text: text})
		for _, w := range res.Warnings {
			title := fmt.Sprintf("%s restore of %s: backup needed", inst, svc)
			if w == flushWarn {
				title = fmt.Sprintf("%s/%s: WAL flush skipped during restore", inst, svc)
			}
			_ = r.Notify.Send(ctx, notify.Message{Host: r.HostName, Level: "warn", Title: title, Text: w})
		}
	}
	if err := r.Store.Append(inst, state.Event{
		At: now, ID: fmt.Sprintf("restore-%d", now.Unix()), Kind: "restore", From: st.Current, To: st.Current,
		Result: "ok", Detail: text, Actor: "cli",
	}); err != nil {
		return res, err
	}
	return res, nil
}

type restoreRun struct {
	inst, svc, envPath, volKey, genKey, prevVol, prevGen string
	hadVol, hadGen, switched, created                    bool
	res                                                  *RestoreResult
	st                                                   *state.State
	tgt                                                  restoreTarget
}

// rollbackRestore puts the previous volume and generation back. The maximum
// generation stays advanced so the abandoned one is never reused.
func (r *Runner) rollbackRestore(ctx context.Context, run restoreRun, cause error) error {
	// A cancelled ctx must not leave the instance stopped.
	rctx := context.WithoutCancel(ctx)
	var back []string
	if run.switched {
		if err := r.patchEnv(run.envPath, map[string]*string{
			run.volKey: optp(run.prevVol, run.hadVol),
			run.genKey: optp(run.prevGen, run.hadGen),
		}); err != nil {
			back = append(back, "could not restore the previous volume setting: "+err.Error())
		}
	}
	if len(back) == 0 {
		if err := r.Apply(rctx, run.inst, run.st.Current); err != nil {
			back = append(back, "could not restart the instance on the previous volume: "+err.Error())
		}
	}
	stateMsg := fmt.Sprintf("the instance is back on volume %s", run.res.PreviousVolume)
	if len(back) > 0 {
		stateMsg = "ROLLBACK FAILED, intervene manually: " + strings.Join(back, "; ")
	}
	kept := ""
	if run.created {
		kept = fmt.Sprintf(" The recovered volume %s was kept for inspection.", run.res.Volume)
	}
	code := "E_RESTORE_FAILED"
	var oe *output.Error
	if errors.As(cause, &oe) && oe.Code == codeRestoreTarget {
		code = codeRestoreTarget
	}
	msg := r.redact(fmt.Sprintf("restore of %s in %s failed: %s; %s.%s", run.svc, run.inst, cause.Error(), stateMsg, kept))
	hint := "inspect the kept volume, fix the cause and retry; cvctl db cleanup removes unused volumes"
	if code == codeRestoreTarget {
		hint = "the WAL archive ends before the target; pick an earlier target or --to latest"
		if _, bounded := run.tgt.boundTime(); run.tgt.kind == "name" && !bounded {
			hint = "the newest base backup is newer than the restore point; restore with --to <timestamp before the deploy> instead"
		}
	}
	now := r.now()
	if r.Notify != nil {
		_ = r.Notify.Send(rctx, notify.Message{Host: r.HostName, Level: "error", Title: fmt.Sprintf("%s restore of %s FAILED", run.inst, run.svc), Text: msg})
	}
	e := &output.Error{Code: code, Message: msg, Hint: hint}
	if err := r.Store.Append(run.inst, state.Event{
		At: now, ID: fmt.Sprintf("restore-%d", now.Unix()), Kind: "restore", From: run.st.Current, To: run.st.Current,
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
		args = append(args, "-e", "RESTORE_TARGET_TIME="+tgt.at.UTC().Format(recoveryTimeLayout))
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
			if strings.Contains(text, targetNotReachedMsg) || strings.Contains(text, targetBeforeConsistentMsg) {
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
