# cvctl

Pull-based deploy agent and CLI for Docker Compose on a single VPS.

Every command accepts `--json` and then prints exactly one JSON document.
Exit codes: `0` ok, `1` validation or business error, `2` usage error.
Every error and issue carries a stable `E_<AREA>_<CAUSE>` code and a `hint`.

## Install from GHCR

Release images are `FROM scratch` (no `cat`), so copy the binary out of a stopped container:

```
id=$(docker create ghcr.io/codeavers/cvctl:<v>); docker cp "$id":/cvctl ./cvctl; docker rm "$id"
```

Binaries and `sha256sums.txt` are also attached to each GitHub release. `docker login ghcr.io`
first with the host token. `cvctl version` prints the embedded tag.

## Project contract

`compose.yml` is read for an allowlist of fields only and is never passed to `docker compose`;
cvctl generates the compose file it runs. Unknown or unsafe fields fail with
`E_CONTRACT_FIELD_FORBIDDEN`. `env.spec.yml` names and values are checked
(`E_ENVSPEC_NAME`, `E_ENVSPEC_VALUE`). Run `cvctl lint <dir>` before publishing.

Each instance gets its own networks, `<inst>_edge` (Traefik) and `<inst>_backup` (WAL-G and the
backup user `bk-<inst>` with policy `cvctl-bk-<inst>`), so instances cannot reach each other.
Edge aliases, routers and services are named `<inst>--<svc>`, so instance names cannot contain `--`
(`E_INSTANCE_NAME`).

## OCI source and publish

A project can take its contract from an OCI artifact instead of git:

```
cvctl publish ./infra ghcr.io/<org>/<repo>-infra:develop
cvctl project add oci://ghcr.io/<org>/<repo>-infra --env dev
```

`project add` takes the repository without a tag or digest: the instance follows `develop` in dev
and `main` in prod, the same tags `publish` pushes.

`publish` runs the `lint` checks, then pushes `compose.yml`, `env.spec.yml` and, if present,
`deploy/prod.env` (as `prod.env`) and prints the digest (`--json`: `{"ref":...,"digest":...}`).
Credentials: `CVCTL_PUBLISH_TOKEN` or `GITHUB_TOKEN`, user `CVCTL_PUBLISH_USER` or `GITHUB_ACTOR`;
the token goes only to the registry host of the ref. Failures use `E_PUBLISH`; a bad artifact on
the host side is `E_INFRA_ARTIFACT`. Layers are capped at 1 MiB. Hosts in
`CVCTL_INSECURE_REGISTRIES` use plain HTTP. A CI template is in
[examples/ci/publish-contract.yml](examples/ci/publish-contract.yml).

## Migrating an existing deploy

`project add --env-file <path>` reads an existing `.env` (docker compose rules; no host
environment expansion) and takes only the env.spec vars with source `random` or `prompt`, under
the same names. `service`, `fixed` and `template` vars come from cvctl and the contract, and keys
outside env.spec never reach `app.env`. `--set` wins over the file. The output lists the keys
used and ignored, never their values. Errors: `E_USAGE_ENV_FILE`; a multi-line value is
`E_ENVSPEC_VALUE`.

```
sudo cvctl project add oci://ghcr.io/<org>/<repo>-infra --env dev \
  --env-file /srv/deploy/<project>/dev/.env --domain web=app.example.com
```

## Domain aliases and redirects

`--domain` can repeat for one service: the first is its primary domain (`{svc.domain}` in
`env.spec.yml`) and the rest are aliases that serve the same app. `--redirect source=destination`
answers `source` with a 301 to the same path and query on `destination`, which must be the primary
domain or an alias of an exposed service.

```
sudo cvctl project add oci://ghcr.io/<org>/<repo>-infra --env prod \
  --domain web=example.com --domain web=app.example.com --redirect www.example.com=example.com
sudo cvctl project update <inst> --redirect old.example.com=example.com
sudo cvctl project update <inst> --redirect old.example.com=
```

