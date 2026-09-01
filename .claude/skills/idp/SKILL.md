---
name: idp
description: >-
  Use idpctl to validate, render, deploy, promote, or inspect apps on this
  self-hosted k3s platform. Read this before running idpctl.
---

# Manage apps with idpctl

This repository contains the Yscale IDP platform. `idpctl` turns a `deploy.yaml`
app contract into reconciled Kubernetes state. The platform derives namespaces,
secret references, environment variables, probes, resource limits, autoscaling,
and observability configuration.

Deployments are Git commits, not `kubectl apply` or `helm upgrade` operations.
`idpctl render` updates `clusters/<env>/platform.yaml`. Commit that file on the
environment's `flux.branch`; Flux then reconciles it into the cluster. Flux is
the only cluster writer.

`charts/cluster` renders an isolated HelmRelease for each app, store, and enabled
module. `disableWait` prevents one failing app from blocking its siblings.

Use these references as needed:

- `docs/USAGE.md`: operator runbooks and command reference
- `README.md`: platform overview and lifecycle
- `CONVENTIONS.md`: platform contract
- `docs/DEPLOY_GO_CLI.md`: CLI design, package layout, and guardrails
- `docs/ENVIRONMENTS.md`: environment topology and promotion
- `docs/SECRETS.md` and `docs/ENV.md`: secret and environment handling
- `schemas/deploy.schema.json`: contract schema
- `internal/appconfig`: authoritative Go implementation of the contract
- `examples/`: worked single-service and multi-component apps

## Workflow

1. Run `command -v idpctl || ls ./idpctl`. If the CLI is unavailable, use
   `make build`. Prefer a fresh build because a checked-in binary may target a
   different architecture.
2. Confirm the target environment. `--env` defaults to `dev`.
3. Start with the relevant read-only command.

   | Intent | Command | Writes? |
   |---|---|---|
   | Check a contract is valid | `idpctl validate --file deploy.yaml` | no |
   | See what a deploy would change | `idpctl plan --env <env> --file deploy.yaml --image <ref>` | no |
   | Look at what is deployed | `idpctl catalog --env <env> [--format json]` | no |
   | Health-check the live cluster's seams | `idpctl doctor --env <env>` | no (reads cluster) |
   | Scaffold a new app | `idpctl new app --name <name> [--dir <path>]` | writes a starter file |
   | Scaffold an env for a fork | `idpctl init --env <env>` | writes `environments/<env>/cluster.yaml` |
   | Deploy an app to an env | `idpctl render --env <env> -f deploy.yaml --image <ref>` then commit | mutates `platform.yaml` |
   | Promote dev to prod without rebuilding | `idpctl promote <app> prod --from dev -f deploy.yaml` then commit | mutates `platform.yaml` |
   | Drop an app or component | `idpctl remove --env <env> --app <name> [--component <c>]` | mutates `platform.yaml` |
   | Set enabled infra modules | `idpctl infra render --env <env>` | mutates `platform.yaml` |
   | Build and push an image (no local Docker) | `idpctl build --repo <org/name> --ref <sha> --image <repo:tag>` | in-cluster job |
   | Cloudflare DNS or tunnel | `idpctl dns sync\|prune` and `idpctl tunnel up\|down --env <env>` | touches Cloudflare |

4. Before rendering, run `validate` and `plan`.
5. Commit only the changed `clusters/<env>/platform.yaml` on the environment's
   `flux.branch`.
6. Confirm the result with `catalog` or `doctor`.

## deploy.yaml contract

```yaml
app: carshowdb
runtime: { image: ghcr.io/yscale-sh/carshowdb-api, port: 8080 }   # port 0 means a worker (no Service or probes)
routes:  [{ host: carshowdb, public: true }]   # public route: Cloudflare Tunnel in prod, MetalLB LAN LoadBalancer in dev
sizing:  { profile: minimal, replicas: 2, autoscale: { enabled: true, max: 5 } }
db:      [{ name: primary, type: postgres, size: minimal }]   # platform provisions and wires DATABASE_URL
cache:   [{ name: default, type: redis }]                     # wires REDIS_URL
```

Required keys are `app` and `runtime`. A multi-component app uses one file with
a `components:` list. The shared base and component overrides expand into an
isolated HelmRelease per component.

- `db` or `cache` with `provision: false` shares a sibling component's store.
- `connectsTo[]` injects an environment-specific dependency address using
  `clusterService`, `publicRoute`, or `serviceToken`.
- `scheme: none` produces a bare `host:port` value.
- `volumes[]` supports `nfs`, `emptyDir`, and `pvc`.
- `probes.type` supports `http` (default), `tcp`, and `none`.

See `docs/DEPLOY_GO_CLI.md` and `schemas/deploy.schema.json` for the complete
field list.

## Lifecycle

- Development is continuous. The in-cluster `idp-shipper` polls registered app
  branches, builds changed inputs with rootless BuildKit, renders components into
  `clusters/dev/platform.yaml`, and pushes `main` for Flux. A new or unregistered
  app can be rendered manually, then committed and pushed.
- Production promotion is explicit and digest-forward. `idpctl promote <app>
  prod --from dev -f deploy.yaml` reads the digest running in development and
  renders it with production policy, secrets, and namespaces. Commit the result
  on the `prod` branch. Roll back by reverting that commit.

## Setup: build idpctl

The CLI is a small Go module and does not require private repository access.

```sh
command -v go >/dev/null || mise use -g go@1.25 || mise use -g go@latest
hash -r
make build            # compiles ./idpctl ; make test runs unit and golden tests
./idpctl --help
```

`validate`, `plan`, `render`, and `catalog` run locally from source. `build` and
`doctor` also require `kubectl` because they create an in-cluster job or read
cluster state. Install it with `mise use -g kubectl`.

## Platform constraints

- Production rejects mutable tags such as `:latest`.
- Promotion uses the digest already running in the source environment and does
  not rebuild it. Select the source with `--from`.
- Each environment declares the seams it supports: stateful stores, LAN
  exposure, public routes, autoscaling, and volumes. Validation fails when an
  app requests an unsupported seam. Production does not support PVCs.
- `idp.yaml` defines the registry and repository. `idpctl` fails if it is absent.
- `expose.lan` is the only supported LoadBalancer path.

## Guardrails

- `render`, `promote`, `remove`, `infra`, `dns`, `tunnel`, and `build` mutate
  state. `dns` and `tunnel` change Cloudflare. `doctor` and `build` access the
  live cluster.
- A deployment is complete only after the environment's `platform.yaml` is
  committed on its `flux.branch`. Confirm the diff does not touch sibling
  environments before pushing.
- Treat Flux-reconciled state as authoritative. Verify it with `catalog` or
  `doctor`.
