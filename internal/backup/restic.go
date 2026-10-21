package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/secrets"
	"github.com/codeavers/cvctl/internal/sys"
)

// resticExitNoRepo is restic's exit code for a repository that does not exist.
const resticExitNoRepo = 10

// backupNet makes sure the instance's backup network exists (internal with the
// host silo, with egress for external storage): backup
// containers run from the host CLI, before or without a compose up.
func (r *Runner) backupNet(ctx context.Context, inst string) (string, error) {
	name := render.NetworkBackup(inst)
	if err := sys.EnsureNetwork(ctx, r.Run, name, r.Host != nil && r.Host.Local); err != nil {
		return "", err
	}
	return name, nil
}

func (r *Runner) resticEnvPath(inst, svc string) string {
	return filepath.Join(r.instanceDir(inst), "restic-"+svc+".env")
}

// writeResticEnv keeps the secrets out of argv: docker reads them from the file.
func (r *Runner) writeResticEnv(inst, svc string, idx int) (string, error) {
	env := r.Host.ResticEnv(inst, svc, idx)
	if env["RESTIC_PASSWORD"] == "" {
		return "", &output.Error{
			Code:    "E_BACKUP_NOT_CONFIGURED",
			Message: "restic_password is not set",
			Hint:    "create /etc/cvctl/secrets/restic_password (run cvctl init)",
		}
	}
	body := envFileBody(env)
	path := r.resticEnvPath(inst, svc)
	if err := r.FS.WriteFile(path, body, 0o640, deployOwner); err != nil {
		return "", &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: fmt.Sprintf("cannot write %s: %v", path, err),
			Hint:    "check permissions on the instance directory",
		}
	}
	return path, nil
}

func (r *Runner) resticCmd(inst, envFile, volume string, args ...string) sys.Cmd {
	a := []string{"run", "--rm", "--network", render.NetworkBackup(inst)}
	if volume != "" {
		a = append(a, "-v", volume+":/data:ro")
	}
	a = append(a, "--env-file", envFile, resticImage)
	return sys.Cmd{Name: "docker", Args: append(a, args...), Env: []string{dockerConfig}}
}

func (r *Runner) resticStdinCmd(inst, envFile string, stdin []byte, args ...string) sys.Cmd {
	a := []string{"run", "-i", "--rm", "--network", render.NetworkBackup(inst), "--env-file", envFile, resticImage}
	return sys.Cmd{Name: "docker", Args: append(a, args...), Stdin: stdin, Env: []string{dockerConfig}}
}

func (r *Runner) resticBackup(ctx context.Context, inst string, t Target) error {
	envFile, err := r.writeResticEnv(inst, t.Service, r.Host.ResticRepoIndex(t.Service))
	if err != nil {
		return err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return err
	}
	if err := r.resticInit(ctx, inst, envFile); err != nil {
		return err
	}
	if _, err := r.exec(ctx, r.resticCmd(inst, envFile, t.Volume, "backup", "/data", "--host", r.Host.Host, "--tag", inst)); err != nil {
		return err
	}
	_, err = r.exec(ctx, r.resticCmd(inst, envFile, "", "forget", "--keep-daily", "7", "--keep-weekly", "4", "--prune"))
	return err
}

func (r *Runner) resticInit(ctx context.Context, inst, envFile string) error {
	cmd := r.resticCmd(inst, envFile, "", "snapshots", "--json")
	res, err := r.Run.Run(ctx, cmd)
	if err == nil && res.ExitCode == 0 {
		return nil
	}
	if err != nil || res.ExitCode != resticExitNoRepo {
		return r.cmdFailed(cmd, res, err)
	}
	_, err = r.exec(ctx, r.resticCmd(inst, envFile, "", "init"))
	return err
}

// resticList lists the snapshots of the current repository and, after a move
// to per-instance keys, of the older ones, tagged with their repository.
func (r *Runner) resticList(ctx context.Context, inst string, t Target) ([]Snapshot, error) {
	cur := r.Host.ResticRepoIndex(t.Service)
	var out []Snapshot
	for i := cur; i >= 0; i-- {
		snaps, err := r.resticRepoList(ctx, inst, t, i, cur > 0)
		if err != nil {
			return nil, err
		}
		if i < cur {
			for k := range snaps {
				snaps[k].Repo = fmt.Sprintf("r%d", i)
			}
		}
		out = append(out, snaps...)
	}
	if out == nil {
		out = []Snapshot{}
	}
	return out, nil
}