In `project update`, `--domain` for a service replaces its primary domain and all its aliases;
`--redirect source=` removes that redirect. Every domain gets its own Traefik router and
certificate, so each one needs its DNS pointing at this host; one without it only fails its own
certificate. A domain belongs to one service only, as primary, alias or redirect source
(`E_DECISION_INVALID`, `E_USAGE_REDIRECT`), and never to an adopted service (`E_ADOPT_DOMAIN_IN_USE`).
They are stored as `CVCTL_ALIASES_<SVC>` and `CVCTL_REDIRECT_<SVC>` in `cvctl.env`.

## project list

`cvctl project list [--json]` reads each instance's `cvctl.env` and installed contract (no network,
no docker) and prints its env, strategy, contract source, tracked tag, domains, data service modes and
whether it is paused. An instance whose contract cannot be read is listed with the error.
`cvctl status` shows the strategy too.

## plan

`cvctl plan [--json]` describes what this binary would do for each instance, from its `cvctl.env`,
installed contract and embedded `kinds` catalog (read-only, no network, no docker). Per instance:
`env`, `strategy`, `apps` (image without digest, exposed port, domain, and `aliases` and
`redirects` when set), `data` (`kind`, `version`,
`mode`, `backup`, buckets), `health`, hook counts and `issues`, the sorted codes of the contract and
decision checks a deploy would fail on (codes only, omitted when empty; app digests are not required,
because a deploy gets them from the desired state). The document has a `schema` number, sorted
keys and no secrets (no `app.env` values or `cvctl.env` credentials), so two runs of the same
binary are byte-equal and two versions can be diffed. An unreadable instance appears with `error`.

## upgrade

`sudo cvctl upgrade [--version vX.Y.Z] [--check] [--yes] [--accept-changes]` replaces the installed
`/usr/local/bin/cvctl` with the latest stable release, verifies its checksum, restarts `cvctl-agent`
and watches it for 30 s; if it does not hold, the previous binary is put back (`E_UPGRADE_FAILED`).
The previous binary stays in `/usr/local/bin/cvctl.previous`.

It needs root, `--check` included (`E_UPGRADE_ROOT`). It refuses while any instance is `DEPLOYING` and
while another `cvctl upgrade` runs (`E_UPGRADE_BUSY`). It replaces only the binary: it does not run
`cvctl init` or `cvctl project update`, so systemd units and host config are not refreshed; run them
yourself if the release notes ask for it.

If `cvctl-agent` is not active (`inactive` or `failed`), the binary is installed and the agent is left
as it was: no restart and no watch, so nothing has tested the new binary. The output and the notice say
so, and `--json` reports `agent_stopped`. While the agent is starting or stopping, or if its state
cannot be read, nothing is installed (`E_UPGRADE_INSTALL`).

Before installing it shows both versions, the release notes and the differences between what the
current and the new binary would do per instance (`cvctl plan`). To get them, the downloaded binary,
once its checksum matches, runs as root before any confirmation, even with `--check`. The checksum
catches a corrupt download, not a compromised release.

- `--version vX.Y.Z` installs that release instead of the latest; it is the only way to go back to an
  older version, and to upgrade a development build.
- `--check` shows everything and exits without installing.
- `--yes` answers the `y/N` question asked when there are no differences.
- `--accept-changes`, together with `--yes`, also accepts differences. With differences, or when the
  comparison failed, you must otherwise type the target version; `--yes` alone is not enough.

Without a terminal and those options it fails with `E_UPGRADE_CONFIRM` and installs nothing. With
`--json` there are no questions.

Releases come from `Codeavers/cvctl`. For a fork, set `CVCTL_UPGRADE_REPO=owner/repo` in
`/etc/cvctl/host.env` (`sudo` usually drops it from the environment, so a variable there is not
reliable); a `CVCTL_UPGRADE_REPO` in the environment takes precedence. An invalid value in `host.env`
makes the command fail instead of falling back. The review shows the repository the release comes
from (`repo` in `--json`).

