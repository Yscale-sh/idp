---
name: idp
description: Ship and manage apps on this self-hosted k3s platform with idpctl. Use when working in this repo to author or validate a deploy.yaml app manifest, deploy or promote an app (dev or prod), inspect the catalog or live-cluster seams, or understand the lifecycle (auto-reconcile via the idp-shipper, digest-forward promotion, Flux). Read this before running idpctl.
---

# idp: ship to this self-hosted k3s platform

You are working inside the **idp platform repo**. `idpctl` turns a small app contract
(`deploy.yaml`, the "app manifest") into reconciled Kubernetes state. You never write a
Deployment, Service, or LoadBalancer. The platform derives namespaces, secret refs, env vars
(`DATABASE_URL`, `REDIS_URL`, and the rest), probes, resource limits, autoscaling, and
observability from the contract.

**Deploying is a git commit, never `kubectl apply` or `helm upgrade`.** `idpctl render` upserts the
app into `clusters/<env>/platform.yaml` (the one umbrella HelmRelease whose values list every app).
You commit that file on the env's `flux.branch`, and **Flux** reconciles it into the cluster. Flux
is the only writer. `charts/cluster` templates one isolated HelmRelease per app, its stores, and
each enabled module; `disableWait` isolates a failing app so it cannot wedge its siblings.

The per-app `deploy.yaml` app manifest is the product. Everything else is derived from it.

Authoritative detail lives in the repo. Read on demand, do not preload:

- `docs/USAGE.md` (start here: operator runbooks plus the command cheat-sheet)
- `README.md` (overview and lifecycle), `CONVENTIONS.md` (the platform contract)
- `docs/DEPLOY_GO_CLI.md` (CLI design, package layout, the full app contract, guardrails)
- `docs/ENVIRONMENTS.md` (environment topology and promotion), `docs/SECRETS.md`, `docs/ENV.md`
- `schemas/deploy.schema.json` (the contract schema; the Go in `internal/appconfig` is authoritative)
- `examples/` (worked apps: `carshowdb` single service, `hello-dev` Cloudflare Access dev exposure, `yscale-media` multi-component)

## Chain of thought (read first)

1. **Preflight: is `idpctl` built?** Run `command -v idpctl || ls ./idpctl`. If it is missing, build
   it (see Setup). The checked-in `./idpctl` binary may be the wrong architecture, so prefer a fresh
   `make build`. Confirm the target environment (`--env`, default `dev`).
2. **Classify the intent and do the smallest safe thing first.** Reads are free; use them to think
   before you mutate.

   | Intent | Command | Writes? |
   |---|---|---|
   | Check a contract is valid | `idpctl validate --file deploy.yaml` | no |
   | See what a deploy would change | `idpctl plan --env <env> --file deploy.yaml --image <ref>` | no |
   | Look at what is deployed | `idpctl catalog --env <env> [--format json]` | no |
   | Health-check the live cluster's seams | `idpctl doctor --env <env>` | no (reads cluster) |
   | Scaffold a new app | `idpctl new app --name <name> [--dir <path>]` | writes a starter file |
   | Scaffold an env for a fork | `idpctl init --env <env>` | writes `environments/<env>/cluster.yaml` |
   | **Deploy** an app to an env | `idpctl render --env <env> -f deploy.yaml --image <ref>` then commit | mutates `platform.yaml` |
   | **Promote** dev to prod (no rebuild) | `idpctl promote <app> prod --from dev -f deploy.yaml` then commit | mutates `platform.yaml` |
   | Drop an app or component | `idpctl remove --env <env> --app <name> [--component <c>]` | mutates `platform.yaml` |
   | Set enabled infra modules | `idpctl infra render --env <env>` | mutates `platform.yaml` |
   | Build and push an image (no local Docker) | `idpctl build --repo <org/name> --ref <sha> --image <repo:tag>` | in-cluster job |
   | Cloudflare DNS or tunnel | `idpctl dns sync\|prune` and `idpctl tunnel up\|down --env <env>` | touches Cloudflare |

3. **Author or validate, then plan, then render, then commit, then verify.** Always `validate` then
   `plan` before a `render`. After a write, commit the changed `clusters/<env>/platform.yaml` on the
   env's `flux.branch`; Flux does the rest. Confirm with `catalog` or `doctor`.

## The deploy.yaml contract (the app manifest)