// resticRepoList lists repository idx; a missing repository is empty when
// missingOK (a moved service's new repository before its first backup).
func (r *Runner) resticRepoList(ctx context.Context, inst string, t Target, idx int, missingOK bool) ([]Snapshot, error) {
	envFile, err := r.writeResticEnv(inst, t.Service, idx)
	if err != nil {
		return nil, err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return nil, err
	}
	cmd := r.resticCmd(inst, envFile, "", "snapshots", "--json")
	res, err := r.Run.Run(ctx, cmd)
	if err == nil && res.ExitCode == resticExitNoRepo && missingOK {
		return nil, nil
	}
	if err != nil || res.ExitCode != 0 {
		return nil, r.cmdFailed(cmd, res, err)
	}
	var raw []struct {
		ShortID string `json:"short_id"`
		ID      string `json:"id"`
		Time    string `json:"time"`
	}
	if len(res.Stdout) > 0 {
		if err := json.Unmarshal(res.Stdout, &raw); err != nil {
			return nil, &output.Error{
				Code:    "E_BACKUP_PARSE",
				Message: fmt.Sprintf("cannot parse restic snapshots output for %s: %v", t.Service, err),
				Hint:    "run restic snapshots --json against the repository to inspect the output",
			}
		}
	}
	out := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		id := s.ShortID
		if id == "" {
			id = s.ID
		}
		out = append(out, Snapshot{Service: t.Service, Strategy: StrategyVolume, ID: id, Time: s.Time})
	}
	return out, nil
}

func (r *Runner) envBackup(ctx context.Context, inst string) error {
	entries, warns, err := secrets.CollectInstance(r.FS, r.secretsRoot(), inst)
	if err != nil {
		return &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: r.redact(err.Error()),
			Hint:    "check permissions and files in the instance directory",
		}
	}
	if len(warns) > 0 {
		return &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: r.redact(strings.Join(warns, "; ")),
			Hint:    "check permissions and files in the instance directory",
		}
	}
	if len(entries) == 0 {
		return &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: fmt.Sprintf("no env files found for %s", inst),
			Hint:    "check that the instance directory exists and contains cvctl.env",
		}
	}
	tarBytes, err := secrets.Pack(entries)
	if err != nil {
		return &output.Error{
			Code:    "E_BACKUP_FAILED",
			Message: r.redact(err.Error()),
			Hint:    "check permissions and files in the instance directory",
		}
	}
	envFile, err := r.writeResticEnv(inst, "env", 0)
	if err != nil {
		return err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return err
	}
	if _, err := provisionUser(ctx, r.Run, r.Host, inst); err != nil {
		return err
	}
	if err := r.resticInit(ctx, inst, envFile); err != nil {
		return err
	}
	backupCmd := r.resticStdinCmd(inst, envFile, tarBytes, "backup", "--stdin", "--stdin-filename", "env.tar", "--host", r.Host.Host, "--tag", inst)
	if _, err := r.exec(ctx, backupCmd); err != nil {
		return err
	}
	_, err = r.exec(ctx, r.resticCmd(inst, envFile, "", "forget", "--keep-daily", "7", "--keep-weekly", "4", "--prune"))
	return err
}

func (r *Runner) envList(ctx context.Context, inst string) ([]Snapshot, error) {
	envFile, err := r.writeResticEnv(inst, "env", 0)
	if err != nil {
		return nil, err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return nil, err
	}
	if _, err := provisionUser(ctx, r.Run, r.Host, inst); err != nil {
		return nil, err
	}
	cmd := r.resticCmd(inst, envFile, "", "snapshots", "--json")
	res, err := r.Run.Run(ctx, cmd)
	if err == nil && res.ExitCode == resticExitNoRepo {
		return nil, nil
	}
	if err != nil || res.ExitCode != 0 {
		return nil, r.cmdFailed(cmd, res, err)
	}
	var raw []struct {
		ShortID string `json:"short_id"`
		ID      string `json:"id"`
		Time    string `json:"time"`
	}
	if len(res.Stdout) > 0 {
		if err := json.Unmarshal(res.Stdout, &raw); err != nil {
			return nil, &output.Error{
				Code:    "E_BACKUP_PARSE",
				Message: fmt.Sprintf("cannot parse restic snapshots output for env: %v", err),
				Hint:    "run restic snapshots --json against the repository to inspect the output",
			}
		}
	}
	out := make([]Snapshot, 0, len(raw))
	for _, s := range raw {
		id := s.ShortID
		if id == "" {
			id = s.ID
		}
		out = append(out, Snapshot{Service: "env", Strategy: StrategyEnv, ID: id, Time: s.Time})
	}
	return out, nil
}

