# Install and prepare a host

cvctl runs on Linux with systemd. On Ubuntu and Debian `cvctl init` installs and hardens everything;
on other distributions install Docker yourself and cvctl skips what the host lacks.

## Get the binary

Each GitHub release attaches static binaries for `linux/amd64` and `linux/arm64`
(`cvctl-linux-amd64`, `cvctl-linux-arm64`) and a `sha256sums.txt`:

```
curl -fsSLO https://github.com/Codeavers/cvctl/releases/latest/download/cvctl-linux-amd64
curl -fsSLO https://github.com/Codeavers/cvctl/releases/latest/download/sha256sums.txt
sha256sum --ignore-missing -c sha256sums.txt
install -m 0755 cvctl-linux-amd64 ./cvctl
```

### From GHCR

Release images are `FROM scratch` (no `cat`), so copy the binary out of a stopped container:

```
id=$(docker create ghcr.io/codeavers/cvctl:<v>); docker cp "$id":/cvctl ./cvctl; docker rm "$id"
```

`docker login ghcr.io` first with the host token. `cvctl version` prints the embedded tag.

`cvctl init` installs the binary it runs from to `/usr/local/bin/cvctl` (owner `root`, mode `0755`)
and runs it as the `cvctl-agent` systemd service. Later versions are installed with
[`cvctl upgrade`](operations.md#upgrade).

## init

Prepares a Linux host with systemd. On Ubuntu and Debian it installs and hardens everything;
elsewhere install Docker yourself and cvctl skips what the host lacks. It adopts an existing
server to the desired state idempotently: packages, docker daemon, accounts (`deployer` user), SSH hardening
with auto-revert timer, fail2ban, firewall (UFW), unattended security upgrades, Traefik v3
reverse proxy (file provider only, no Docker socket), backup storage (local silo or S3),
and notifications.

```
sudo cvctl init
sudo cvctl init --dry-run
sudo cvctl init --adopt
sudo cvctl init --non-interactive --json
sudo cvctl init --reconfigure notify
```

- `--dry-run` prints the plan without changing anything.
- `--adopt` adopts an already-configured host with conservative defaults (edge mode
  `behind-proxy`, firewall not managed).
- `--non-interactive` never prompts: it uses `host.env` values and defaults, and reports what is
  still missing as `E_INIT_MISSING`.
- `--reconfigure notify` asks the notification questions again (see [Notifications](#notifications)).

SSH changes are applied with a revert timer: if the new session cannot be confirmed, the previous
sshd config comes back on its own. `cvctl init confirm-ssh` confirms the hardening and cancels the
timer.

## Host configuration

init only asks for values `host.env` and `secrets/` lack. Answers are stored in
`/etc/cvctl/host.env` (keys with the `CVCTL_` prefix) and secrets in `/etc/cvctl/secrets/<name>`.
Re-running init never asks again for a key that already has a value.

| Key | Asked when | Values |
| --- | --- | --- |
| `HOST_NAME` | always | DNS label, defaults to the hostname |
| `EDGE_MODE` | always | `edge` (Traefik terminates TLS on 80/443) or `behind-proxy` (another proxy forwards to Traefik) |
| `PROXY_PORT` | `behind-proxy` | local port Traefik listens on, default `8081` |
| `ADMIN_USER` | always | admin system user, defaults to the sudo user |
| `SSH_MANAGE` | always | `true` / `false`: let cvctl manage SSH hardening |
| `FIREWALL_MANAGE` | always | `true` / `false`: let cvctl manage the firewall |
| `ACME_EMAIL`, `ACME_CHALLENGE` | `edge` | Let's Encrypt email; challenge `http`, `route53` or `cloudflare` |
| `STORAGE` | always | backup storage: `local` (silo, S3-compatible, on the host) or `s3` |
| `S3_ENDPOINT`, `S3_BUCKET`, `S3_REGION` | `s3` | off-site backup bucket (secrets `s3_access_key`, `s3_secret_key`) |
| `NOTIFY` | always | `telegram` or `none` |
| `TELEGRAM_CHAT_ID`, `TELEGRAM_THREAD_ID` | `telegram` | see [Notifications](#notifications) |
| `REGISTRIES`, `REGISTRY_USER` | always, optional | image registries, comma-separated (e.g. `ghcr.io/<org>`); secret `registry_token`; empty means public images |

The DNS challenges take their credentials as secrets: `route53_access_key` and
`route53_secret_key`, or `cloudflare_dns_token`. Other `host.env` keys are optional and documented
where they apply: timeouts and Traefik's read timeout in [deploy.md](deploy.md#timeouts) and
[domains.md](domains.md#traefik), `CVCTL_UPGRADE_REPO` in [operations.md](operations.md#upgrade).

## Notifications

`--reconfigure notify` asks the notification questions again (current values as defaults; an empty
token keeps the stored one, an empty thread ID removes it). When the settings change, init sends a
test message and restarts `cvctl-agent` so it uses them; disabling notifications only restarts the
agent. The agent and the backup timers log notifier errors (bad configuration, failed sends) to the
journal.

The Telegram chat ID is numeric (groups and channels are negative) or a public `@channelname`. From
a topic link `t.me/c/<id>/<thread>` the chat ID is `-100<id>` and the optional thread ID is
`<thread>`.

`cvctl notify test` sends a test notification. What the notices contain and how to label images
for them is in [operations.md](operations.md#deploy-notices).
