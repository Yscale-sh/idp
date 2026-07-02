# docs/

Design notes and references. The two contract documents live at the repo root:
[`CONVENTIONS.md`](../CONVENTIONS.md) (the platform contract) and
[`ARCHITECTURE.md`](../ARCHITECTURE.md) (the target production shape). Everything here supports
them.

The platform runs on one idea: the per-app `deploy.yaml` shopping list is the product. A developer
declares a small app contract (app, runtime, routes, sizing, db, cache, volumes, `connectsTo`) and
the platform derives all Kubernetes from it. Deploying is a Git commit and Flux is the only writer.
Start with [`USAGE.md`](USAGE.md) for the operator workflow.

## Current

| Doc | What |
|---|---|
| [`USAGE.md`](USAGE.md) | **start here**: the operator manual. Day-to-day runbooks and command cheat-sheet |
| [`DEPLOY_GO_CLI.md`](DEPLOY_GO_CLI.md) | `idpctl` design: package layout, command surface, app contract, guardrails |
| [`ENVIRONMENTS.md`](ENVIRONMENTS.md) | environment topology and digest-forward `dev -> prod` promotion model (stage deferred) |
| [`SECRETS.md`](SECRETS.md) | the secrets trust model: platform mints, tenants never hold keys |
| [`ENV.md`](ENV.md) | env-var tiers (A to D): what the platform injects vs. what apps declare |
| [`DIAGRAMS.md`](DIAGRAMS.md) | Mermaid diagrams of the deploy flow and cost-model history |

## History (superseded, kept for the "why")

| Doc | What |
|---|---|
| [`IDP.md`](IDP.md) | the original IDP sketch (ingress-nginx era), superseded by `ARCHITECTURE.md` |
| [`DEPLOY_MODULE.md`](DEPLOY_MODULE.md) | the shell-based deploy module that preceded the Go CLI |
| [`PLATFORM_DX.md`](PLATFORM_DX.md) | early developer-experience explorations, later folded into the contract |

The cloud cost review and migration planning that motivated all of this were personal operational
notes (real account data) and live outside the published tree.
