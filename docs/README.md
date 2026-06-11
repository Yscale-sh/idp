# docs/

Design notes and references. The two contract documents live at the repo root —
[`CONVENTIONS.md`](../CONVENTIONS.md) (the platform contract) and
[`ARCHITECTURE.md`](../ARCHITECTURE.md) (the target production shape). Everything here supports
them.

## Current

| Doc | What |
|---|---|
| [`DEPLOY_GO_CLI.md`](DEPLOY_GO_CLI.md) | `idpctl` design: package layout, command surface, app contract, guardrails |
| [`SECRETS.md`](SECRETS.md) | the secrets trust model — platform mints, tenants never hold keys |
| [`ENV.md`](ENV.md) | env-var tiers (A–D): what the platform injects vs. what apps declare |
| [`POSTGRES_BACKUPS.md`](POSTGRES_BACKUPS.md) | self-hosted Postgres backup strategy (pg_dump → R2) |
| [`DIAGRAMS.md`](DIAGRAMS.md) | ASCII diagrams of the deploy flow and architectures |

## History (superseded, kept for the "why")

| Doc | What |
|---|---|
| [`IDP.md`](IDP.md) | the original IDP sketch (ingress-nginx era) — superseded by `ARCHITECTURE.md` |
| [`DEPLOY_MODULE.md`](DEPLOY_MODULE.md) | the shell-based deploy module that preceded the Go CLI |
| [`PLATFORM_DX.md`](PLATFORM_DX.md) | early developer-experience explorations, later folded into the contract |

The cloud cost review and migration planning that motivated all of this were personal operational
notes (real account data) and live outside the published tree.
