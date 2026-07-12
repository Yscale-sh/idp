# Documentation

The per-app `deploy.yaml` shopping list is the product. The platform derives Kubernetes state from
that contract, commits desired state, and leaves reconciliation to Flux.

## Start here

| Doc | Purpose |
|---|---|
| [USAGE.md](USAGE.md) | Operator workflow, commands, and troubleshooting |
| [DEPLOY_GO_CLI.md](DEPLOY_GO_CLI.md) | `idpctl` design, package layout, and command surface |
| [ENVIRONMENTS.md](ENVIRONMENTS.md) | Environment topology and digest-forward promotion |
| [SECRETS.md](SECRETS.md) | Secrets trust model and External Secrets flow |
| [ENV.md](ENV.md) | Generic environment-variable ownership contract |
| [DIAGRAMS.md](DIAGRAMS.md) | Platform, promotion, secret, seam, and module diagrams |

The authoritative contracts are [CONVENTIONS.md](../CONVENTIONS.md) and
[ARCHITECTURE.md](../ARCHITECTURE.md). Private migration inventories, live account data, rendered
platform state, and customer deployment manifests intentionally live outside this repository.
