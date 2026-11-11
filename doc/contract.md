# Project contract

A project describes itself with two files in its infra directory: `compose.yml` and
`env.spec.yml`. `examples/demo` is a complete sample project (an API, a worker, Postgres, Redis
and S3): [compose.yml](../examples/demo/compose.yml), [env.spec.yml](../examples/demo/env.spec.yml).

## compose.yml and env.spec.yml

`compose.yml` is read for an allowlist of fields only and is never passed to `docker compose`;
cvctl generates the compose file it runs. Unknown or unsafe fields fail with
`E_CONTRACT_FIELD_FORBIDDEN`. `env.spec.yml` names and values are checked
(`E_ENVSPEC_NAME`, `E_ENVSPEC_VALUE`). Run `cvctl lint <dir>` before publishing.

Each `env.spec.yml` var has a source: `service` (taken from a data service), `random` (generated
secret), `fixed`, `template` (e.g. `https://{domain:api}`) or `prompt` (asked when the instance is
added).

Each instance gets its own networks, `<inst>_edge` (Traefik) and `<inst>_backup` (WAL-G and the
backup user `bk-<inst>` with policy `cvctl-bk-<inst>`), so instances cannot reach each other.
Edge aliases, routers and services are named `<inst>--<svc>`, so instance names cannot contain `--`
(`E_INSTANCE_NAME`).

Data services (`cvctl.kind`) are described in [data.md](data.md).

## lint

Validates `compose.yml` and `env.spec.yml` against the cvctl contract.

```
cvctl lint examples/demo
```

## publish and the OCI source

A project can take its contract from an OCI artifact instead of git:

```
cvctl publish ./infra ghcr.io/<org>/<repo>-infra:develop
cvctl project add oci://ghcr.io/<org>/<repo>-infra --env dev
```

`project add` takes the repository without a tag or digest: the instance follows `develop` in dev
and `main` in prod, or the tag given with `--tag` (dev only); publish the contract under that tag.

`publish` runs the `lint` checks, then pushes `compose.yml`, `env.spec.yml` and, if present,
`deploy/prod.env` (as `prod.env`) and prints the digest (`--json`: `{"ref":...,"digest":...}`).

- **Credentials**: `CVCTL_PUBLISH_TOKEN` or `GITHUB_TOKEN`, user `CVCTL_PUBLISH_USER` or
  `GITHUB_ACTOR`; the token goes only to the registry host of the ref.
- **Errors**: failures use `E_PUBLISH`; a bad artifact on the host side is `E_INFRA_ARTIFACT`.
- **Limits**: layers are capped at 1 MiB.
- **Plain HTTP**: hosts in `CVCTL_INSECURE_REGISTRIES` use plain HTTP.

A CI template is in [examples/ci/publish-contract.yml](../examples/ci/publish-contract.yml).

## render

Turns an instance directory into the files Docker Compose and Traefik consume.
The directory holds `cvctl.env` (the instance decisions) and `infra/` (a checkout
of the project's infra, containing `compose.yml` and `env.spec.yml`).

```
cvctl render --instance-dir /srv/deploy/demo-dev [--edge edge|behind-proxy]
```

It writes, atomically and with mode `0640`:

- `compose.cvctl.yml`: override file wiring data services, limits, networks and the route aliases
  (routes themselves come from the Traefik file provider, see `traefik.yml`);
- `traefik.yml`: routes, only when some service is exposed;
- `app.env`: resolved variables from `env.spec.yml`, sorted. `random` and `prompt` values already
  set (including with `cvctl set-env`) are kept; `service`, `fixed` and `template` values are
  recomputed from the contract on every render.

Variables of source `prompt` without a default are not an error: they are
reported as `missing` together with a hint, and the render still succeeds. Set the value with
`cvctl set-env <instance> <key> [value]`.

```
$ cvctl render --instance-dir /srv/deploy/demo-dev --json
{"ok":true,"data":{"files":[...],"generated":["JWT_SECRET"],"hint":"cvctl set-env demo-dev SMTP_PASSWORD","instance":"demo-dev","missing":["SMTP_PASSWORD"]}}
```
