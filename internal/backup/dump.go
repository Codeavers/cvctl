package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/sys"
)

// forgetTimeout bounds removing a partial snapshot: --prune rewrites pack
// files, which takes far longer on a large repository than cleanupTimeout.
const forgetTimeout = 30 * time.Minute

func (r *Runner) dumpBackup(ctx context.Context, inst string, t Target) error {
	return r.dump(ctx, inst, t)
}

// dump streams the service's dump into restic without touching the host disk.
// restic reads to EOF and stores whatever arrived, so a producer that dies
// mid-way still leaves a snapshot: a failed run must remove it or it becomes
// the latest good backup.
func (r *Runner) dump(ctx context.Context, inst string, t Target, tags ...string) error {
	script, err := r.dumpScript(t)
	if err != nil {
		return err
	}
	envFile, err := r.writeResticEnv(inst, t.Service, r.Host.ResticRepoIndex(t.Service))
	if err != nil {
		return err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return err
	}
	container, err := r.runningContainer(ctx, inst, t.Service)
	if err != nil {
		return err
	}
	if err := r.resticInit(ctx, inst, envFile); err != nil {
		return err
	}
	from := r.docker("exec", container, "sh", "-c", script)
	// --quiet drops the status lines --json would print ten times a second for
	// the whole dump; the summary with the snapshot id is printed regardless.
	args := []string{"backup", "--stdin", "--stdin-filename", t.Service + ".dump", "--host", r.Host.Host, "--tag", inst}
	for _, tag := range tags {
		args = append(args, "--tag", tag)
	}
	to := r.resticStdinCmd(inst, envFile, nil, append(args, "--json", "--quiet")...)

	res, pipeErr := r.Pipe.Pipe(ctx, from, to)
	cause := r.pipeFailure(from, to, res, pipeErr)
	if cause == nil {
		_, err = r.exec(ctx, r.resticCmd(inst, envFile, "", "forget", "--keep-daily", "7", "--keep-weekly", "4", "--prune"))
		return err
	}
	if id := resticSnapshotID(res.To.Stdout); id != "" {
		return r.forgetPartial(ctx, inst, envFile, id, cause)
	}
	if ctx.Err() != nil {
		// Killing the docker client leaves the restic container running, and
		// it stores what it read once its stdin closes.
		return fmt.Errorf("%w; a partial snapshot may remain: check cvctl backup list", cause)
	}
	return cause
}

func (r *Runner) dumpScript(t Target) (string, error) {
	cat, err := r.catalog()
	if err != nil {
		return "", err
	}
	k, ok := cat[t.Kind]
	if !ok || len(k.DumpCommand) == 0 {
		return "", &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: fmt.Sprintf("kind %q of %s has no dump_command", t.Kind, t.Service),
			Hint:    "declare dump_command in the kind definition",
		}
	}
	return strings.Join(k.DumpCommand, " "), nil
}

func (r *Runner) runningContainer(ctx context.Context, inst, svc string) (string, error) {
	containers, err := r.Compose(inst).Containers(ctx, svc)
	if err != nil {
		return "", err
	}
	for _, c := range containers {
		if c.State == "running" {
			return c.ID, nil
		}
	}
	return "", &output.Error{
		Code:    "E_BACKUP_FAILED",
		Message: fmt.Sprintf("service %s of %s is not running", svc, inst),
		Hint:    "start the service and retry the backup",
	}
}

// pipeFailure looks at restic first: when it dies the producer gets SIGPIPE,
// so a failed producer says nothing about the cause unless restic exited 0.
func (r *Runner) pipeFailure(from, to sys.Cmd, res sys.PipeResult, err error) error {
	switch {
	case err != nil:
		return r.cmdFailed(to, res.To, err)
	case res.To.ExitCode != 0:
		return r.cmdFailed(to, res.To, nil)
	case res.From.ExitCode != 0:
		return r.cmdFailed(from, res.From, nil)
	}
	return nil
}

// forgetPartial returns cause once the snapshot is gone. It outlives ctx: the
// run that timed out is the one most likely to have left a partial snapshot.
func (r *Runner) forgetPartial(ctx context.Context, inst, envFile, id string, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), forgetTimeout)
	defer cancel()
	if _, err := r.exec(cctx, r.resticCmd(inst, envFile, "", "forget", id, "--prune")); err != nil {
		return &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: r.redact(fmt.Sprintf("partial snapshot %s could not be removed: %v; the backup failed: %v", id, err, cause)),
			Hint:    "remove it with restic forget <id> --prune before restoring: it would count as the latest backup",
		}
	}
	return cause
}

// resticSnapshotID reads the id from restic's --json output, one object per
// line; it is empty when restic stored no snapshot.
func resticSnapshotID(out []byte) string {
	for line := range strings.Lines(string(out)) {
		var m struct {
			Type string `json:"message_type"`
			ID   string `json:"snapshot_id"`
		}
		if json.Unmarshal([]byte(line), &m) == nil && m.Type == "summary" {
			return m.ID
		}
	}
	return ""
}
