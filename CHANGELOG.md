# Changelog

All notable changes to cvctl are documented here. The format is based on
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and the project uses
[Semantic Versioning](https://semver.org/) (still `0.x`: minor versions may need manual steps).
Steps a release needs beyond `cvctl upgrade` are in
[doc/operations.md](doc/operations.md#upgrade-notes).

## [Unreleased]

### Changed

- Documentation moved from the README into [doc/](doc/README.md); added this changelog, a code
  of conduct and issue and pull request templates.

## [0.8.6] - 2026-11-09

### Added

- `cvctl adopt add|list|rm` exposes a service cvctl does not deploy (systemd unit, manual process,
  another Docker stack) through Traefik with TLS, without a contract. `cvctl status` lists it as
  `ADOPTED`, its domain is reserved, and `cvctl init` regenerates its routes.

## [0.8.5] - 2026-11-06

### Added

- `cvctl.backup: dump` strategy: restore points before prod deploys are full dumps in restic,
  with `complete`/`partial` snapshot tags, restore through a temporary engine and `backup verify`.
- `sqlite` kind: a managed volume mounted in one app, backed up with `sqlite3 .backup`, restored
  with `PRAGMA integrity_check`, and owned by `cvctl.uid` or the image's numeric `USER`.

### Fixed

- Service credentials are redacted in dumps, and an incomplete dump is never taken as valid.
- A lost sqlite database is reported as `E_SQLITE_LOST`; its volume is handed back after a partial
  `chown`, and the owner app is stopped before the owner changes.

## [0.8.4] - 2026-11-05

### Fixed

- The agent no longer sends an error notice when it is restarted.

## [0.8.3] - 2026-11-05

### Added

- `cvctl upgrade`: installs the latest (or `--version`) release after a checksum check, shows the
  release notes and the per-instance `cvctl plan` differences, asks for confirmation and puts the
  previous binary back if the agent does not hold.
- `cvctl plan`: read-only, offline description of what the binary would do per instance, with the
  codes a deploy would fail on.
- `cvctl images prune`, and the same cleanup after each successful deploy: keeps the current and
  two previous versions that ran per service.
- Telegram notices get one Open button per app with a domain.

### Fixed

- A manual rollback pulls the images it needs first.
- `cvctl upgrade` checks root before reading the host config, validates `CVCTL_UPGRADE_REPO`
  from the environment, refuses to run twice at once and does not start an agent that was
  stopped on purpose.

## [0.8.2] - 2026-11-04

### Added

- Telegram notices with the PR, commit, title and author read from image labels, an `Error` line
  with the cause and a `Next` line with the command to run; `info` notices are silent.
- `cvctl init --reconfigure notify`; the agent and backup timers log notifier failures.
- `cvctl project update --domain` changes the domain of an exposed service.
- `CVCTL_TRAEFIK_READ_TIMEOUT`; Traefik's access log no longer records request paths.

### Changed

- GitHub Actions run only after a merge to `main`; a local pre-push hook runs the checks.

### Fixed

- `init` sets `default-address-pools` with `/24` networks in `daemon.json`.
- The deployer's `git` runs from `/`.
- Disabling the notifier restarts the agent; `notify.ok` is updated only after the restart.

## [0.8.1] - 2026-10-22

### Added

- `blue-green` deploy strategy for exposed services (default for prod).

## [0.8.0] - 2026-10-21

### Added

- Each instance's env files (`cvctl.env`, `app.env`, deploy key) are backed up with the host key,
  and `cvctl project restore` recovers them from the off-site backup.
- `cvctl secrets export --instance` and `--host`.
- `init` checks host capabilities instead of the distribution name.

### Changed

- `s3` services are no longer backed up unless the contract sets `cvctl.backup: volume`
  (see the [upgrade notes](doc/operations.md#upgrading-to-v080)).

### Fixed

- Traefik no longer overwrites the app's `Referrer-Policy`.

## [0.7.3] - 2026-10-21

### Fixed

- `init` accepts Ubuntu 24.04 or later (including 26.04) and checks the Docker repository for the
  release codename.

## [0.7.2] - 2026-10-19

### Added

- `cvctl project list`.
- `cvctl project add --env-file` seeds `random` and `prompt` vars from an existing `.env`.

## [0.7.1] - 2026-10-19

### Added

- `postgis` kind with WAL-G backups.

### Fixed

- The release image ships CA roots, so `publish` can reach the registry.

## [0.7.0] - 2026-10-18

First tagged release: `init` host setup, contract `lint`/`publish`/`render`, OCI and git contract
sources, the pull agent with the `recreate` strategy, migrations, health checks and rollback,
`postgres`, `redis` and `s3` data services, WAL-G and restic backups with restore points and
verification, `db restore`, `project restore`, the secrets bundle, per-instance networks and
Telegram notices. Upgrading an existing host needs the steps in the
[upgrade notes](doc/operations.md#upgrading-a-host-to-v07).

[Unreleased]: https://github.com/Codeavers/cvctl/compare/v0.8.6...HEAD
[0.8.6]: https://github.com/Codeavers/cvctl/compare/v0.8.5...v0.8.6
[0.8.5]: https://github.com/Codeavers/cvctl/compare/v0.8.4...v0.8.5
[0.8.4]: https://github.com/Codeavers/cvctl/compare/v0.8.3...v0.8.4
[0.8.3]: https://github.com/Codeavers/cvctl/compare/v0.8.2...v0.8.3
[0.8.2]: https://github.com/Codeavers/cvctl/compare/v0.8.1...v0.8.2
[0.8.1]: https://github.com/Codeavers/cvctl/compare/v0.8.0...v0.8.1
[0.8.0]: https://github.com/Codeavers/cvctl/compare/v0.7.3...v0.8.0
[0.7.3]: https://github.com/Codeavers/cvctl/compare/v0.7.2...v0.7.3
[0.7.2]: https://github.com/Codeavers/cvctl/compare/v0.7.1...v0.7.2
[0.7.1]: https://github.com/Codeavers/cvctl/compare/v0.7.0...v0.7.1
[0.7.0]: https://github.com/Codeavers/cvctl/releases/tag/v0.7.0