The host notifier gets `upgraded vA -> vB` (`info`), with a note if the agent was not started, or an
`error` notice if the install failed. A notifier failure is logged and does not fail the upgrade.

## Deploy strategy

`recreate` (default for dev) replaces containers in place: a few seconds of downtime per deploy.
`blue-green` (default for prod) swaps the services that carry `cvctl.expose` without downtime.
`project add --strategy recreate|blue-green` sets it explicitly (interactive or not; without the
flag: prod gets `blue-green`, everything else `recreate`) and `cvctl set-strategy <instance>
<strategy>` changes it later. An instance with a `sqlite` service only runs `recreate`.

Per exposed service, in contract order: compose scales it to 2 (the old container keeps running
and a new one starts with the new config); the new container must pass its health check; Traefik's
route is pinned to the new container only; the old one is stopped with its `stop_grace_period`
(10s by default) and removed; the route goes back to the service alias, which now resolves only to
the new container. If the new container is unhealthy it is removed and the old one never stops.
Workers, data services and non-exposed apps are recreated as in `recreate`. The first deploy of an
instance is a plain `up`. `cvctl history` lists the services swapped blue-green per deploy.

Constraints:

- Memory of a swapped service doubles while both containers run.
- Migrations run before the swap, so they must be backward compatible with the old version:
  expand in one deploy, contract in a later one.
- Websocket connections on the old container close when it stops (clients must reconnect).

## Timeouts

Defaults and `host.env` keys: fetch 2m (`CVCTL_TIMEOUT_FETCH`), pull 10m (`CVCTL_TIMEOUT_PULL`),
each hook 15m (`CVCTL_TIMEOUT_HOOK`; `x-cvctl.hooks[].timeout` can only lower it), whole deploy
45m (`CVCTL_TIMEOUT_DEPLOY`). Health waits use `x-cvctl.health.timeout` (at most 10m) plus 2m.
Exceeding one fails with `E_TIMEOUT`. Fetch, pull and deploy timeouts are retried on the next
cycle; a hook timeout fails that desired state until it changes.

Traefik's `readTimeout` (how long a request body may take to arrive) is Traefik's default, 60s,
unless `CVCTL_TRAEFIK_READ_TIMEOUT` sets it (e.g. `15m` for large uploads). It applies to the
`web` and `websecure` entrypoints; re-run `cvctl init` to apply it. Traefik's access log is JSON
without `RequestPath`, since paths can carry tokens.

## Upgrading a host to v0.7

Requires Docker Engine 28 or later (`E_DOCKER_TOO_OLD`). Instance names containing `--` must be
renamed first: `E_INSTANCE_NAME`, and bundle import rejects them. Run, in this order:

1. With the new binary, `cvctl lint` each instance's infra dir and fix and publish what it reports
   (forbidden compose fields, service names that are not `^[a-z0-9]+([-_][a-z0-9]+)*$`).
2. Check that the app images run without capabilities: no port below 1024, no setuid at start.
3. Install the binary.
4. `cvctl project update <inst>` for every instance: per-instance backup keys (the backup
   generation is bumped, see Backup generations; failures are `E_BACKUP_PROVISION`), the
   `<inst>_edge` and `<inst>_backup` networks, and connecting Traefik and the storage silo to them.
5. `cvctl init` last: Traefik moves to `cvctl-edge-proxy` (`10.203.0.0/24`,
   `E_TRAEFIK_EDGE_SUBNET` if it clashes) and the legacy `cvctl-edge` and `cvctl-backup` networks
   are retired once nothing else uses them.
6. `cvctl secrets export`, so the bundle holds the new keys.
7. Registry logins other than the token host (`CVCTL_REGISTRY_HOST`, else the first
   `CVCTL_REGISTRIES` host) are pruned automatically by the agent on start.

