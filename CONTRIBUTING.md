# Contributing

## Development

- Go version from `go.mod`. No cgo; the binary is static.
- Before a pull request: `gofmt -l .` (must print nothing), `go vet ./...`, `go test ./...`.
  Pull requests get no CI run, so enable the pre-push hook that runs them once per clone:
  `git config core.hooksPath .githooks`.
- End-to-end tests (`scripts/e2e/`, `.github/workflows/e2e-*.yml`) need a Linux host with
  systemd and Docker. Each runs in GitHub Actions after a merge to `main` only when the merge
  touches its `paths` filter, and on demand with `workflow_dispatch`.

## Pull requests

- One topic per pull request, with tests for the behaviour it changes.
- Commit messages follow Conventional Commits (`feat(deploy): ...`, `fix(init): ...`).
- Code comments explain why or a constraint, not what the code does.
- Never put secret values in argv, logs or error messages; tests should assert it.
- Security issues: see [SECURITY.md](SECURITY.md), not a public issue.

## License

By contributing you agree that your contribution is licensed under the Apache License 2.0.
