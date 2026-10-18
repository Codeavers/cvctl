package backup

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/sys"
)

// resticExitNoRepo is restic's exit code for a repository that does not exist.
const resticExitNoRepo = 10

// backupNet makes sure the instance's internal backup network exists: backup
// containers run from the host CLI, before or without a compose up.
func (r *Runner) backupNet(ctx context.Context, inst string) (string, error) {
	name := render.NetworkBackup(inst)
	if err := sys.EnsureNetwork(ctx, r.Run, name, true); err != nil {
		return "", err
	}
	return name, nil
}

func (r *Runner) resticEnvPath(inst, svc string) string {
	return filepath.Join(r.instanceDir(inst), "restic-"+svc+".env")
}

// writeResticEnv keeps the secrets out of argv: docker reads them from the file.
func (r *Runner) writeResticEnv(inst, svc string) (string, error) {
	if r.Host.ResticPassword == "" {
		return "", &output.Error{
			Code:    "E_BACKUP_NOT_CONFIGURED",
			Message: "restic_password is not set",
			Hint:    "create /etc/cvctl/secrets/restic_password (run cvctl init)",
		}
	}
	b := r.Host
	body := envFileBody(map[string]string{
		"RESTIC_REPOSITORY":     "s3:" + b.Endpoint + "/" + b.Bucket + "/" + b.Host + "/" + inst + "/" + svc,
		"RESTIC_PASSWORD":       b.ResticPassword,
		"AWS_ACCESS_KEY_ID":     b.AccessKey,
		"AWS_SECRET_ACCESS_KEY": b.SecretKey,
		"AWS_DEFAULT_REGION":    b.Region,
	})
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

func (r *Runner) resticBackup(ctx context.Context, inst string, t Target) error {
	envFile, err := r.writeResticEnv(inst, t.Service)
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

func (r *Runner) resticList(ctx context.Context, inst string, t Target) ([]Snapshot, error) {
	envFile, err := r.writeResticEnv(inst, t.Service)
	if err != nil {
		return nil, err
	}
	if _, err := r.backupNet(ctx, inst); err != nil {
		return nil, err
	}
	res, err := r.exec(ctx, r.resticCmd(inst, envFile, "", "snapshots", "--json"))
	if err != nil {
		return nil, err
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