## Upgrading to v0.8.0

`s3` services stop being backed up unless the contract sets `cvctl.backup: volume`. The old restic
repository `<bucket>/<host>/<inst>/<svc>` is no longer used and can be deleted by hand.

## init

Prepares a Linux host with systemd. On Ubuntu and Debian it installs and hardens everything;
elsewhere install Docker yourself and cvctl skips what the host lacks. It adopts an existing
server to the desired state idempotently: packages, docker daemon, accounts (`deployer` user), SSH hardening
with auto-revert timer, fail2ban, firewall (UFW), unattended security upgrades, Traefik v3
reverse proxy (file provider only, no Docker socket), backup storage (local MinIO or S3),
and notifications.

```
sudo cvctl init
sudo cvctl init --dry-run
sudo cvctl init --adopt
sudo cvctl init --non-interactive --json
sudo cvctl init --reconfigure notify
```

`--dry-run` prints the plan without changing anything. SSH changes are applied with a revert
timer: if the new session cannot be confirmed, the previous sshd config comes back on its own.

init only asks for values `host.env` and `secrets/` lack. `--reconfigure notify` asks the
notification questions again (current values as defaults; an empty token keeps the stored one, an empty thread ID removes it).
When the settings change, init sends a test message and restarts `cvctl-agent` so it uses them;
disabling notifications only restarts the agent. The agent and the backup timers log notifier
errors (bad configuration, failed sends) to the journal.

### Deploy notices and image labels

Telegram deploy notices show the PR, commit, title and author of what was deployed. cvctl reads
them from the labels of the changed images, so the build must set `cvctl.repo`, `cvctl.commit`,
`cvctl.pr`, `cvctl.title` and `cvctl.author` (`cvctl.pr`, `cvctl.title` and `cvctl.author` may be
empty). `cvctl.repo` is the repository URL (`https://github.com/org/app`); the notice links the
PR and commit from it, and a value that is not an https URL drops the links. `cvctl.author` is the
commit author name. [examples/ci/image-labels.yml](examples/ci/image-labels.yml) sets them with
`docker/build-push-action`, taking the PR number and title from the merge or squash commit.

Without labels the notice still goes out with the result only. Labels are read with a 5 second
timeout; a failed read never fails the deploy.

A failed deploy adds an `Error` line with the cause and a `Logs` line with the history command.
When there is something to do, a `Next` line gives the command: `sudo cvctl resume <instance>` after
a manual rollback, or `cvctl db restore <instance> <service> --to <id>` when a rolled back deploy
had already run its migrations.

`info` notices are silent: successful deploys, the pause after a manual rollback, finished restores
and `cvctl notify test`. `warn` and `error` notices make a sound.

## Adopting external services

`cvctl adopt` exposes a process cvctl does not deploy, such as a systemd unit, a manual Node process or
another Docker stack, without a contract:

```
sudo cvctl adopt add panel --domain panel.example.com --target https://host:9090 --insecure-tls
sudo cvctl adopt list
sudo cvctl adopt rm panel
```

cvctl only adds the Traefik route and TLS, and `cvctl status` lists the service as `ADOPTED` without
checking it. It does not start, update, notify about or back up what it adopts, and `adopt rm` leaves the
process running.

`host` is the gateway of Traefik's network, so the process cannot listen on `127.0.0.1` only: `adopt add`
fails with `E_ADOPT_UNREACHABLE` unless it answers there. `--force` skips that check for a stopped process.
With `CVCTL_FIREWALL_MANAGE=true` the port opens to Traefik's subnet only. Domains are exclusive: an instance
and an adopted service cannot share one, counting the instance's aliases and redirect sources. HTTP only; raw TCP or UDP ports stay out.

## lint

Validates `compose.yml` and `env.spec.yml` against the cvctl contract.

```
cvctl lint examples/demo
```

## publish

