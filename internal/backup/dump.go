package backup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/sys"
	"github.com/codeavers/cvctl/kinds"
)

// tagPartial marks a dump snapshot not yet known to be complete.
const tagPartial = "partial"

// repoLockWait lets a dump outlast a prune of its repository. A restore point
// cannot wait on the data lock instead: it runs under the instance lock, and a
// backup holding the data lock waits for that lock to record its status.
const repoLockWait = "10m"

// forgetTimeout bounds removing a partial snapshot: --prune rewrites pack
// files, which takes far longer on a large repository than cleanupTimeout.
const forgetTimeout = 30 * time.Minute

// engineReadyTimeout bounds the start of an empty engine: no data is loaded
// yet, so a longer wait would only hide a broken image.
const (
	engineReadyTimeout = 5 * time.Minute
	enginePollInterval = 2 * time.Second
)

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
	if err := r.resticInit(ctx, inst, envFile, "--retry-lock", repoLockWait); err != nil {
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
		"--env-file", envFile, resticImage, "--retry-lock", repoLockWait}, append(args, "--json", "--quiet")...)...)

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
		_, cause = r.exec(ctx, r.dumpRestic(inst, envFile, "tag", "--remove", tagPartial, id))
	}
	if cause != nil {
		if id != "" {
			return r.forgetPartial(ctx, inst, envFile, id, cause)
		}
		return cause
	}
	// Leftovers go first: retention would count one as the day's snapshot and
	// drop the complete one beside it.
	if _, err := r.exec(ctx, r.dumpRestic(inst, envFile, "forget", "--tag", tagPartial, "--unsafe-allow-remove-all")); err != nil {
		return err
	}
	// --keep-daily keeps only a day's newest snapshot, so a second deploy would
	// drop the first one's restore point. --keep-within counts back from the
	// newest snapshot, which is the one just taken.
	_, err = r.exec(ctx, r.dumpRestic(inst, envFile, "forget", "--keep-within", strconv.Itoa(walgRetainDays)+"d", "--keep-daily", "7", "--keep-weekly", "4", "--prune"))
	return err
}

func (r *Runner) dumpRestic(inst, envFile string, args ...string) sys.Cmd {
	return r.resticCmd(inst, envFile, "", append([]string{"--retry-lock", repoLockWait}, args...)...)
}

