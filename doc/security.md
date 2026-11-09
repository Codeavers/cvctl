# Security model

How cvctl limits what a project, a host user and a network peer can do. To report a vulnerability,
see [SECURITY.md](../SECURITY.md); do not open a public issue.

## Who runs what

- **root** (`sudo`): `cvctl init`, `cvctl secrets`, `cvctl upgrade` and `cvctl adopt` refuse to
  run otherwise. The examples in these docs also run `project add`, `resume` and `images prune`
  with `sudo`.
- **`deployer`** (group `deploy`): the `cvctl-agent` service, which pulls and deploys. Its systemd
  unit sets `NoNewPrivileges=yes`, `ProtectSystem=strict` and `PrivateTmp=yes`, can write only
  `/srv/deploy`, `/var/lib/cvctl` and `/etc/traefik/dynamic`, and gets the `docker` group as a
  supplementary group. The backup timers run with the same sandbox.
- Host config lives in `/etc/cvctl` and `/etc/cvctl/secrets` (`root:deploy`, mode `0750`);
  instances live in `/srv/deploy/<instance>/` (`deployer:deploy`, `/srv/deploy` mode `2750`).

## Contract allowlist

`compose.yml` is read for an allowlist of fields only and is never passed to `docker compose`;
cvctl generates the compose file it runs, and unknown or unsafe fields fail with
`E_CONTRACT_FIELD_FORBIDDEN` (see [contract.md](contract.md)). App containers run with every
capability dropped and `no-new-privileges`, so images must not bind a port below 1024 or need
setuid at start. Data services get only the capabilities their kind needs, and the `sqlite`
helpers run with no network and no capabilities (`CHOWN` only for the one that hands a volume
over).

## Isolation between instances

Each instance gets its own networks, `<inst>_edge` (Traefik) and `<inst>_backup` (WAL-G and the
backup user `bk-<inst>` with policy `cvctl-bk-<inst>`), so instances cannot reach each other and
each instance's backups use their own credentials. A local `s3` service gives the app a user
scoped to its buckets, never the silo root keys (see [data.md](data.md#s3-buckets)).

## Secrets

- Secret values never go into argv, logs or error messages.
- `app.env` and the rendered files are written with mode `0640`.
- The secrets bundle is age-encrypted, written with mode `0600`, and its plaintext only lives in
  memory (see [backups.md](backups.md#secrets-bundle)).
- `cvctl plan` output holds no `app.env` values or `cvctl.env` credentials.
- `project add --env-file` prints the keys it used and ignored, never their values.
- The publish token goes only to the registry host of the ref.
- Registry logins other than the token host are pruned by the agent on start.
- `--ext` values land in the shell history and the process list; prefer the interactive prompt for
  keys on a shared host.
- Backups are encrypted: env files with the host restic key, WAL-G archives with a libsodium key.

## Traefik without the Docker socket

Traefik v3 uses the file provider only and gets no Docker socket: cvctl writes the routes, so a
compromised proxy cannot drive Docker. Its access log is JSON without `RequestPath`, since paths
can carry tokens. See [domains.md](domains.md#traefik).

## Host hardening

`cvctl init` applies, when the host allows it:

- SSH hardening with an auto-revert timer (`SSH_MANAGE`): if the new session cannot be confirmed,
  the previous sshd config comes back on its own.
- fail2ban and unattended security upgrades.
- UFW (`FIREWALL_MANAGE`): deny incoming, allow outgoing, allow the SSH ports and, in `edge` mode,
  80 and 443. If the SSH port cannot be determined it refuses to enable the firewall
  (`E_FIREWALL_FAILED`). Without `ufw` the firewall is not managed: allow SSH, 80 and 443
  yourself. An adopted service's port opens to Traefik's subnet only.

## Upgrades

`cvctl upgrade` verifies the release checksum, which catches a corrupt download, not a compromised
release. The downloaded binary runs as root to compute the plan before any confirmation, even with
`--check` (see [operations.md](operations.md#upgrade)).
