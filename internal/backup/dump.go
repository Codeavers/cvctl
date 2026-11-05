package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/sys"
)

// tagPartial marks a dump snapshot not yet known to be complete.
const tagPartial = "partial"

// forgetTimeout bounds removing a partial snapshot: --prune rewrites pack
// files, which takes far longer on a large repository than cleanupTimeout.
const forgetTimeout = 30 * time.Minute

func (r *Runner) dumpBackup(ctx context.Context, inst string, t Target) error {
	return r.dump(ctx, inst, t)
}

// dump streams the service's dump into restic without touching the host disk.
// restic stores whatever arrived by EOF, so a killed producer looks complete.
// Every snapshot therefore starts tagged partial and loses the tag only once
// both sides exited 0: a crash at any point leaves one nothing will use.
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
	for _, tag := range append(tags, tagPartial) {
		args = append(args, "--tag", tag)
	}
	name := r.tempName("dump", inst, t.Service)
	to := r.docker(append([]string{"run", "-i", "--rm", "--name", name, "--network", render.NetworkBackup(inst),
		"--env-file", envFile, resticImage}, append(args, "--json", "--quiet")...)...)

	res, pipeErr := r.Pipe.Pipe(ctx, from, to)
	if ctx.Err() != nil {
		// Killing the docker client leaves the container running.
		r.cleanup(ctx, name, "")
	}
	id := resticSnapshotID(res.To.Stdout)
	cause := r.pipeFailure(from, to, res, pipeErr)
	if cause == nil && id == "" {
		cause = &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: fmt.Sprintf("restic reported no snapshot id for %s", t.Service),
			Hint:    "run the backup again; without the id the snapshot stays partial and unused",
		}
	}
	if cause == nil {
		_, cause = r.exec(ctx, r.resticCmd(inst, envFile, "", "tag", "--remove", tagPartial, id))
	}
	if cause != nil {
		if id != "" {
			return r.forgetPartial(ctx, inst, envFile, id, cause)
		}
		return cause
	}
	// Leftovers go first: retention would count one as the day's snapshot and
	// drop the complete one beside it.
	if _, err := r.exec(ctx, r.resticCmd(inst, envFile, "", "forget", "--tag", tagPartial, "--unsafe-allow-remove-all")); err != nil {
		return err
	}
	_, err = r.exec(ctx, r.resticCmd(inst, envFile, "", "forget", "--keep-daily", "7", "--keep-weekly", "4", "--prune"))
	return err
}

// completeDump resolves snap ("latest" or an id prefix) to a complete snapshot
// and the repository holding it, newest repository first. restic's own latest
// cannot exclude a tag.
func (r *Runner) completeDump(ctx context.Context, inst, svc, snap string) (int, string, error) {
	t := Target{Service: svc, Strategy: StrategyDump}
	for i := r.Host.ResticRepoIndex(svc); i >= 0; i-- {
		snaps, err := r.resticRepoList(ctx, inst, t, i, true)
		if err != nil {
			return 0, "", err
		}
		for _, s := range slices.Backward(snaps) {
			if snap == "latest" || strings.HasPrefix(s.ID, snap) || strings.HasPrefix(snap, s.ID) {
				return i, s.ID, nil
			}
		}
	}
	return 0, "", &output.Error{
		Code:    "E_BACKUP_NO_SNAPSHOT",
		Message: fmt.Sprintf("no complete snapshot of %s matches %s", svc, snap),
		Hint:    "run cvctl backup list <instance> to see the complete snapshots",
	}
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
			Hint:    "it stays tagged partial, so nothing uses it; the next good backup removes it",
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
