package backup

import (
	"context"
	"fmt"
	"strings"

	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/render"
	"github.com/codeavers/cvctl/internal/s3prov"
	"github.com/codeavers/cvctl/internal/sys"
)

const (
	siloProject = "cvctl-storage"
	siloService = "s3"
	siloAlias   = "cvctl-storage"
)

// BackupUser is the silo user of an instance; BackupPolicy its policy.
func BackupUser(inst string) string   { return "bk-" + inst }
func BackupPolicy(inst string) string { return "cvctl-bk-" + inst }

// ProvisionUser creates or updates the instance's backup user in the host silo
// and attaches the silo to the instance's backup network. It does nothing
// unless t is a per-instance target on the local silo. The backup network must
// exist already.
func ProvisionUser(ctx context.Context, run sys.Runner, t *render.BackupTarget, inst string) error {
	_, err := provisionUser(ctx, run, t, inst)
	return err
}

// provisionUser also reports whether the silo was found and only the user
// step failed.
func provisionUser(ctx context.Context, run sys.Runner, t *render.BackupTarget, inst string) (bool, error) {
	if t == nil || !t.Local || t.Instance == "" {
		return false, nil
	}
	if run == nil {
		return false, provisionErr(inst, "no command runner")
	}
	ps := sys.Cmd{Name: "docker", Args: []string{
		"ps",
		"--filter", "label=com.docker.compose.project=" + siloProject,
		"--filter", "label=com.docker.compose.service=" + siloService,
		"--format", "{{.ID}}",
	}, Env: []string{dockerConfig}}
	res, err := run.Run(ctx, ps)
	if err != nil || res.ExitCode != 0 {
		return false, provisionErr(inst, "cannot list the storage container: "+cmdDetail(res, err))
	}
	ids := strings.Fields(string(res.Stdout))
	if len(ids) == 0 {
		return false, provisionErr(inst, "the storage container is not running")
	}
	id := ids[0]
	script := s3prov.BackupUserScript(t.AccessKey, t.SecretKey, t.Bucket, t.UserPrefix(inst), BackupPolicy(inst))
	res, err = run.Run(ctx, sys.Cmd{Name: "docker", Args: []string{"exec", "-i", id, "sh", "-s"}, Stdin: []byte(script), Env: []string{dockerConfig}})
	if err != nil || res.ExitCode != 0 {
		detail := cmdDetail(res, err)
		for _, s := range []string{t.SecretKey, t.AccessKey} {
			if s != "" {
				detail = strings.ReplaceAll(detail, s, "[REDACTED]")
			}
		}
		return true, provisionErr(inst, "creating the backup user failed: "+detail)
	}
	if err := sys.ConnectNetwork(ctx, run, render.NetworkBackup(inst), id, siloAlias); err != nil {
		return false, provisionErr(inst, err.Error())
	}
	return false, nil
}

func cmdDetail(res sys.Result, err error) string {
	if err != nil {
		return err.Error()
	}
	d := strings.TrimSpace(string(res.Stderr))
	if len(d) > maxErrBytes {
		d = d[len(d)-maxErrBytes:]
	}
	if d == "" {
		d = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return d
}

func provisionErr(inst, msg string) error {
	return &output.Error{
		Code:    "E_BACKUP_PROVISION",
		Message: inst + ": " + msg,
		Hint:    "check the storage silo (cvctl init repairs it), then deploy the instance: cvctl deploy " + inst,
	}
}
