package project

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/deploy"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/notify"
	"github.com/codeavers/cvctl/internal/output"
	"github.com/codeavers/cvctl/internal/state"
)

// backupExtService is the --ext key of the instance's own backup bucket.
const backupExtService = "backup"

var backupExtFields = []string{"access_key", "bucket", "endpoint", "region", "secret_key"}

// takeBackupExt moves --ext backup.* out of ext into d.Backup.Ext. Only an
// s3 host takes it, and only while the instance has nothing backed up yet
// (canSet): later, existing backups would be orphaned in the old bucket.
func takeBackupExt(d *instance.Decisions, ext map[string]map[string]string, storage string, canSet bool) (map[string]map[string]string, error) {
	fields, ok := ext[backupExtService]
	if !ok {
		return ext, nil
	}
	if storage != "s3" {
		return nil, &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_EXT",
			Message: "--ext backup.* needs off-site storage (CVCTL_STORAGE=s3); the local silo already gives each instance its own user",
			Hint:    "remove --ext backup.*",
		}}
	}
	if !canSet {
		return nil, &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_EXT",
			Message: "the backup bucket is set at project add; changing it would orphan existing backups",
			Hint:    "remove --ext backup.*",
		}}
	}
	names := make([]string, 0, len(fields))
	for f := range fields {
		names = append(names, f)
	}
	sort.Strings(names)
	for _, f := range names {
		if !slices.Contains(backupExtFields, strings.ToLower(f)) {
			return nil, &output.UsageError{Err: &output.Error{
				Code:    "E_USAGE_EXT",
				Message: fmt.Sprintf("unknown external field %q", backupExtService+"."+f),
				Hint:    "valid fields for backup: " + strings.Join(backupExtFields, ", "),
			}}
		}
	}
	if d.Backup.Ext == nil {
		d.Backup.Ext = map[string]string{}
	}
	for f, v := range fields {
		d.Backup.Ext[strings.ToLower(f)] = v
	}
	if d.Backup.Ext["access_key"] == "" || d.Backup.Ext["secret_key"] == "" {
		return nil, &output.UsageError{Err: &output.Error{
			Code:    "E_USAGE_EXT",
			Message: "backup.access_key and backup.secret_key must both be set",
			Hint:    "pass --ext backup.access_key=<key> --ext backup.secret_key=<secret> (and backup.bucket, backup.endpoint, backup.region when they differ from the host's)",
		}}
	}
	rest := make(map[string]map[string]string, len(ext)-1)
	for k, v := range ext {
		if k != backupExtService {
			rest[k] = v
		}
	}
	return rest, nil
}

// ensureBackupKeys fills the instance's missing backup identity and keys. It
// reports whether the encryption keys were new.
func ensureBackupKeys(d *instance.Decisions) (bool, error) {
	b := &d.Backup
	if b.AccessKey == "" {
		b.AccessKey = backup.BackupUser(d.Instance)
	}
	if b.SecretKey == "" {
		s, err := randomAlnum(32)
		if err != nil {
			return false, err
		}
		b.SecretKey = s
	}
	if b.HasKeys() {
		return false, nil
	}
	if b.LibsodiumKey == "" {
		k := make([]byte, 32)
		if _, err := rand.Read(k); err != nil {
			return false, err
		}
		b.LibsodiumKey = hex.EncodeToString(k)
	}
	if b.ResticPassword == "" {
		s, err := randomAlnum(40)
		if err != nil {
			return false, err
		}
		b.ResticPassword = s
	}
	return true, nil
}

const keysCreatedWarning = "backup keys created; re-run cvctl secrets export so a rebuilt host can read the new backups"

// afterKeyMigration backs up every moved service once the deploy is up, so
// the new generation (restic: repository) holds a full backup, and warns that
// the secrets bundle lacks the new keys.
func afterKeyMigration(ctx context.Context, e *Env, d *instance.Decisions, p *contract.Project, res deploy.Result) []string {
	warnings := []string{keysCreatedWarning}
	if res.Status == state.StatusOK {
		for _, t := range backup.Targets(p, d) {
			if d.Services[t.Service].BackupKeyGen == 0 {
				continue
			}
			var err error
			if e.Data == nil {
				err = errors.New("backups are not wired")
			} else {
				err = e.Data.BackupService(ctx, d.Instance, t.Service)
			}
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("backup of %s into the new key generation failed: %v; run cvctl backup now %s %s", t.Service, err, d.Instance, t.Service))
			}
		}
	} else {
		for _, t := range backup.Targets(p, d) {
			if d.Services[t.Service].BackupKeyGen > 0 {
				warnings = append(warnings, fmt.Sprintf("%s moved to a new key generation; once the deploy succeeds, run cvctl backup now %s %s", t.Service, d.Instance, t.Service))
			}
		}
	}
	if e.Deployer != nil && e.Deployer.Notify != nil {
		host := ""
		if e.Host != nil {
			host = e.Host.HostName
		}
		_ = e.Deployer.Notify.Send(ctx, notify.Message{
			Host: host, Level: "warn", Title: d.Instance + ": backup keys created", Text: strings.Join(warnings, "\n"),
		})
	}
	return warnings
}

// migrateBackupKeyGen moves every backed-up local service of an instance that
// just got its own keys past what the host keys encrypted: WAL-G services to
// a new generation, restic services to repository r1. usedGen is the highest
// generation known to hold WAL-G data per service, when the caller knows it.
func migrateBackupKeyGen(d *instance.Decisions, p *contract.Project, usedGen map[string]int) {
	for _, t := range backup.Targets(p, d) {
		sd := d.Services[t.Service]
		if t.Strategy == backup.StrategyWalG {
			next := max(sd.BackupGen, sd.BackupGenMax, usedGen[t.Service]) + 1
			sd.BackupGen, sd.BackupGenMax, sd.BackupKeyGen = next, next, next
		} else {
			sd.BackupKeyGen = 1
		}
		d.Services[t.Service] = sd
	}
}
