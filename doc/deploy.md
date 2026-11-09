# Deploy

An instance is one project in one environment (`dev` or `prod`) on the host, under
`/srv/deploy/<instance>/`. The `cvctl-agent` service pulls the desired state and deploys it; the
commands below add and inspect instances.

## project add

```
sudo cvctl project add oci://ghcr.io/<org>/<repo>-infra --env dev --domain web=app.example.com
```

`project add <repo-url>` takes a git URL or `oci://host/repository` (see
[contract.md](contract.md#publish-and-the-oci-source)) and adds and deploys a new instance.

| Flag | Meaning |
| --- | --- |
| `-e, --env` | target environment, `dev` (default) or `prod` |
| `-n, --name` | override the project name |
| `-b, --branch` | infra git branch to clone |
| `-t, --tag` | image tag to track (dev only) |
| `--infra-path` | contract directory inside the infra git repo (monorepos) |
| `--strategy` | `recreate` or `blue-green`, see [Deploy strategy](#deploy-strategy) |
| `--domain svc=host` | domain for an exposed service (repeatable) |
| `--ext svc.field=value` | pre-seed external service configuration (repeatable), see [data.md](data.md) |
| `--set KEY=VALUE` | pre-seed `app.env` values (repeatable) |
| `--env-file <path>` | take values from an existing `.env`, see [Migrating an existing deploy](#migrating-an-existing-deploy) |
| `--non-interactive` | never prompt; use defaults and `--set` values |

## project update

`cvctl project update <instance>` refreshes an existing instance (networks, backup keys, generated
decisions). `--domain svc=host` changes the domain of an exposed service and `--ext
svc.field=value` pre-seeds external service configuration; `--non-interactive` never prompts.

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
no docker) and prints its env, strategy, contract source, tracked tag, domains, data service modes and
whether it is paused. An instance whose contract cannot be read is listed with the error.
`cvctl status` shows the strategy too.

## plan

`cvctl plan [--json]` describes what this binary would do for each instance, from its `cvctl.env`,
installed contract and embedded `kinds` catalog (read-only, no network, no docker). Per instance:
`env`, `strategy`, `apps` (image without digest, exposed port, domain), `data` (`kind`, `version`,
`mode`, `backup`, buckets), `health`, hook counts and `issues`, the sorted codes of the contract and
decision checks a deploy would fail on (codes only, omitted when empty; app digests are not required,
because a deploy gets them from the desired state).

The document has a `schema` number, sorted keys and no secrets (no `app.env` values or
`cvctl.env` credentials), so two runs of the same binary are byte-equal and two versions can be
diffed. An unreadable instance appears with `error`. [`cvctl upgrade`](operations.md#upgrade) uses
it to show what a new release would change.

## Deploy strategy

`recreate` (default for dev) replaces containers in place: a few seconds of downtime per deploy.
`blue-green` (default for prod) swaps the services that carry `cvctl.expose` without downtime.
`project add --strategy recreate|blue-green` sets it explicitly (interactive or not; without the
flag: prod gets `blue-green`, everything else `recreate`) and `cvctl set-strategy <instance>
<strategy>` changes it later. An instance with a `sqlite` service only runs `recreate` (see
[data.md](data.md#sqlite)).

Blue-green, per exposed service, in contract order:

1. Compose scales it to 2 (the old container keeps running and a new one starts with the new
   config).
2. The new container must pass its health check: the HTTP probe of `x-cvctl.health` for the
   health service, only running with no restarts for the other services.
3. Traefik's route is pinned to the new container only.
4. The old one is stopped with its `stop_grace_period` (10s by default) and removed.
5. The route goes back to the service alias, which now resolves only to the new container.

If the new container fails that check it is removed and the old one never stops; a service other
than the health service that runs but answers HTTP errors still replaces the old one. Workers, data
services and non-exposed apps are recreated as in `recreate`. The first deploy of an instance is a
plain `up`. `cvctl history` lists the services swapped blue-green per deploy.

Constraints:

- Memory of a swapped service doubles while both containers run.
- Migrations run before the swap, so they must be backward compatible with the old version:
  expand in one deploy, contract in a later one.
- Websocket connections on the old container close when it stops (clients must reconnect).

## Timeouts

Defaults and `host.env` keys:

| Step | Default | Key |
| --- | --- | --- |
| fetch | 2m | `CVCTL_TIMEOUT_FETCH` |
| pull | 10m | `CVCTL_TIMEOUT_PULL` |
| each hook | 15m | `CVCTL_TIMEOUT_HOOK` (`x-cvctl.hooks[].timeout` can only lower it) |
| whole deploy | 45m | `CVCTL_TIMEOUT_DEPLOY` |

Health waits use `x-cvctl.health.timeout` (at most 10m) plus 2m. Exceeding one fails with
`E_TIMEOUT`. Fetch, pull and deploy timeouts are retried on the next cycle; a hook timeout fails
that desired state until it changes.

Traefik's request read timeout is in [domains.md](domains.md#traefik).

## Manual operations

| Command | What it does |
| --- | --- |
| `cvctl deploy <instance>` | deploy an instance now |
| `cvctl rollback <instance> [--to <n>]` | roll back to a previous state (`--to`: history index) |
| `cvctl pause <instance> [--reason <text>]` | pause deployments for an instance |
| `cvctl resume <instance>` | resume deployments, e.g. after a manual rollback |
| `cvctl set-env <instance> <key> [value]` | set a variable in `app.env` |
| `cvctl get-env <instance>` | print the variables of `app.env` |
| `cvctl set-strategy <instance> <strategy>` | change the deploy strategy |

Status, logs and history are in [operations.md](operations.md#status-logs-and-history).
