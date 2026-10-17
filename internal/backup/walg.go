package backup

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/codeavers/cvctl/internal/output"
)

// The container already carries backup-<svc>.env as env_file, so no
// credential is passed here.
func (r *Runner) walgBackup(ctx context.Context, inst string, t Target) error {
	p := r.Compose(inst)
	if _, err := r.exec(ctx, p.Cmd("exec", "-T", "-u", "postgres", t.Service, "wal-g", "backup-push", pgData)); err != nil {
		return err
	}
	_, err := r.exec(ctx, p.Cmd("exec", "-T", "-u", "postgres", t.Service, "wal-g", "delete", "retain", "FULL", walgRetain, "--confirm"))
	return err
}

func (r *Runner) walgList(ctx context.Context, inst string, t Target) ([]Snapshot, error) {
	p := r.Compose(inst)
	res, err := r.exec(ctx, p.Cmd("exec", "-T", "-u", "postgres", t.Service, "wal-g", "backup-list", "--json"))
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Name string `json:"backup_name"`
		Time string `json:"time"`
	}
	if len(res.Stdout) > 0 {
		if err := json.Unmarshal(res.Stdout, &raw); err != nil {
			return nil, &output.Error{
				Code:    "E_BACKUP_PARSE",
				Message: fmt.Sprintf("cannot parse wal-g backup-list output for %s: %v", t.Service, err),
				Hint:    "run wal-g backup-list --json inside the container to inspect the output",
			}
		}
	}
	out := make([]Snapshot, 0, len(raw))
	for _, b := range raw {
		out = append(out, Snapshot{Service: t.Service, Strategy: StrategyWalG, ID: b.Name, Time: b.Time})
	}
	return out, nil
}
