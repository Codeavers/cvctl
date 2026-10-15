# cvctl

Pull-based deploy agent and CLI for Docker Compose on a single VPS.

Status: plan 1 (core). Two offline commands exist today: `lint` and `render`.
The deploy agent arrives in later plans.

Every command accepts `--json` and then prints exactly one JSON document.
Exit codes: `0` ok, `1` validation or business error, `2` usage error.
Every error and issue carries a stable `E_<AREA>_<CAUSE>` code and a `hint`.

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
