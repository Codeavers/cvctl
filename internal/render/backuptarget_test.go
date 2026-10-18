package render

import (
	"testing"

	"github.com/codeavers/cvctl/internal/instance"
)

func keyedDecisions() *instance.Decisions {
	return &instance.Decisions{
		Instance: "proj-dev",
		Services: map[string]instance.ServiceDecision{
			"db":    {Mode: "local", BackupGen: 3, BackupGenMax: 3, BackupKeyGen: 3},
			"files": {Mode: "local", BackupKeyGen: 1},
		},
		Backup: instance.BackupDecision{AccessKey: "bk-proj-dev", SecretKey: "INSTSECRET", LibsodiumKey: "INSTLIBSODIUM", ResticPassword: "INSTRESTIC"},
	}
}

func TestBackupTargetForSelectsKeyByGeneration(t *testing.T) {
	host := testBackup()
	host.Local = true
	tg := BackupTargetFor(host, keyedDecisions())
	if tg.Instance != "proj-dev" || tg.AccessKey != "bk-proj-dev" || tg.SecretKey != "INSTSECRET" {
		t.Fatalf("silo target must carry the instance identity: %+v", tg)
	}
	if tg.Endpoint != host.Endpoint || tg.Bucket != host.Bucket || tg.Region != host.Region || tg.Host != host.Host {
		t.Fatalf("endpoint, bucket and region come from the host: %+v", tg)
	}
	for gen, want := range map[int]string{0: "LIBSODIUMSECRET", 2: "LIBSODIUMSECRET", 3: "INSTLIBSODIUM", 4: "INSTLIBSODIUM"} {
		env := tg.Env("proj-dev", "db", gen)
		if env["WALG_LIBSODIUM_KEY"] != want {
			t.Errorf("gen %d key %q, want %q", gen, env["WALG_LIBSODIUM_KEY"], want)
		}
		if env["AWS_ACCESS_KEY_ID"] != "bk-proj-dev" {
			t.Errorf("gen %d uses %q, want the instance user for every generation", gen, env["AWS_ACCESS_KEY_ID"])
		}
	}
	// A service without a key generation uses the instance key from gen 0.
	if k := tg.Env("proj-dev", "other", 0)["WALG_LIBSODIUM_KEY"]; k != "INSTLIBSODIUM" {
		t.Errorf("new service key %q", k)
	}
	if host.AccessKey != "AKIDSECRET1" || host.LibsodiumKey != "LIBSODIUMSECRET" {
		t.Fatal("host target modified")
	}
}

func TestBackupTargetForResticRepository(t *testing.T) {
	host := testBackup()
	host.Local = true
	tg := BackupTargetFor(host, keyedDecisions())
	idx := tg.ResticRepoIndex("files")
	cur := tg.ResticEnv("proj-dev", "files", idx)
	if idx != 1 || cur["RESTIC_REPOSITORY"] != "s3:http://cvctl-storage:9000/cvctl/h1/proj-dev/files/r1" || cur["RESTIC_PASSWORD"] != "INSTRESTIC" {
		t.Fatalf("current repo %d %v", idx, cur)
	}
	old := tg.ResticEnv("proj-dev", "files", 0)
	if old["RESTIC_REPOSITORY"] != "s3:http://cvctl-storage:9000/cvctl/h1/proj-dev/files" || old["RESTIC_PASSWORD"] != "RESTICSECRET" {
		t.Fatalf("old repo %v", old)
	}
	if hostEnv := host.ResticEnv("proj-dev", "files", host.ResticRepoIndex("files")); hostEnv["RESTIC_REPOSITORY"] != "s3:http://cvctl-storage:9000/cvctl/h1/proj-dev/files" || hostEnv["RESTIC_PASSWORD"] != "RESTICSECRET" {
		t.Fatalf("host target repo %v", hostEnv)
	}
}

func TestBackupTargetForWithoutKeysKeepsHost(t *testing.T) {
	host := testBackup()
	d := keyedDecisions()
	d.Backup = instance.BackupDecision{}
	if BackupTargetFor(host, d) != host {
		t.Fatal("an instance without keys keeps the host target")
	}
	if BackupTargetFor(nil, keyedDecisions()) != nil {
		t.Fatal("no host target means no backups")
	}
}

func TestBackupTargetForExternal(t *testing.T) {
	host := testBackup()
	host.Endpoint = "https://s3.example"
	d := keyedDecisions()
	tg := BackupTargetFor(host, d)
	if tg.Instance != "" || tg.AccessKey != host.AccessKey || tg.LibsodiumKey != "INSTLIBSODIUM" {
		t.Fatalf("external without Ext: host credentials, instance keys: %+v", tg)
	}
	d.Backup.Ext = map[string]string{"bucket": "own", "endpoint": "https://r2.example", "region": "auto", "access_key": "EXTAK", "secret_key": "EXTSK"}
	tg = BackupTargetFor(host, d)
	if tg.Instance != "proj-dev" || tg.Bucket != "own" || tg.Endpoint != "https://r2.example" || tg.Region != "auto" || tg.AccessKey != "EXTAK" || tg.SecretKey != "EXTSK" {
		t.Fatalf("Ext target %+v", tg)
	}
	host.Local = true
	if tg := BackupTargetFor(host, d); tg.Bucket != "cvctl" || tg.AccessKey != "bk-proj-dev" {
		t.Fatalf("the silo ignores Ext: %+v", tg)
	}
}
