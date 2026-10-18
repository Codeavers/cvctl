package backup

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

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
	verifyPollInterval = 5 * time.Second
	// a large cluster replays WAL as slowly in a verify as in a restore
	verifyReadyTimeout = restoreReadyTimeout
	cleanupTimeout     = time.Minute
)

// verifyScript runs inside the throwaway container as root and drops to the
// postgres user, because the image entrypoint is bypassed. PGDATA comes from
// the container environment. The restored cluster replays archived WAL up to
// the end of the archive and then promotes. Recovery settings are command-line
// flags, never written to postgresql.auto.conf: a later base backup would
// carry them and break the next restore.
const verifyScript = `set -e
mkdir -p "$PGDATA"
chown postgres:postgres "$PGDATA"
chmod 700 "$PGDATA"
gosu postgres wal-g backup-fetch "$PGDATA" LATEST
gosu postgres touch "$PGDATA/recovery.signal"
exec gosu postgres postgres -c listen_addresses= -c archive_mode=off -c "restore_command=wal-g wal-fetch %f %p" -c recovery_target_action=promote
`

const resticVerifyScript = `set -e
restic restore latest --target /restore >/dev/null
find /restore -type f | wc -l
`

func verifyFailed(msg, hint string) *output.Error {
	return &output.Error{Code: "E_VERIFY_FAILED", Message: msg, Hint: hint}
}

func verifyEmpty(msg string) *output.Error {
	return &output.Error{
		Code:    "E_VERIFY_EMPTY",
		Message: msg,
		Hint:    "the latest backup restored but holds no data; run cvctl backup now and check the data service",
	}
}

func (r *Runner) catalog() (kinds.Catalog, error) {
	if r.Catalog != nil {
		return r.Catalog, nil
	}
	return kinds.Load()
}

// Verify restores the latest backup of each target into a throwaway container
// and checks it holds data. Every target is attempted; a failure returns the
// results together with an E_VERIFY_FAILED error.
func (r *Runner) Verify(ctx context.Context, inst, only string) ([]Result, error) {
	if r.Host == nil {
		return nil, notConfigured()
	}
	p, d, err := r.Load(inst)
	if err != nil {
		return nil, err
	}
	r, ts, err := r.targets(ctx, inst, only)
	if err != nil {
		return nil, err
	}
	cat, err := r.catalog()
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
			tctx, cancel := context.WithTimeout(ctx, verifyTimeout)
			defer cancel()
			if t.Strategy == StrategyWalG {
				return r.walgVerify(tctx, inst, p, d, cat, t)
			}
			return r.resticVerify(tctx, inst, t)
		}()
		res := Result{Instance: inst, Service: t.Service, Strategy: t.Strategy, OK: runErr == nil}
		if runErr != nil {
			res.Error = r.redact(runErr.Error())
			failed = append(failed, t.Service)
		}
		results = append(results, res)
		if err := r.recordVerify(ctx, inst, t.Service, res); err != nil {
			statusErrs = append(statusErrs, fmt.Sprintf("%s: %s", t.Service, r.redact(err.Error())))
		}
	}
	if len(failed) > 0 {
		return results, &output.Error{
			Code:    "E_VERIFY_FAILED",
			Message: fmt.Sprintf("restore verification of %s failed for: %s", inst, strings.Join(failed, ", ")),
			Hint:    "see the per-service error; the backup may be missing, empty or unreadable",
		}
	}
	if len(statusErrs) > 0 {
		return results, &output.Error{
			Code:    "E_BACKUP_STATUS_WRITE",
			Message: fmt.Sprintf("verification of %s ran but the status could not be recorded: %s", inst, strings.Join(statusErrs, "; ")),
			Hint:    "the instance stayed locked (deploy in progress?); the verification itself succeeded",
		}
	}
	return results, nil
}

func (r *Runner) docker(args ...string) sys.Cmd {
	return sys.Cmd{Name: "docker", Args: args, Env: []string{dockerConfig}}
}

// cleanup must outlive a cancelled ctx or the volume and container leak.
func (r *Runner) cleanup(ctx context.Context, name, volume string) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
	defer cancel()
	_, _ = r.Run.Run(cctx, r.docker("rm", "-f", name))
	if volume != "" {
		_, _ = r.Run.Run(cctx, r.docker("volume", "rm", "-f", volume))
	}
}

