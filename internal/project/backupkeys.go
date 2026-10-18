package project

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/codeavers/cvctl/internal/backup"
	"github.com/codeavers/cvctl/internal/contract"
	"github.com/codeavers/cvctl/internal/instance"
	"github.com/codeavers/cvctl/internal/output"
)

// backupExtService is the --ext key of the instance's own backup bucket.
const backupExtService = "backup"

var backupExtFields = []string{"access_key", "bucket", "endpoint", "region", "secret_key"}

// takeBackupExt moves --ext backup.* out of ext into d.Backup.Ext. A data
// service named backup keeps the key, as before.
func takeBackupExt(d *instance.Decisions, p *contract.Project, ext map[string]map[string]string) (map[string]map[string]string, error) {
	fields, ok := ext[backupExtService]
	if !ok {
		return ext, nil
	}
	for _, ds := range p.Data {
		if ds.Name == backupExtService {
			return ext, nil
		}
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
