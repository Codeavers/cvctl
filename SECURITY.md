# Security policy

## Supported versions

Security fixes go into the latest release only. Upgrade to the newest `v0.x` before reporting.

## Reporting a vulnerability

Do not open a public issue. Report it privately through GitHub:
**Security → Report a vulnerability** on this repository (private vulnerability reporting).

Include the cvctl version, the host OS, what an attacker can do, and steps to reproduce. You will
get an answer within 7 days. Please give us a reasonable time to ship a fix before disclosing.

## Scope

cvctl runs as root (`init`, `secrets`) and as the `deployer` user (agent, deploys) on hosts that
run other people's containers. In scope, among others: escaping the contract allowlist, reading
another instance's secrets or data, secrets in logs, argv or errors, registry token leaks, and
anything that weakens SSH, firewall or Traefik settings that `cvctl init` applies.
