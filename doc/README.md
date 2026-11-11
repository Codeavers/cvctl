# cvctl documentation

| Topic | Covers |
| --- | --- |
| [Install and prepare a host](install.md) | binary, `cvctl init`, host configuration, notifier setup |
| [Project contract](contract.md) | `compose.yml`, `env.spec.yml`, `lint`, `publish`, OCI source, `render` |
| [Deploy](deploy.md) | `project add/update/list`, `plan`, strategies, timeouts, migrating an existing deploy |
| [Domains and Traefik](domains.md) | domains, edge modes, Traefik, adopting external services |
| [Data services](data.md) | data kinds, `sqlite`, S3 buckets, external mode |
| [Backups and recovery](backups.md) | backups, dumps, restore, disaster recovery, generations, secrets bundle |
| [Operations](operations.md) | status, logs, history, deploy notices, `upgrade`, upgrade notes, `images prune` |
| [Security model](security.md) | who runs what, contract allowlist, isolation, secrets, hardening |

Release history is in [CHANGELOG.md](../CHANGELOG.md).

## Conventions

- Every command accepts `--json` and then prints exactly one JSON document.
- Exit codes: `0` ok, `1` validation or business error, `2` usage error.
- Every error and issue carries a stable `E_<AREA>_<CAUSE>` code and a `hint`.
- `<inst>` or `<instance>` is an instance name such as `demo-dev`; it cannot contain `--`.

## Commands

| Command | Purpose | Docs |
| --- | --- | --- |
| `init` | bring this host to the desired state (idempotent) | [install](install.md#init) |
| `lint`, `publish`, `render` | validate, publish and render a contract | [contract](contract.md) |
| `project add`, `update`, `list`, `restore` | manage instances | [deploy](deploy.md), [backups](backups.md#disaster-recovery-and-restore) |
| `plan` | what this binary would do per instance (read-only, offline) | [deploy](deploy.md#plan) |
| `deploy`, `rollback`, `pause`, `resume` | drive deployments by hand | [deploy](deploy.md#manual-operations) |
| `set-env`, `get-env`, `set-strategy` | change instance settings | [deploy](deploy.md#manual-operations) |
| `status`, `history`, `logs` | inspect instances | [operations](operations.md#status-logs-and-history) |
| `adopt add`, `list`, `rm` | expose services cvctl does not deploy | [domains](domains.md#adopting-external-services) |
| `backup now`, `run`, `list`, `verify` | run and check backups | [backups](backups.md) |
| `db restore`, `cleanup` | restore data services, remove old volumes | [backups](backups.md) |
| `secrets export`, `import`, `remote export` | encrypted secrets bundle | [backups](backups.md#secrets-bundle) |
| `notify test` | send a test notification | [install](install.md#notifications) |
| `upgrade` | install a newer release | [operations](operations.md#upgrade) |
| `images prune` | remove images no instance needs | [operations](operations.md#images-prune) |
| `agent` | the deploy loop run by `cvctl-agent` | [install](install.md#init) |
| `version` | print the embedded tag | [operations](operations.md#status-logs-and-history) |