```yaml
app: carshowdb
runtime: { image: ghcr.io/yscale-sh/carshowdb-api, port: 8080 }   # port 0 means a worker (no Service or probes)
routes:  [{ host: carshowdb, public: true }]   # public route: Cloudflare Tunnel in prod, MetalLB LAN LoadBalancer in dev
sizing:  { profile: minimal, replicas: 2, autoscale: { enabled: true, max: 5 } }
db:      [{ name: primary, type: postgres, size: minimal }]   # platform provisions and wires DATABASE_URL
cache:   [{ name: default, type: redis }]                     # wires REDIS_URL
```

Required keys are `app` and `runtime`. A multi-component product (api plus workers plus ui) is
**one** file with a `components:` list: a shared base plus per-component deltas that expand into one
isolated HelmRelease per component. `db`/`cache` with `provision: false` shares a sibling
component's store. `connectsTo[]` wires app-to-app dependencies and injects the resolved address per
env (`clusterService`, `publicRoute`, or `serviceToken`; `scheme: none` yields a bare `host:port`).
`volumes[]` mounts `nfs`, `emptyDir`, or `pvc`. `probes.type` is `http` (default), `tcp`, or `none`.
The authoritative field list is `docs/DEPLOY_GO_CLI.md` and `schemas/deploy.schema.json`.

## Lifecycle

- **dev (continuous and automatic).** The in-cluster `idp-shipper` (`cmd/idp-shipper`) is
  infra-owned and enabled per environment. Dev's instance (`registry.env: dev`) polls each
  registered app's branch, builds only the images whose inputs changed via the in-cluster
  `image-builder` (rootless BuildKit to GHCR), renders each component into
  `clusters/dev/platform.yaml`, and commits and pushes `main` for Flux to reconcile. The developer
  only ever touches their `deploy.yaml`. A new or unregistered app deploys manually with
  `idpctl render --env dev --root <idp clone> -f deploy.yaml --image <ref>`, then a commit and push
  of `clusters/dev/platform.yaml`.
- **prod (deliberate plus its own shipper).** Prod promotes from dev directly; stage is scaffolded
  but deferred (`environments/prod/cluster.yaml` declares `promotion.from: dev`).
  `idpctl promote <app> prod --from dev -f deploy.yaml` reads the digest already running in dev and
  re-renders it into `clusters/prod/platform.yaml` with prod's policy, secrets backend, and
  namespaces. It never rebuilds the artifact. Prod also runs its own shipper
  (`registry.env: prod`, `platformBranch: prod`) that commits the `prod` branch, brought online app
  by app. Commit the promote on the `prod` branch (the command prints it). Rollback is a `git
  revert` of that one commit.

## Setup: build idpctl

It is a small pure-Go module, no private-repo auth needed.

```sh
command -v go >/dev/null || mise use -g go@1.25 || mise use -g go@latest
hash -r
make build            # compiles ./idpctl ; make test runs unit and golden tests
./idpctl --help
```

`validate`, `plan`, `render`, and `catalog` are pure-local and work from source alone. `build` and
`doctor` additionally need `kubectl` (they drive an in-cluster Job or read the cluster); provision
it with `mise use -g kubectl`.

## Fail-closed rails (the platform enforces these, work with them)

- **Prod refuses mutable tags** (`:latest`). Prod means a pinned artifact.
- **Promotion is digest-forward.** `promote` reads the image already running in the source env and
  re-renders it with the target env's policy and secrets. It never rebuilds. Pin the source with
  `--from`.
- **The seam contract.** Each env declares which seams it backs (stateful stores, LAN exposure,
  public routes, autoscale, volumes). An app that requests a seam the env does not provide fails at
  `validate` or `render` with a message naming the missing seam. Prod stays PVC-free by contract.
- **One identity file.** `idp.yaml` holds the registry and repo. `idpctl` fails closed if it is
  missing.
- **The only sanctioned LoadBalancer** is `expose.lan` (a labeled exemption). Do not add others.

## Guardrails

- Reads (`validate`, `plan`, `catalog`, `doctor`) are free. Writes (`render`, `promote`, `remove`,
  `infra`, `dns`, `tunnel`, `build`) mutate real state; `dns` and `tunnel` touch Cloudflare and
  `doctor` and `build` touch the live cluster. Be deliberate, especially with prod.
- A deploy is not done until the `clusters/<env>/platform.yaml` change is committed on the env's
  `flux.branch`. Before pushing, confirm the diff touches only that env's file; sibling envs live in
  separate files and must stay untouched.
- The running cluster state is what Flux has reconciled from git, not whatever you built locally.
  `catalog` and `doctor` show the truth.