func (r *Runner) sleep(d time.Duration) {
	if r.Sleep != nil {
		r.Sleep(d)
		return
	}
	time.Sleep(d)
}

// sleepCtx waits d or until ctx ends: the CLI turns SIGINT/SIGTERM into a
// cancelled context, so lock waits must observe it or Ctrl-C does nothing.
func (r *Runner) sleepCtx(ctx context.Context, d time.Duration) error {
	if r.Sleep != nil {
		r.Sleep(d)
	} else {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
		}
	}
	if ctx.Err() != nil {
		return &output.Error{
			Code:    "E_CANCELLED",
			Message: "cancelled while waiting for a lock",
			Hint:    "nothing was changed; run the command again",
		}
	}
	return nil
}

func (r *Runner) walgVerify(ctx context.Context, inst string, p *contract.Project, d *instance.Decisions, cat kinds.Catalog, t Target) error {
	sd := d.Services[t.Service]
	var ds contract.DataService
	for _, x := range p.Data {
		if x.Name == t.Service {
			ds = x
		}
	}
	kind := cat[ds.Kind]
	image, err := kind.ImageFor(sd.Version)
	if err != nil {
		return err
	}
	net, err := r.backupNet(ctx, inst)
	if err != nil {
		return err
	}
	name := r.tempName("verify", inst, t.Service)
	if _, err := r.exec(ctx, r.docker("volume", "create", name)); err != nil {
		return err
	}
	defer r.cleanup(ctx, name, name)

	envFile := render.BackupEnvPath(r.instanceDir(inst), t.Service)
	args := []string{
		"run", "-d", "--name", name, "--network", net,
		"--env-file", envFile,
		"-v", name + ":" + pgData,
		"-e", "PGDATA=" + pgData,
		"-e", "PGUSER=" + sd.Creds.User,
		"-e", "PGDATABASE=" + sd.Creds.DB,
		"-e", "PGHOST=/var/run/postgresql",
		"--entrypoint", "sh", image, "-c", verifyScript,
	}
	if _, err := r.exec(ctx, r.docker(args...)); err != nil {
		return err
	}
	if err := r.waitRecovered(ctx, name, sd.Creds.User, sd.Creds.DB); err != nil {
		return err
	}
	check := kind.RestoreCheck
	if check == "" {
		check = "select count(*) from information_schema.tables where table_schema='public'"
	}
	out, err := r.psql(ctx, name, sd.Creds, check)
	if err != nil {
		return err
	}
	n, convErr := strconv.Atoi(strings.TrimSpace(out))
	if convErr != nil {
		return verifyFailed(fmt.Sprintf("restore check returned %q, expected a number", strings.TrimSpace(out)), "check the kind's restore_check query")
	}
	if n <= 0 {
		return verifyEmpty(fmt.Sprintf("restored %s database has no tables", t.Service))
	}
	if p.X.Verify != nil && p.X.Verify.Service == t.Service && p.X.Verify.SQL != "" {
		out, err := r.psql(ctx, name, sd.Creds, p.X.Verify.SQL)
		if err != nil {
			return verifyFailed("x-cvctl.verify.sql failed on the restored database: "+err.Error(), "fix the verify query or investigate the backup")
		}
		if first, ok := firstValue(out); !ok || falsy(first) {
			return verifyFailed(fmt.Sprintf("x-cvctl.verify.sql returned %s on the restored database; it must return a row whose first column is not empty, 0, false or null", describe(out)),
				"the restored data is missing what the verify query checks; investigate the backup or fix the query")
		}
	}
	return nil
}

// firstValue is the first column of the first row of psql -tA output.
func firstValue(out string) (string, bool) {
	if strings.TrimSpace(out) == "" {
		return "", false
	}
	row, _, _ := strings.Cut(strings.TrimLeft(out, "\n"), "\n")
	col, _, _ := strings.Cut(row, "|")
	return strings.TrimSpace(col), true
}

func falsy(v string) bool {
	switch strings.ToLower(v) {
	case "", "0", "f", "false", "null":
		return true
	}
	return false
}

