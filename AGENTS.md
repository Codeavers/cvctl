# cvctl — agent rules

Go CLI and pull agent that prepares a Linux host with systemd and deploys Docker Compose projects
(GitOps): contract lint/publish as OCI artifacts, data services with encrypted backups (WAL-G,
restic), Traefik with the file provider.

- Read `README.md` first; `docs/` is git-ignored local notes (specs, plans) when present.
- Checks before a PR: `gofmt -l .`, `go vet ./...`, `go test ./...` (see `CONTRIBUTING.md`).
- Nothing organisation-specific in code: registries, domains and tokens come from host config.
- Never put secrets in argv, logs or errors.

@docs/context.md