See [OCI source and publish](#oci-source-and-publish).

## render

Turns an instance directory into the files Docker Compose and Traefik consume.
The directory holds `cvctl.env` (the instance decisions) and `infra/` (a checkout
of the project's infra, containing `compose.yml` and `env.spec.yml`).

```
cvctl render --instance-dir /srv/deploy/demo-dev [--edge edge|behind-proxy]
```

It writes, atomically and with mode `0640`:

- `compose.cvctl.yml`: override file wiring data services, limits, networks and the route aliases (routes themselves come from the Traefik file provider, see `traefik.yml`);
- `traefik.yml`: routes, only when some service is exposed;
- `app.env`: resolved variables from `env.spec.yml`, sorted. Existing values
  (including ones set with `cvctl set-env`, coming in a later release) and generated secrets are preserved,
  so re-running is idempotent.

Variables of source `prompt` without a default are not an error: they are
reported as `missing` together with a hint, and the render still succeeds. `cvctl set-env` is coming in a later release; until then set the value in `app.env` by hand.

```
$ cvctl render --instance-dir /srv/deploy/demo-dev --json
{"ok":true,"data":{"files":[...],"generated":["JWT_SECRET"],"hint":"cvctl set-env demo-dev SMTP_PASSWORD","instance":"demo-dev","missing":["SMTP_PASSWORD"]}}
```

`examples/demo` is a complete sample project (an API, a worker, Postgres, Redis and S3).

## secrets

Backs up or restores host and instance secrets as an age-encrypted bundle (`0600`). Plaintext
only lives in memory.

```
sudo cvctl secrets export -o secrets.age
sudo cvctl secrets export --instance demo-prod -o demo-prod.age
sudo cvctl secrets export --host -o host.age
sudo cvctl secrets export -o - > secrets.age.txt

sudo cvctl secrets import secrets.age
sudo cvctl secrets import --force secrets.age
```

- **Scope flags**: `--instance <name>` bundles the instance's files under `/srv/deploy/<inst>/` and its deploy key;
  `--host` bundles only `host.env` and `/etc/cvctl/secrets/*`. The flags are mutually exclusive
  (`E_USAGE_SCOPE`). Omitting both bundles every host secret, instance env file and deploy key.
- **Passphrase**: prompted interactively on the terminal (typed twice, hidden) or read from
  `CVCTL_SECRETS_PASSPHRASE`.
- **Import**: validates every entry path, file mode, and owner before writing. Partial bundles
  (host-only or instance-only) are accepted. Existing files are not overwritten unless `--force`
  is given (`E_SECRETS_EXISTS`).
- **Remote export**: `cvctl remote export <host> [--port <port>] [--user <user>]` runs export
  over SSH with `-t` and accepts the same `--instance` and `--host` flags.

## Data kinds

A service declares `cvctl.kind` to get a managed data service: `postgres` (15, 16, 17), `postgis`
(Postgres with PostGIS, 16, 17; same WAL-G backups and PITR as `postgres`), `redis`, `s3` and
`sqlite`.

## sqlite

`cvctl.kind: sqlite` starts no container: it declares a managed volume mounted in one app, with no
port, health check, credentials or external mode.

```yaml
store:
  labels:
    cvctl.kind: sqlite
    cvctl.owner: api   # the one app that uses it; depends_on stays rejected
    cvctl.env: STORE
    cvctl.uid: "1000"  # optional
```

- The app gets `STORE_PATH=/data/store/app.db` and must keep its database at that path: backups read
  no other file.
- **Owner of the volume**: the uid of `cvctl.uid`, or else the image's `USER` when it is numeric
  (`1000` or `1000:1000`). A name or an empty `USER` requires `cvctl.uid`, and root is rejected
  (`E_SQLITE_UID`). `cvctl.uid` also renders `user: <uid>` on the owner app, so the app runs as that
  uid whatever its image says: with every capability dropped, an image that defaults to root could
  not write to the volume. On each deploy, after the pull and
  before the hooks, a helper container holding only `CHOWN` hands the volume over if its owner differs.
  A rollback does the same for the release it starts, and a deploy that fails before the switch gives
  the volume back to the running app. All the sqlite services of one app must set the same `cvctl.uid`,
  or none.
- **Strategy**: requires `recreate`, so a deploy stops the app. `project add`, `set-strategy` and the
  deploy reject `blue-green` with `E_SQLITE_RECREATE`; `project add` on prod without `--strategy`
  picks `recreate` and warns.
- Helpers run `ghcr.io/codeavers/cvctl-sqlite` with no network and no capabilities. Publish that image
  (workflow `image-sqlite`) before the first deploy of a release that uses the kind.

## S3 buckets

An `s3` service declares its buckets in `x-cvctl.s3.<service>.buckets`, each public or private
(see `examples/demo`). The app gets `S3_BUCKET_<B>` per bucket and `S3_PUBLIC_URL_<B>` per public
bucket, plus `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` and `S3_REGION`.

- **Local (silo)**, for dev and tests: every deploy creates the buckets and a user scoped to them
  (`<service>-app`); the app never receives the silo root keys. Public buckets allow anonymous
  `GetObject` only, and `cvctl.expose: "9000"` publishes them through Traefik for `GET`/`HEAD` on
  `/<bucket>/<key>`. A public bucket on local silo is rejected in `prod`.
- **External (R2, S3)**, for prod: cvctl creates nothing; `project add`/`update` ask for each
  bucket's real name and, for public ones, its public URL (or take them from
  `--ext <service>.<field>=<value>`). Every deploy probes each bucket with the app keys and fetches
  the probe object anonymously from each public URL. Backups are the provider's job.

- **Not backed up by default**: object storage (photos, uploads) is treated as disposable. A
  contract that needs it backed up sets `cvctl.backup: volume` on the service (restic, daily).
- **Upgrading** an instance that already runs a local `s3` service: its deploys fail with
  `E_DECISION_MISSING` until `cvctl project update <instance>` generates the app keys; from then on
  the app holds scoped keys and cannot create buckets itself. Without declared buckets the bucket is
  `CVCTL_SVC_<SVC>_DB`, which must be a valid S3 bucket name; otherwise declare the buckets.
- **Moving to external**: `cvctl project update <instance> --ext <service>.endpoint=…` switches a
  local service with `cvctl.external: allowed` to external. Data is not copied; use `mc mirror`.
- `--ext` values land in the shell history and the process list; prefer the interactive prompt
  for keys on a shared host.
- An exposed silo joins its instance's `<inst>_edge` network so Traefik can reach it; anonymous access stays
  limited to `GetObject` on public buckets.

## Backups

Per app the daily timer backs up databases (WAL-G) and the env files (`cvctl.env`, `app.env`, deploy key) encrypted with the host restic key; object storage (`s3` services) is not backed up by default: set `cvctl.backup: volume` on the service to back it up. Run manual env backups with `cvctl backup now <inst> env`.

A service backed up with `cvctl.backup: dump` has no WAL to mark, so its restore point before a prod deploy is a full dump. Each prod deploy takes that dump first, so deploy time grows with the size of the database. If a prune holds the restic repository, the dump waits up to 10 minutes for it.

`dump` has no point-in-time recovery. `cvctl db restore <inst> <svc> --to` takes `latest`, a snapshot id, a restore point `deploy-<id>`, or a timestamp; a timestamp restores the latest complete snapshot not newer than it and says which one it used, and one older than every snapshot fails with `E_RESTORE_NO_BACKUP`. Only snapshots tagged `complete` are listed, restored or counted by retention; a dump gets that tag once both sides exited 0 and restic confirms the change. A `partial` snapshot left by a run that died stays unused and is removed by a later backup, sparing anything less than 2 days older than the newest partial so overlapping runs do not delete each other's work.

The restore creates a new volume, starts a temporary engine on it and streams the snapshot in while the service keeps running. Only then are the apps and the service stopped for the volume switch, so any failure while loading leaves the service, its volume and the instance state as they were, and the temporary container and volume are removed. A failure after the switch rolls back to the previous volume like any other restore. `cvctl backup verify` runs the same load in a throwaway engine, then the kind's `restore_check`.

A `sqlite` service is dumped by a helper beside the running app, with `sqlite3 .backup` to a temporary file inside the helper and from there into restic; the volume is mounted read-only unless the database is in WAL mode or has a journal to roll back. The copy is one snapshot taken in a single read transaction, so it finishes however busy the app is: in WAL mode the app is not held up, while in rollback mode its commits wait until the copy ends, so its busy timeout should cover that. While the database file does not exist or is empty the backup stores nothing and reports OK with a note, and a prod deploy records no restore point for it. Once a complete snapshot exists that no longer holds: a missing file or volume means the database was lost, so the backup and the restore point, and with it a prod deploy, fail with `E_SQLITE_LOST` until it is restored or the app creates the file again. `cvctl db restore` takes the same `--to` forms but stops the apps first, writes the snapshot to a new volume, runs `PRAGMA integrity_check` on it, switches and starts them; a failure before the switch restarts them on the old volume. `cvctl backup verify` restores to a temporary volume and requires `integrity_check` to print exactly `ok`. `x-cvctl.verify` runs SQL on a restored `wal-g` service, so `lint` rejects it on a dump service (`E_CONTRACT_VERIFY_DUMP`). A `sqlite` service with no snapshot yet fails verify and `cvctl project restore` with `E_BACKUP_NO_SNAPSHOT`, like any service without backups.

Retention keeps every dump of the last 7 days, the time WAL-G keeps its restore points, so a second deploy on a day does not drop the first one's restore point. After that, the 7 daily and 4 weekly rules apply.

### Disaster recovery and restore

With off-site backups (`CVCTL_STORAGE=s3`), rebuilding an instance on a new host only needs the host bundle (`cvctl secrets export --host`) plus `cvctl project restore <inst>`. `cvctl.env`, `app.env` and the deploy key are recovered automatically from the latest off-site env backup (or pick an older one with `--env-snapshot <id>`). If `cvctl.env` already exists locally, those files are kept as is and `--env-snapshot` is rejected (`E_USAGE_ENV_SNAPSHOT`).

`cvctl project restore` clones the infra repository, restores every data service from its latest backup into a fresh volume, deploys the instance, and starts a new WAL-G backup generation. `s3` services without `cvctl.backup: volume` are recreated empty.

### Backup generations

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

## images prune

`sudo cvctl images prune [--dry-run] [--instance <inst>]` removes the images that deploys left behind. Per service it keeps the current version and the 2 previous ones that really ran, 3 in total, plus every digest of an instance that is deploying or has no state yet. After each successful deploy the same cleanup runs on its own, so the command is mostly for hosts that already hold many images.

Only images whose digest cvctl recorded in an instance's history are touched; other digests on the host are never listed or removed. An image with a tag is never touched, in any repository: a tag on a digest protects that digest everywhere. An untagged image of another repository with the same digest can be removed, since it is the same content. Images in use by a container are skipped (Docker refuses them).

A rollback to a version beyond those kept pulls the image again, so it needs the image to still be in the registry. If the pull fails, the rollback stops with an error that names the image, and nothing changes. `--instance` limits what is removed to that instance's images, but the keep list still covers every instance. If any instance's state cannot be read, nothing is removed.

On a host with many images run `--dry-run` first: it lists each image with its size and the reason, plus the total, without removing anything.

## Development

Checks run locally before a push, not on pull requests. Enable the pre-push hook once per clone:
`git config core.hooksPath .githooks`. See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache License 2.0, see [LICENSE](LICENSE).
