# Contributing

## Development

- Go version from `go.mod`. No cgo; the binary is static.
- Before a pull request: `gofmt -l .` (must print nothing), `go vet ./...`, `go test ./...`.
- End-to-end tests (`scripts/e2e/`, `.github/workflows/e2e-*.yml`) need a Linux host with
  systemd and Docker; they run in GitHub Actions on every push.

## Pull requests

- One topic per pull request, with tests for the behaviour it changes.
- Commit messages follow Conventional Commits (`feat(deploy): ...`, `fix(init): ...`).
- Code comments explain why or a constraint, not what the code does.
- Never put secret values in argv, logs or error messages; tests should assert it.
- Security issues: see [SECURITY.md](SECURITY.md), not a public issue.

## License

By contributing you agree that your contribution is licensed under the Apache License 2.0.
