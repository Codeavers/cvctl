# Operations

## Status, logs and history

| Command | What it does |
| --- | --- |
| `cvctl status [instance]` | deployment status of instances, with their strategy; adopted services show as `ADOPTED` |
| `cvctl history <instance> [-n <count>]` | deployment history (default 20 events), including the services swapped blue-green per deploy |
| `cvctl logs <instance> [service] [--tail <lines>]` | container logs (default 100 lines) |
| `cvctl project list` | instances with their contract source, domains and data modes, see [deploy.md](deploy.md#project-list) |
| `cvctl plan` | what this binary would do for each instance, see [deploy.md](deploy.md#plan) |
| `cvctl version` | the embedded release tag |

## Deploy notices

Telegram deploy notices show the PR, commit, title and author of what was deployed. cvctl reads
them from the labels of the changed images, so the build must set `cvctl.repo`, `cvctl.commit`,
`cvctl.pr`, `cvctl.title` and `cvctl.author` (`cvctl.pr`, `cvctl.title` and `cvctl.author` may be
empty). `cvctl.repo` is the repository URL (`https://github.com/org/app`); the notice links the
PR and commit from it, and a value that is not an https URL drops the links. `cvctl.author` is the
commit author name. [examples/ci/image-labels.yml](../examples/ci/image-labels.yml) sets them with
`docker/build-push-action`, taking the PR number and title from the merge or squash commit.

Without labels the notice still goes out with the result only. Labels are read with a 5 second
timeout; a failed read never fails the deploy.

A failed deploy adds an `Error` line with the cause and a `Logs` line with the history command.
When there is something to do, a `Next` line gives the command: `sudo cvctl resume <instance>` after
a manual rollback, or `cvctl db restore <instance> <service> --to <id>` when a rolled back deploy
had already run its migrations.

`info` notices are silent: successful deploys, the pause after a manual rollback, finished restores
and `cvctl notify test`. `warn` and `error` notices make a sound.

Setting up the notifier is in [install.md](install.md#notifications).

## upgrade

`sudo cvctl upgrade [--version vX.Y.Z] [--check] [--yes] [--accept-changes]` replaces the installed
`/usr/local/bin/cvctl` with the latest stable release, verifies its checksum, restarts `cvctl-agent`
and watches it for 30 s; if it does not hold, the previous binary is put back (`E_UPGRADE_FAILED`).
The previous binary stays in `/usr/local/bin/cvctl.previous`.

It needs root, `--check` included (`E_UPGRADE_ROOT`). It refuses while any instance is `DEPLOYING` and
while another `cvctl upgrade` runs (`E_UPGRADE_BUSY`). It replaces only the binary: it does not run
`cvctl init` or `cvctl project update`, so systemd units and host config are not refreshed; run them
yourself if the release notes ask for it (see [Upgrade notes](#upgrade-notes)).

If `cvctl-agent` is not active (`inactive` or `failed`), the binary is installed and the agent is left
as it was: no restart and no watch, so nothing has tested the new binary. The output and the notice say
so, and `--json` reports `agent_stopped`. While the agent is starting or stopping, or if its state
cannot be read, nothing is installed (`E_UPGRADE_INSTALL`).

Before installing it shows both versions, the release notes and the differences between what the
current and the new binary would do per instance ([`cvctl plan`](deploy.md#plan)). To get them, the
downloaded binary, once its checksum matches, runs as root before any confirmation, even with
`--check`. The checksum catches a corrupt download, not a compromised release.

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

## Upgrade notes

Steps a release needs beyond `cvctl upgrade`. The full list of changes is in
[CHANGELOG.md](../CHANGELOG.md).

### Upgrading to v0.8.0

`s3` services stop being backed up unless the contract sets `cvctl.backup: volume`. For a service
without it, its restic repository `<bucket>/<host>/<inst>/<svc>` (with any `r<N>` under it) is no
longer used; it still holds the restore points taken before the upgrade, so delete it by hand only
once those are no longer needed. With `volume` it stays in use, so keep it.

### Upgrading a host to v0.7

Requires Docker Engine 28 or later (`E_DOCKER_TOO_OLD`). Instance names containing `--` must be
renamed first: `E_INSTANCE_NAME`, and bundle import rejects them. Run, in this order:

1. With the new binary, `cvctl lint` each instance's infra dir and fix and publish what it reports
   (forbidden compose fields, service names that are not `^[a-z0-9]+([-_][a-z0-9]+)*$`).
2. Check that the app images run without capabilities: no port below 1024, no setuid at start.
3. Install the binary.
4. `cvctl project update <inst>` for every instance: per-instance backup keys (the backup
   generation is bumped, see [Backup generations](backups.md#backup-generations); failures are
   `E_BACKUP_PROVISION`), the `<inst>_edge` and `<inst>_backup` networks, and connecting Traefik
   and the storage silo to them.
5. `cvctl init` last: Traefik moves to `cvctl-edge-proxy` (`10.203.0.0/24`,
   `E_TRAEFIK_EDGE_SUBNET` if it clashes) and the legacy `cvctl-edge` and `cvctl-backup` networks
   are retired once nothing else uses them.
6. `cvctl secrets export`, so the bundle holds the new keys.
7. Registry logins other than the token host (`CVCTL_REGISTRY_HOST`, else the first
   `CVCTL_REGISTRIES` host) are pruned automatically by the agent on start.

## images prune

`sudo cvctl images prune [--dry-run] [--instance <inst>]` removes the images that deploys left
behind. Per service it keeps the current version and the 2 previous ones that really ran, 3 in
total, plus every digest of an instance that is deploying or has no state yet. After each
successful deploy the same cleanup runs on its own, so the command is mostly for hosts that already
hold many images.

Only images whose digest cvctl recorded in an instance's history are touched; other digests on the
host are never listed or removed. An image with a tag is never touched, in any repository: a tag on
a digest protects that digest everywhere. An untagged image of another repository with the same
digest can be removed, since it is the same content. Images in use by a container are skipped
(Docker refuses them).

A rollback to a version beyond those kept pulls the image again, so it needs the image to still be
in the registry. If the pull fails, the rollback stops with an error that names the image, and
nothing changes. `--instance` limits what is removed to that instance's images, but the keep list
still covers every instance. If any instance's state cannot be read, nothing is removed.

On a host with many images run `--dry-run` first: it lists each image with its size and the reason,
plus the total, without removing anything.
