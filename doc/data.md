# Data services

## Data kinds

A service declares `cvctl.kind` to get a managed data service:

| Kind | Versions | Backup | External mode |
| --- | --- | --- | --- |
| `postgres` | 15, 16, 17 | WAL-G with PITR | yes |
| `postgis` | 16, 17 (Postgres with PostGIS) | same WAL-G backups and PITR as `postgres` | yes |
| `redis` | 7 | none | yes |
| `s3` | S3-compatible (silo when local) | none unless `cvctl.backup: volume` | yes |
| `sqlite` | no container | dump with restic | no |

A service with `cvctl.external: allowed` can run on the host (local) or point at an external
provider; `project add` and `project update` ask for the external values, or take them from
`--ext <service>.<field>=<value>`. Backups are described in [backups.md](backups.md).

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

How a sqlite database is backed up and restored is in [backups.md](backups.md#sqlite).

## S3 buckets

An `s3` service declares its buckets in `x-cvctl.s3.<service>.buckets`, each public or private
(see [examples/demo](../examples/demo/compose.yml)). The app gets `S3_BUCKET_<B>` per bucket and
`S3_PUBLIC_URL_<B>` per public bucket, plus `S3_ENDPOINT`, `S3_ACCESS_KEY`, `S3_SECRET_KEY` and
`S3_REGION`.

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
