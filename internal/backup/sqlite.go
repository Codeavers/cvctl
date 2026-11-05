package backup

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/sys"
)

// CodeSqliteUID reports an app whose user cvctl cannot give the volume to. Only
// the contract or the image can fix it, so a retry of the same state is useless.
const CodeSqliteUID = "E_SQLITE_UID"

// Every helper sees the volume at /data, whatever path the app mounts it on.

// sqliteProbeScript says, without opening the database, what the dump needs:
// whose file it is, since with no capabilities only the owner can read a 0600
// file, and whether a read-only mount is enough. It is not in WAL mode (byte 18
// of the header is 2), where readers share the -shm file with the app, nor with
// a journal left to roll back. A header that root without capabilities cannot
// read counts as WAL. ls fails on a directory it cannot list, which would
// otherwise pass for a missing file. An empty file holds no data yet: SQLite
// creates it on open, before the first table.
const sqliteProbeScript = `set -e
cd /data
ls . >/dev/null
if [ ! -s app.db ]; then
  echo absent
  exit 0
fi
uid=$(stat -c %u app.db)
format=$(od -An -tu1 -j18 -N1 app.db 2>/dev/null | tr -d ' ' || true)
if [ "$format" = 1 ] && [ ! -s app.db-journal ]; then
  echo "$uid ro"
else
  echo "$uid rw"
fi
`

// sqliteDumpScript copies through the backup API and only then streams: stdout
// must be nothing but the finished copy. The open read transaction pins one
// snapshot: without it .backup starts over whenever the app commits between two
// steps, and a busy app keeps it from ever finishing. In WAL mode the app does
// not notice; in rollback mode its commits wait for the copy.
const sqliteDumpScript = `set -e
sqlite3 -batch /data/app.db ".timeout 60000" "BEGIN" "SELECT count(*) FROM sqlite_master" ".backup /tmp/cvctl-copy.db" >/dev/null
cat /tmp/cvctl-copy.db
`

// sqliteLoadScript refuses an empty stream: an empty file passes integrity_check.
const sqliteLoadScript = `set -e
cat > /data/app.db.part
[ -s /data/app.db.part ] || { echo "the snapshot is empty" >&2; exit 1; }
mv /data/app.db.part /data/app.db
`

const sqliteCheckScript = `sqlite3 -batch /data/app.db 'PRAGMA integrity_check'`

const sqliteOwnerScript = `stat -c %u /data`

// sqliteChownScript leaves the root for last: its owner is what marks the volume
// as prepared, so an interrupted run is repeated. The check does not rely on
// find passing on the exit code of a batched -exec.
const sqliteChownScript = `set -e
find /data -depth -exec chown -h "$OWNER_UID" {} +
[ "$(stat -c %u /data)" = "$OWNER_UID" ]
`

// errNoData marks a target with nothing to back up yet: not a failure, and no
// snapshot, since an empty one would count as a complete backup.
var errNoData = errors.New("nothing to back up")

// helperImage is the auxiliary image that takes the dump of t, empty when the
// service takes its own.
func (r *Runner) helperImage(t Target) (string, error) {
	cat, err := r.catalog()
	if err != nil {
		return "", err
	}
	return cat[t.Kind].Helper, nil
}

// helperCmd runs a script in the helper with no network and no capabilities;
// opts are further docker run flags.
func (r *Runner) helperCmd(image, user, mount, script string, opts ...string) sys.Cmd {
	args := append([]string{"run", "--rm"}, opts...)
	return r.docker(append(args, "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true",
		"--user", user, "-v", mount, image, "sh", "-c", script)...)
}

func (r *Runner) helperExec(ctx context.Context, cmd sys.Cmd) (sys.Result, error) {
	res, err := r.Run.Run(ctx, cmd)
	if err != nil || res.ExitCode != 0 {
		e := sys.CmdErr("E_SQLITE_HELPER", cmd, res, err, "check that docker can pull the helper image and that the volume is intact")
		e.Message = r.redact(e.Message)
		return res, e
	}
	return res, nil
}

// sqliteSource is the command that writes a consistent copy of the database to
// stdout, and the name of its container. It runs beside the app, which keeps
// serving, and returns errNoData when there is no database yet. The volume is
// checked first because docker run would create it.
func (r *Runner) sqliteSource(ctx context.Context, inst string, t Target, image string) (sys.Cmd, string, error) {
	noData := fmt.Errorf("%w: %s has no database file yet", errNoData, t.Service)
	exists, err := r.volumeExists(ctx, t.Volume)
	if err != nil {
		return sys.Cmd{}, "", err
	}
	if !exists {
		return sys.Cmd{}, "", noData
	}
	res, err := r.helperExec(ctx, r.helperCmd(image, "0", t.Volume+":/data:ro", sqliteProbeScript))
	if err != nil {
		return sys.Cmd{}, "", err
	}
	f := strings.Fields(string(res.Stdout))
	if len(f) == 1 && f[0] == "absent" {
		return sys.Cmd{}, "", noData
	}
	if len(f) != 2 || !slices.Contains([]string{"ro", "rw"}, f[1]) || !numeric(f[0]) {
		return sys.Cmd{}, "", &output.Error{
			Code:    "E_SQLITE_HELPER",
			Message: fmt.Sprintf("the helper reported %q about %s", truncate(r.redact(strings.TrimSpace(string(res.Stdout))), 64), t.Service),
			Hint:    "check that the helper image matches this cvctl version",
		}
	}
	mount := t.Volume + ":/data"
	if f[1] == "ro" {
		mount += ":ro"
	}
	name := r.tempName("sqlite", inst, t.Service)
	return r.helperCmd(image, f[0], mount, sqliteDumpScript, "--name", name), name, nil
}