// describe reports only the (truncated) first value, not the whole result.
func describe(out string) string {
	if v, ok := firstValue(out); ok {
		return fmt.Sprintf("a first value of %q", truncate(v, 32))
	}
	return "no rows"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func (r *Runner) psql(ctx context.Context, name string, c kinds.Credentials, sql string) (string, error) {
	cmd := r.docker("exec", name, "psql", "-U", c.User, "-d", c.DB, "-v", "ON_ERROR_STOP=1", "-tA", "-c", sql)
	res, err := r.exec(ctx, cmd)
	if err != nil {
		return "", err
	}
	return string(res.Stdout), nil
}

// waitRecovered waits for the restored cluster to accept connections and to
// finish WAL replay (promotion).
func (r *Runner) waitRecovered(ctx context.Context, name, user, db string) error {
	deadline := verifyReadyTimeout
	var waited time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return verifyFailed("verification cancelled: "+err.Error(), "retry cvctl backup verify")
		}
		running, _ := r.Run.Run(ctx, r.docker("inspect", "-f", "{{.State.Running}}", name))
		if running.ExitCode == 0 && strings.TrimSpace(string(running.Stdout)) == "false" {
			logs, _ := r.Run.Run(ctx, r.docker("logs", "--tail", "20", name))
			return verifyFailed("verify container exited before postgres was ready: "+r.redact(strings.TrimSpace(string(logs.Stderr)+string(logs.Stdout))),
				"the latest backup could not be fetched or replayed; check backup-list and the storage")
		}
		if res, err := r.Run.Run(ctx, r.docker("exec", name, "pg_isready", "-U", user, "-d", db)); err == nil && res.ExitCode == 0 {
			q, err := r.Run.Run(ctx, r.docker("exec", name, "psql", "-U", user, "-d", db, "-tA", "-c", "select pg_is_in_recovery()"))
			if err == nil && q.ExitCode == 0 && strings.TrimSpace(string(q.Stdout)) == "f" {
				return nil
			}
		}
		if waited >= deadline {
			return verifyFailed(fmt.Sprintf("restored postgres was not ready after %s", deadline), "the base backup or its WAL is probably incomplete; check backup-list")
		}
		r.sleep(verifyPollInterval)
		waited += verifyPollInterval
	}
}

func (r *Runner) resticVerify(ctx context.Context, inst string, t Target) error {
	envFile, err := r.writeResticEnv(inst, t.Service, r.Host.ResticRepoIndex(t.Service))
	if err != nil {
		return err
	}
	net, err := r.backupNet(ctx, inst)
	if err != nil {
		return err
	}
	name := r.tempName("verify", inst, t.Service)
	defer r.cleanup(ctx, name, "")
	cmd := r.docker("run", "--name", name, "--network", net, "--env-file", envFile,
		"--entrypoint", "sh", resticImage, "-c", resticVerifyScript)
	res, err := r.exec(ctx, cmd)
	if err != nil {
		return err
	}
	n, convErr := strconv.Atoi(lastLine(string(res.Stdout)))
	if convErr != nil {
		return verifyFailed("cannot count restored files", "run restic restore latest manually to inspect the snapshot")
	}
	if n <= 0 {
		return verifyEmpty(fmt.Sprintf("restored %s snapshot has no files", t.Service))
	}
	return nil
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// recordVerify mirrors record: a failure notifies only when its message
// differs from the previously recorded one.
func (r *Runner) recordVerify(ctx context.Context, inst, svc string, res Result) error {
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
	prev := bs.LastVerifyError
	if res.OK {
		bs.LastVerifyOK = r.now()
		bs.LastVerifyError = ""
	} else {
		bs.LastVerifyError = res.Error
	}
	st.Backups[svc] = bs
	if err := r.Store.Save(st); err != nil {
		return err
	}
	if !res.OK && prev != res.Error && r.Notify != nil {
		_ = r.Notify.Send(ctx, notify.Message{
			Host:  r.HostName,
			Level: "error",
			Title: fmt.Sprintf("%s restore verification failed (%s)", inst, svc),
			Text:  res.Error,
		})
	}
	return nil
}
