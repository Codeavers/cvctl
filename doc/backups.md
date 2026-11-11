# Backups and recovery

Backups go to the storage chosen at `cvctl init`: local silo (S3-compatible) on the host or an S3 bucket
(`CVCTL_STORAGE=s3`, off-site). Databases use WAL-G; dumps, volumes and env files use restic.

## What is backed up

Per app the daily timer backs up databases (WAL-G) and the env files (`cvctl.env`, `app.env`,
deploy key) encrypted with the host restic key; object storage (`s3` services) is not backed up by
default: set `cvctl.backup: volume` on the service to back up its local volume. External services
(R2, S3) are never backed up by cvctl; that is the provider's job. Run manual env backups with
`cvctl backup now <inst> env`.

| Command | What it does |
| --- | --- |
| `cvctl backup now <instance> [service]` | back up an instance now |
| `cvctl backup run` | back up every instance (used by the timer) |
| `cvctl backup list <instance> [service]` | list the backups of an instance |
| `cvctl backup verify [instance]` | restore the latest backups into throwaway containers and check them |
| `cvctl db restore <instance> <service> --to <target>` | restore a data service into a new volume and switch to it |
| `cvctl db cleanup <instance> [--yes]` | list, or with `--yes` remove, unused data volumes of an instance |

`cvctl db restore --skip-wal-flush` does not flush the running cluster's WAL first (WAL not yet
archived stays in the kept old volume).

## Dump backups

A service backed up with `cvctl.backup: dump` has no WAL to mark, so its restore point before a prod
deploy is a full dump. Each prod deploy takes that dump first, so deploy time grows with the size of
the database. If a prune holds the restic repository, the dump waits up to 10 minutes for it.

`dump` has no point-in-time recovery. `cvctl db restore <inst> <svc> --to` takes `latest`, a
snapshot id, a restore point `deploy-<id>`, or a timestamp; a timestamp restores the latest complete
snapshot not newer than it and says which one it used, and one older than every snapshot fails with
`E_RESTORE_NO_BACKUP`.

Only snapshots tagged `complete` are listed, restored or counted by retention; a dump gets that tag
once both sides exited 0 and restic confirms the change. A `partial` snapshot left by a run that died
stays unused and is removed by a later backup, sparing anything less than 2 days older than the
newest partial so overlapping runs do not delete each other's work.

The restore creates a new volume, starts a temporary engine on it and streams the snapshot in while
the service keeps running. Only then are the apps and the service stopped for the volume switch, so
any failure while loading leaves the service, its volume and the instance state as they were, and
the temporary container and volume are removed. A failure after the switch rolls back to the
previous volume like any other restore. `cvctl backup verify` runs the same load in a throwaway
engine, then the kind's `restore_check`.

Retention keeps every dump of the last 7 days, the time WAL-G keeps its restore points, so a second
deploy on a day does not drop the first one's restore point. After that, the 7 daily and 4 weekly
rules apply.

## sqlite

A `sqlite` service is dumped by a helper beside the running app, with `sqlite3 .backup` to a
temporary file inside the helper and from there into restic; the volume is mounted read-only unless
the database is in WAL mode or has a journal to roll back. The copy is one snapshot taken in a
single read transaction, so it finishes however busy the app is: in WAL mode the app is not held up,
while in rollback mode its commits wait until the copy ends, so its busy timeout should cover that.

While the database file does not exist or is empty the backup stores nothing and reports OK with a
note, and a prod deploy records no restore point for it. Once a complete snapshot exists that no
longer holds: a missing file or volume means the database was lost, so the backup and the restore
point, and with it a prod deploy, fail with `E_SQLITE_LOST` until it is restored or the app creates
the file again.

`cvctl db restore` takes the same `--to` forms but stops the apps first, writes the snapshot to a
new volume, runs `PRAGMA integrity_check` on it, switches and starts them; a failure before the
switch restarts them on the old volume. `cvctl backup verify` restores to a temporary volume and
requires `integrity_check` to print exactly `ok`.

`x-cvctl.verify` runs SQL on a restored `wal-g` service, so `lint` rejects it on a dump service
(`E_CONTRACT_VERIFY_DUMP`). A `sqlite` service with no snapshot yet fails verify and
`cvctl project restore` with `E_BACKUP_NO_SNAPSHOT`, like any service without backups.

