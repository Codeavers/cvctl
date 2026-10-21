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
[docs/ci/publish-contract.yml](docs/ci/publish-contract.yml); the cvctl package must grant read
access to the project repository (Package settings, Manage Actions access).

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

## project list

`cvctl project list [--json]` reads each instance's `cvctl.env` and installed contract (no network,
no docker) and prints its env, contract source, tracked tag, domains, data service modes and
whether it is paused. An instance whose contract cannot be read is listed with the error.

## Timeouts

Defaults and `host.env` keys: fetch 2m (`CVCTL_TIMEOUT_FETCH`), pull 10m (`CVCTL_TIMEOUT_PULL`),
each hook 15m (`CVCTL_TIMEOUT_HOOK`; `x-cvctl.hooks[].timeout` can only lower it), whole deploy
45m (`CVCTL_TIMEOUT_DEPLOY`). Health waits use `x-cvctl.health.timeout` (at most 10m) plus 2m.
Exceeding one fails with `E_TIMEOUT`. Fetch, pull and deploy timeouts are retried on the next
cycle; a hook timeout fails that desired state until it changes.

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

## init

Prepares an Ubuntu 24.04 or later VPS from scratch or adopts an existing server to the desired
state idempotently: packages, docker daemon, accounts (`deployer` user), SSH hardening
with auto-revert timer, fail2ban, firewall (UFW), unattended security upgrades, Traefik v3
reverse proxy (file provider only, no Docker socket), backup storage (local MinIO or S3),
and notifications.

```
sudo cvctl init
sudo cvctl init --dry-run
sudo cvctl init --adopt
sudo cvctl init --non-interactive --json
```

See [docs/host-init.md](docs/host-init.md) for full step documentation, file permissions table,
SSH try/confirm recovery flow, and non-interactive configuration.

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

## Data kinds

A service declares `cvctl.kind` to get a managed data service: `postgres` (15, 16, 17), `postgis`
(Postgres with PostGIS, 16, 17; same WAL-G backups and PITR as `postgres`), `redis` and `s3`.

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

Design: `docs/specs/2026-10-17-s3-buckets-design.md`.

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