// completeDump resolves snap ("latest", an id prefix or a restore point tag) to
// a complete snapshot and the repository holding it, newest repository first.
// restic's own latest cannot exclude a tag.
func (r *Runner) completeDump(ctx context.Context, inst, svc, snap string) (int, string, error) {
	t := Target{Service: svc, Strategy: StrategyDump}
	for i := r.Host.ResticRepoIndex(svc); i >= 0; i-- {
		snaps, err := r.resticRepoList(ctx, inst, t, i, true)
		if err != nil {
			return 0, "", err
		}
		for _, s := range slices.Backward(snaps) {
			if snap == "latest" || strings.HasPrefix(s.ID, snap) || strings.HasPrefix(snap, s.ID) || slices.Contains(s.Tags, snap) {
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

// pickDump resolves a restore target to a complete snapshot. A timestamp has no
// replay to stop at, so it takes the latest snapshot not newer than it, looking
// at every repository instead of trusting their order.
func (r *Runner) pickDump(ctx context.Context, inst, svc string, tgt restoreTarget) (Snapshot, error) {
	if tgt.kind != "time" {
		snap := tgt.name
		if tgt.kind == "latest" {
			snap = "latest"
		}
		_, id, err := r.completeDump(ctx, inst, svc, snap)
		return Snapshot{ID: id}, err
	}
	var best Snapshot
	var bestAt time.Time
	t := Target{Service: svc, Strategy: StrategyDump}
	for i := r.Host.ResticRepoIndex(svc); i >= 0; i-- {
		snaps, err := r.resticRepoList(ctx, inst, t, i, true)
		if err != nil {
			return Snapshot{}, err
		}
		for _, s := range snaps {
			at, err := time.Parse(time.RFC3339Nano, s.Time)
			if err != nil || at.After(tgt.at) {
				continue
			}
			if best.ID == "" || at.After(bestAt) {
				best, bestAt = s, at
			}
		}
	}
	if best.ID == "" {
		return Snapshot{}, restoreFailedErr("E_RESTORE_NO_BACKUP",
			fmt.Sprintf("no complete snapshot of %s at or before %s", svc, tgt.at.Format(time.RFC3339)),
			"pick a later target; run cvctl backup list <instance> to see the complete snapshots")
	}
	return best, nil
}

// dumpRestore loads snapshot into the new volume t.Volume and leaves it ready
// for the service. The caller checked that the volume did not exist, so a
// failure can remove it without reaching data in use.
func (r *Runner) dumpRestore(ctx context.Context, inst string, t Target, snapshot string) (retErr error) {
	if _, err := r.exec(ctx, r.docker("volume", "create", t.Volume)); err != nil {
		return err
	}
	name := r.tempName("restore", inst, t.Service)
	defer func() {
		if retErr != nil {
			r.cleanup(ctx, name, t.Volume)
			return
		}
		r.cleanup(ctx, name, "")
	}()
	if err := r.dumpLoad(ctx, inst, t, snapshot, name, t.Volume); err != nil {
		return err
	}
	// a clean shutdown leaves the volume ready for the real container
	_, err := r.exec(ctx, r.docker("stop", "-t", restoreStopTimeout, name))
	return err
}

// dumpVerify loads the newest complete snapshot into a throwaway engine and
// runs restore_check there: a shell script, like the kind's other commands,
// whose last output line is a count that must be above zero.
func (r *Runner) dumpVerify(ctx context.Context, inst string, t Target) error {
	cat, err := r.catalog()
	if err != nil {
		return err
	}
	check := cat[t.Kind].RestoreCheck
	if check == "" {
		return verifyFailed(fmt.Sprintf("kind %q of %s has no restore_check", t.Kind, t.Service), "declare restore_check in the kind definition")
	}
	name := r.tempName("verify", inst, t.Service)
	if _, err := r.exec(ctx, r.docker("volume", "create", name)); err != nil {
		return err
	}
	defer r.cleanup(ctx, name, name)
	if err := r.dumpLoad(ctx, inst, t, "latest", name, name); err != nil {
		return err
	}
	res, err := r.exec(ctx, r.docker("exec", name, "sh", "-c", check))
	if err != nil {
		return err
	}
	out := lastLine(string(res.Stdout))
	n, convErr := strconv.Atoi(out)
	if convErr != nil {
		return verifyFailed(fmt.Sprintf("restore check returned %q, expected a number", truncate(out, 32)), "check the kind's restore_check")
	}
	if n <= 0 {
		return verifyEmpty(fmt.Sprintf("restored %s dump holds no data", t.Service))
	}
	return nil
}

// dumpLoad starts a temporary engine called name on volume and streams snapshot
// into it. The caller removes the container and the volume.
func (r *Runner) dumpLoad(ctx context.Context, inst string, t Target, snapshot, name, volume string) error {
	cat, err := r.catalog()
	if err != nil {
		return err
	}
	k := cat[t.Kind]
	if len(k.RestoreCommand) == 0 {
		return restoreFailedErr("E_RESTORE_FAILED", fmt.Sprintf("kind %q of %s has no restore_command", t.Kind, t.Service), "declare restore_command in the kind definition")
	}
	idx, id, err := r.completeDump(ctx, inst, t.Service, snapshot)
	if err != nil {
		return err
	}
	envFile, err := r.writeResticEnv(inst, t.Service, idx)
	if err != nil {
		return err
	}
	if err := r.startEngine(ctx, inst, t, k, name, volume); err != nil {
		return err
	}
	reader := r.tempName("read", inst, t.Service)
	from := r.docker("run", "--rm", "--name", reader, "--network", render.NetworkBackup(inst),
		"--env-file", envFile, resticImage, "--retry-lock", repoLockWait, "dump", id, t.Service+".dump")
	to := r.docker("exec", "-i", name, "sh", "-c", strings.Join(k.RestoreCommand, " "))
	res, pipeErr := r.Pipe.Pipe(ctx, from, to)
	if ctx.Err() != nil {
		// Killing the docker client leaves the container running.
		r.cleanup(ctx, reader, "")
	}
	// A cut stream fails the engine's client too, which hides the real cause.
	if pipeErr == nil && res.From.ExitCode != 0 && res.To.ExitCode != 0 {
		return errors.Join(r.cmdFailed(from, res.From, nil), r.cmdFailed(to, res.To, nil))
	}
	return r.pipeFailure(from, to, res, pipeErr)
}

// startEngine runs the kind's image as the service would, hardened the same way
// and with its entrypoint, so the engine initialises the empty volume. It joins
// no network: only docker exec talks to it, and nothing may reach half-loaded
// data. Credentials go through a file that docker has read once run returns.
func (r *Runner) startEngine(ctx context.Context, inst string, t Target, k kinds.Kind, name, volume string) error {
	p, d, err := r.Load(inst)
	if err != nil {
		return err
	}
	sd := d.Services[t.Service]
	version := sd.Version
	if version == "" {
		for _, ds := range p.Data {
			if ds.Name == t.Service {
				version = ds.Version
			}
		}
	}
	image, err := k.ImageFor(version)
	if err != nil {
		return err
	}
	test, err := engineHealth(k, sd.Creds, t.Service)
	if err != nil {
		return err
	}
	envFile := filepath.Join(r.instanceDir(inst), "engine-"+t.Service+".env")
	if err := r.FS.WriteFile(envFile, envFileBody(k.ContainerEnvFor(sd.Creds)), 0o640, deployOwner); err != nil {
		return restoreFailedErr("E_FILE_WRITE", fmt.Sprintf("cannot write %s: %v", envFile, err), "check permissions and free space of the instance directory")
	}
	defer func() { _ = r.FS.Remove(envFile) }()
	args := []string{"run", "-d", "--name", name, "--network", "none", "--env-file", envFile,
		"--cap-drop", "ALL", "--security-opt", "no-new-privileges:true"}
	for _, c := range k.CapAdd {
		args = append(args, "--cap-add", c)
	}
	args = append(args, "-v", volume+":"+k.VolumePath, image)
	for _, c := range k.Command {
		args = append(args, k.Substitute(c, sd.Creds))
	}
	if _, err := r.exec(ctx, r.docker(args...)); err != nil {
		return err
	}
	return r.waitEngine(ctx, name, test)
}

// engineHealth turns the kind's compose healthcheck into a command for docker
// exec. Without one the engine's readiness is unknown, and a load into an engine
// still starting fails halfway.
func engineHealth(k kinds.Kind, c kinds.Credentials, svc string) ([]string, error) {
	if len(k.Health) > 1 {
		var test []string
		for _, s := range k.Health[1:] {
			test = append(test, k.Substitute(s, c))
		}
		switch k.Health[0] {
		case "CMD":
			return test, nil
		case "CMD-SHELL":
			return []string{"sh", "-c", strings.Join(test, " ")}, nil
		}
	}
	return nil, restoreFailedErr("E_RESTORE_FAILED", fmt.Sprintf("kind %q of %s has no usable health check", k.Name, svc),
		"declare health as [CMD, ...] or [CMD-SHELL, ...] in the kind definition")
}

func (r *Runner) waitEngine(ctx context.Context, name string, test []string) error {
	var waited time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return restoreFailedErr("E_RESTORE_FAILED", "cancelled while the temporary engine started: "+err.Error(), "run the command again")
		}
		running, _ := r.Run.Run(ctx, r.docker("inspect", "-f", "{{.State.Running}}", name))
		if running.ExitCode == 0 && strings.TrimSpace(string(running.Stdout)) == "false" {
			logs, _ := r.Run.Run(ctx, r.docker("logs", "--tail", "20", name))
			return restoreFailedErr("E_RESTORE_FAILED", "temporary engine exited before it was healthy: "+r.redact(strings.TrimSpace(string(logs.Stderr)+string(logs.Stdout))),
				"check that the image of the kind starts on an empty volume")
		}
		if res, err := r.Run.Run(ctx, r.docker(append([]string{"exec", name}, test...)...)); err == nil && res.ExitCode == 0 {
			return nil
		}
		if waited >= engineReadyTimeout {
			return restoreFailedErr("E_RESTORE_FAILED", fmt.Sprintf("temporary engine was not healthy after %s", engineReadyTimeout), "check that the image of the kind starts on an empty volume")
		}
		r.sleep(enginePollInterval)
		waited += enginePollInterval
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
	if _, err := r.exec(cctx, r.dumpRestic(inst, envFile, "forget", id, "--prune")); err != nil {
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