## Disaster recovery and restore

With off-site backups (`CVCTL_STORAGE=s3`), rebuilding an instance on a new host only needs the host
bundle (`cvctl secrets export --host`). The order the disaster-recovery e2e runs is: `cvctl init`
with S3 storage (`CVCTL_STORAGE=s3`), `cvctl secrets import --force <bundle>`, start the agent, then
`cvctl project restore <inst>`, which fails with `E_RESTORE_LOCAL_STORAGE` without S3 storage.
`cvctl.env`, `app.env` and the deploy key are recovered automatically from the latest off-site env
backup (or pick an older one with `--env-snapshot <id>`). Every backup of the whole instance
(`cvctl backup now <inst>` and the timer) takes that env backup; if none exists (`cvctl backup list
<inst> env`), restore fails with `E_RESTORE_NO_SECRETS`, so take one (`cvctl backup now <inst> env`)
or keep an instance bundle (`cvctl secrets export --instance <inst>`) and import it after the host
bundle, before `project restore` (`cvctl secrets import <inst>.age`). If `cvctl.env` already
exists locally, those files are kept as is and `--env-snapshot` is rejected (`E_USAGE_ENV_SNAPSHOT`).

`cvctl project restore` clones the infra repository, restores every data service from its latest
backup into a fresh volume, deploys the instance, and starts a new WAL-G backup generation. `s3`
services without `cvctl.backup: volume` are recreated empty. `--repo` takes the infra repository URL
or `oci://host/repository` and overrides `CVCTL_INFRA_REF` and `CVCTL_INFRA_URL` from `cvctl.env`.

## Backup generations

`cvctl db restore` brings a data service back into a new volume and starts a new WAL-G backup
generation: the restored cluster is on a new timeline, so it archives under
`<bucket>/<host>/<instance>/<service>/g<N>` instead of mixing timelines in the old prefix
(generation 0 is the original, unsuffixed prefix). `cvctl.env` keeps the current generation in
`CVCTL_SVC_<SVC>_BACKUP_GEN` and the highest one ever used in `..._BACKUP_GEN_MAX`.

A restore target (timestamp or `deploy-<id>`) older than the current generation's first base
backup fails with `E_RESTORE_NO_BACKUP`. Older generations stay in storage under `g<k>`, `k < N`;
restore them manually with `wal-g` using that prefix; they are never pruned. The bucket is the
source of truth: every new generation is numbered above the highest one in the bucket, and
`cvctl project restore` on a new host restores the newest generation holding a base backup even
when the secrets bundle names an older one (it warns). Re-run `cvctl secrets export` after a restore.

## Secrets bundle

`cvctl secrets` backs up or restores host and instance secrets as an age-encrypted bundle (`0600`).
Plaintext only lives in memory. `-o -` writes the bundle ASCII-armored to stdout; the shell creates
the redirected file, hence the `umask` and `set -C` (no overwrite) below.

```
sudo cvctl secrets export -o secrets.age
sudo cvctl secrets export --instance demo-prod -o demo-prod.age
sudo cvctl secrets export --host -o host.age
(umask 077; set -C; sudo cvctl secrets export -o - > secrets.age.txt)

sudo cvctl secrets import secrets.age
sudo cvctl secrets import --force secrets.age
```

- **Scope flags**: `--instance <name>` bundles the instance's files under `/srv/deploy/<inst>/` and
  its deploy key; `--host` bundles only `host.env` and `/etc/cvctl/secrets/*`. The flags are
  mutually exclusive (`E_USAGE_SCOPE`). Omitting both bundles every host secret, instance env file
  and deploy key.
- **Passphrase**: prompted interactively on the terminal (typed twice, hidden) or read from
  `CVCTL_SECRETS_PASSPHRASE`.
- **Import**: validates every entry path, file mode, and owner before writing. Partial bundles
  (host-only or instance-only) are accepted. Existing files are not overwritten unless `--force`
  is given (`E_SECRETS_EXISTS`).
- **Remote export**: `cvctl remote export <host> [--port <port>] [--user <user>]` runs export
  over SSH with `-t` and accepts the same `--instance` and `--host` flags.