// EnvDump unpacks the env tar archive from snapshot of inst.
func (r *Runner) EnvDump(ctx context.Context, inst, snapshot string) ([]secrets.Entry, string, error) {
	if r.Host == nil {
		return nil, "", notConfigured()
	}
	envFile, err := r.writeResticEnv(inst, "env", 0)
	if err != nil {
		return nil, "", err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return nil, "", err
	}
	if _, err := provisionUser(ctx, r.Run, r.Host, inst); err != nil {
		return nil, "", err
	}
	snapArg := "latest"
	if snapshot != "" && snapshot != "latest" {
		snapArg = snapshot
	}
	cmd := r.resticCmd(inst, envFile, "", "snapshots", "--json", snapArg)
	res, err := r.Run.Run(ctx, cmd)
	if err == nil && res.ExitCode == resticExitNoRepo {
		return nil, "", &output.Error{
			Code:    "E_BACKUP_NO_ENV",
			Message: fmt.Sprintf("no env backup found for %s", inst),
			Hint:    "run cvctl backup now " + inst + " env",
		}
	}
	if err != nil || res.ExitCode != 0 {
		if res.ExitCode == resticExitNoRepo || strings.Contains(string(res.Stderr), "does not exist") || strings.Contains(string(res.Stderr), "no matching ID") {
			return nil, "", &output.Error{
				Code:    "E_BACKUP_NO_ENV",
				Message: fmt.Sprintf("no env backup found for %s", inst),
				Hint:    "run cvctl backup now " + inst + " env",
			}
		}
		return nil, "", r.cmdFailed(cmd, res, err)
	}
	var raw []struct {
		ShortID string `json:"short_id"`
		ID      string `json:"id"`
		Time    string `json:"time"`
	}
	if len(res.Stdout) > 0 {
		if err := json.Unmarshal(res.Stdout, &raw); err != nil {
			return nil, "", &output.Error{
				Code:    "E_BACKUP_PARSE",
				Message: fmt.Sprintf("cannot parse restic snapshots output for env: %v", err),
				Hint:    "run restic snapshots --json against the repository to inspect the output",
			}
		}
	}
	if len(raw) == 0 {
		return nil, "", &output.Error{
			Code:    "E_BACKUP_NO_ENV",
			Message: fmt.Sprintf("no env backup found for %s", inst),
			Hint:    "run cvctl backup now " + inst + " env",
		}
	}
	resolved := raw[len(raw)-1].ShortID
	if resolved == "" {
		resolved = raw[len(raw)-1].ID
	}
	if len(resolved) > 8 {
		resolved = resolved[:8]
	}
	dumpCmd := r.resticCmd(inst, envFile, "", "dump", resolved, "/env.tar")
	dumpRes, err := r.Run.Run(ctx, dumpCmd)
	if err != nil || dumpRes.ExitCode != 0 {
		return nil, "", r.cmdFailed(dumpCmd, dumpRes, err)
	}
	entries, err := secrets.Unpack(dumpRes.Stdout)
	if err != nil {
		return nil, "", err
	}
	prefix := "/srv/deploy/" + inst + "/"
	for _, e := range entries {
		if !strings.HasPrefix(e.Path, prefix) {
			return nil, "", &output.Error{
				Code:    "E_BACKUP_ENV_FOREIGN",
				Message: fmt.Sprintf("env backup entry %q is not under %s", e.Path, prefix),
				Hint:    "the backup contains files from a different instance",
			}
		}
	}
	return entries, resolved, nil
}
