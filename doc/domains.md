# Domains, Traefik and adopted services

## Domains

A service is exposed by the `cvctl.expose: "<port>"` label in the contract. Its domain is a host
decision, not part of the contract: `project add --domain <svc>=<host>` sets it and
`project update <instance> --domain <svc>=<host>` changes it (both repeatable). An instance and an
[adopted service](#adopting-external-services) cannot share a domain.

## Edge modes

`EDGE_MODE` in `host.env` (asked by `cvctl init`):

- `edge`: Traefik terminates TLS on ports 80 and 443 with Let's Encrypt certificates. The ACME
  challenge is `http`, `route53` or `cloudflare` (`ACME_CHALLENGE`).
- `behind-proxy`: another proxy forwards to Traefik on a local port (`PROXY_PORT`, default
  `8081`). It is the default with `cvctl init --adopt`.

`cvctl render --edge edge|behind-proxy` renders an instance for either mode.

## Traefik

Traefik v3 runs with the file provider only; it gets no Docker socket. cvctl writes each
instance's routes to a `traefik.yml` (see [contract.md](contract.md#render)), and Traefik reaches
the instance through the instance's own `<inst>_edge` network. Traefik itself lives on
`cvctl-edge-proxy` (`10.203.0.0/24`, `E_TRAEFIK_EDGE_SUBNET` if it clashes).

Traefik's `readTimeout` (how long a request body may take to arrive) is Traefik's default, 60s,
unless `CVCTL_TRAEFIK_READ_TIMEOUT` sets it (e.g. `15m` for large uploads). It applies to the
`web` and `websecure` entrypoints; re-run `cvctl init` to apply it. Traefik's access log is JSON
without `RequestPath`, since paths can carry tokens.

## Adopting external services

`cvctl adopt` exposes a process cvctl does not deploy, such as a systemd unit, a manual Node process or
another Docker stack, without a contract:

```
sudo cvctl adopt add panel --domain panel.example.com --target https://host:9090 --insecure-tls
sudo cvctl adopt list
sudo cvctl adopt rm panel
```

- `--target` is `http://host:<port>` or `https://host:<port>`.
- `--insecure-tls` does not verify the backend certificate.
- `--force` adopts even if the target does not answer now.

cvctl only adds the Traefik route and TLS, and `cvctl status` lists the service as `ADOPTED` without
checking it. It does not start, update, notify about or back up what it adopts, and `adopt rm` leaves the
process running. `cvctl init` regenerates the routes of adopted services.

`host` is the gateway of Traefik's network, so the process cannot listen on `127.0.0.1` only: `adopt add`
fails with `E_ADOPT_UNREACHABLE` unless it answers there. `--force` skips that check for a stopped process.
With `CVCTL_FIREWALL_MANAGE=true` and `ufw` installed, cvctl allows the port from Traefik's subnet;
it removes no other rule, and without `ufw` it only warns, so check the effective rules (`ufw status`)
to keep clients from bypassing Traefik. Domains are exclusive: an instance
and an adopted service cannot share one. HTTP only; raw TCP or UDP ports stay out.