func numeric(s string) bool {
	_, err := strconv.ParseUint(s, 10, 32)
	return err == nil
}

// sqliteRestore loads snapshot into the new volume t.Volume and checks it. The
// file stays root's: the app's user is applied by the caller, or by the deploy
// on a rebuilt host, where the app image is not there yet to read it from. Root
// without capabilities can write here only because it owns the empty volume.
func (r *Runner) sqliteRestore(ctx context.Context, inst string, t Target, snapshot, image string) (retErr error) {
	if _, err := r.helperExec(ctx, r.docker("volume", "create", t.Volume)); err != nil {
		return err
	}
	loader := r.tempName("load", inst, t.Service)
	defer func() {
		if retErr != nil {
			r.cleanup(ctx, loader, t.Volume)
		}
	}()
	from, reader, err := r.dumpReader(ctx, inst, t, snapshot)
	if err != nil {
		return err
	}
	to := r.helperCmd(image, "0", t.Volume+":/data", sqliteLoadScript, "-i", "--name", loader)
	if err := r.feed(ctx, from, reader, to); err != nil {
		return err
	}
	res, err := r.helperExec(ctx, r.helperCmd(image, "0", t.Volume+":/data", sqliteCheckScript))
	if err != nil {
		return err
	}
	// integrity_check exits 0 on a corrupt file; only its output tells.
	if out := strings.TrimSpace(string(res.Stdout)); out != "ok" {
		return verifyFailed(fmt.Sprintf("integrity_check on the snapshot of %s returned %q", t.Service, truncate(r.redact(out), 200)),
			"the snapshot is damaged; restore an older one from cvctl backup list")
	}
	return nil
}

func (r *Runner) sqliteVerify(ctx context.Context, inst string, t Target, image string) error {
	fresh := t
	fresh.Volume = r.tempName("verify", inst, t.Service)
	defer r.cleanup(ctx, fresh.Volume, fresh.Volume)
	return r.sqliteRestore(ctx, inst, fresh, "latest", image)
}

// sqliteUID is the user the volume of ds must belong to. digests are those of
// the release being started: the image of another one may run as someone else.
func (r *Runner) sqliteUID(ctx context.Context, p *contract.Project, ds contract.DataService, digests map[string]string) (string, error) {
	if ds.UID != "" {
		return ds.UID, nil
	}
	i := slices.IndexFunc(p.Apps, func(a contract.AppService) bool { return a.Name == ds.Owner })
	if i < 0 {
		return "", uidErr(ds, fmt.Sprintf("owner %q of %s is not an app service", ds.Owner, ds.Name))
	}
	ref := p.Apps[i].Image
	if dg := digests[ds.Owner]; dg != "" {
		ref += "@" + dg
	}
	res, err := r.helperExec(ctx, r.docker("image", "inspect", "--format", "{{.Config.User}}", ref))
	if err != nil {
		return "", err
	}
	raw := strings.TrimSpace(string(res.Stdout))
	// USER may name a group too: 1000:1000.
	user, _, _ := strings.Cut(raw, ":")
	switch {
	case contract.ValidUID(user):
		return user, nil
	case user == "0" || user == "root":
		return "", uidErr(ds, fmt.Sprintf("image of %s runs as root, and apps run unprivileged", ds.Owner))
	}
	return "", uidErr(ds, fmt.Sprintf("image of %s sets USER to %q, not a numeric uid", ds.Owner, raw))
}

func uidErr(ds contract.DataService, msg string) *output.Error {
	return &output.Error{
		Code:    CodeSqliteUID,
		Message: msg,
		Hint:    fmt.Sprintf("set %s on service %s to the numeric uid above 0 the app runs as", contract.LabelUID, ds.Name),
	}
}

// sqliteOwn gives volume to uid. CHOWN is the only capability any helper gets,
// and it is granted only here and only when the owner is not the right one.
func (r *Runner) sqliteOwn(ctx context.Context, image, volume, uid string) error {
	res, err := r.helperExec(ctx, r.helperCmd(image, "0", volume+":/data:ro", sqliteOwnerScript))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(res.Stdout)) == uid {
		return nil
	}
	_, err = r.helperExec(ctx, r.helperCmd(image, "0", volume+":/data", sqliteChownScript, "--cap-add", "CHOWN", "-e", "OWNER_UID="+uid))
	return err
}

// PrepareSqlite gives every local volume of a kind without a container to the
// user of its app. The deploy runs it before anything can open the volume, the
// pre-deploy hooks included, and after the pull: the image says who that user
// is. An error with code CodeSqliteUID will not go away by retrying.
func (r *Runner) PrepareSqlite(ctx context.Context, p *contract.Project, d *instance.Decisions) error {
	r = r.withServiceSecrets(d)
	cat, err := r.catalog()
	if err != nil {
		return err
	}
	for _, ds := range p.Data {
		k := cat[ds.Kind]
		if !k.NoContainer || d.Services[ds.Name].Mode != "local" {
			continue
		}
		uid, err := r.sqliteUID(ctx, p, ds, d.Digests)
		if err != nil {
			return err
		}
		if err := r.sqliteOwn(ctx, k.Helper, render.VolumeName(d, ds.Name), uid); err != nil {
			return err
		}
	}
	return nil
}
