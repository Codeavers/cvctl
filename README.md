# cvctl

Pull-based deploy agent and CLI for Docker Compose on a single VPS.

Status: plan 2 (host preparation). Commands available: `init`, `lint`, and `render`.
The deploy agent arrives in later plans.

Every command accepts `--json` and then prints exactly one JSON document.
Exit codes: `0` ok, `1` validation or business error, `2` usage error.
Every error and issue carries a stable `E_<AREA>_<CAUSE>` code and a `hint`.

## init

Prepares an Ubuntu 24.04 VPS from scratch or adopts an existing server to the desired
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

## Backup generations

`cvctl db restore` brings a data service back into a new volume and starts a new WAL-G backup
generation: the restored cluster is on a new timeline, so it archives under
`<bucket>/<host>/<instance>/<service>/g<N>` instead of mixing timelines in the old prefix
(generation 0 is the original, unsuffixed prefix). `cvctl.env` keeps the current generation in
`CVCTL_SVC_<SVC>_BACKUP_GEN` and the highest one ever used in `..._BACKUP_GEN_MAX`.

A restore target (timestamp or `deploy-<id>`) older than the current generation's first base
backup fails with `E_RESTORE_NO_BACKUP`. Older generations stay in storage under `g<k>`, `k < N`;
restore them manually with `wal-g` using that prefix. `cvctl project restore` on a new host does
the same: it reads the latest backup of the current generation, then advances to a new one.
