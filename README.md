# idp — Jake's Developer Platform

> A tiny, opinionated **Internal Developer Platform** for self-hosted Kubernetes (k3s).
> A developer writes one small `deploy.yaml`; the platform renders desired state; **Flux**
> (Flux Operator + `HelmRelease`/`GitRepository`) reconciles it into the cluster. No
> hand-written Kubernetes YAML, no per-app load balancers.

**Status:** early / work-in-progress. Built and validated against a homelab k3s cluster (`dev`)
before it targets cloud k3s (`prod`). Repo is being kept private until it's ready to open-source.
The CLI is **`idpctl`** (Go module `github.com/jakenesler/idp`).

---

## What it is

`idpctl` turns a small app contract into reconciled Kubernetes state:

```
 deploy.yaml  ──►  idpctl render  ──►  clusters/<env>/platform.yaml   ──►  Flux  ──►  cluster
 (the dev's         (CLI: validate /     (ONE umbrella HelmRelease whose          (the only
  shopping list)     plan / render)        values list every app; charts/cluster    writer)
                                           templates an isolated HelmRelease each)
```

The contract is a **shopping list**, not Kubernetes:

```yaml
app: carshowdb
runtime: { image: ghcr.io/jakenesler/carshowdb-api, port: 8080 }
routes:  [{ host: carshowdb.example.com, public: true }]
sizing:  { profile: minimal, replicas: 2, autoscale: { enabled: true, max: 5 } }
db:      [{ name: primary, type: postgres, size: minimal }]   # platform provisions + wires DATABASE_URL
cache:   [{ name: default, type: redis }]
```

The platform derives namespaces, secret refs, env vars (`DATABASE_URL`, `REDIS_URL`, …), probes,
resource limits, autoscaling and observability wiring from that. The developer never writes a
`Deployment`, `Service`, or — by policy — a `LoadBalancer`.

## How you deploy (with Flux)

**Deploying is a git commit, never `kubectl apply`/`helm upgrade`. Flux is the only writer.**

- **Operator, once:** install the **Flux Operator** and apply one `FluxInstance`
  (`clusters/<env>/flux-instance.yaml`). Its `spec.sync` points Flux at `clusters/<env>`, which holds
  the **umbrella HelmRelease** (`platform.yaml`). Flux installs the `charts/cluster` umbrella, and that
  chart templates one **isolated `HelmRelease` per app** (+ its dedicated stores) and per enabled
  module — the Helm-native replacement for the old Argo CD app-of-apps. `disableWait` isolates a
  failing app so it can't wedge its siblings.
- **Developer, every day:** push your app repo → CI builds & pushes the image → runs
  `idpctl render --env <env> --file deploy.yaml --image <tag>` → which **upserts the app** into
  `clusters/<env>/platform.yaml` → commit it → Flux reconciles. A new app is just **one new entry** in
  the umbrella's `spec.values.apps` (each app stays its own isolated Helm release). CI runs `idpctl`
  from its own published image; `ship.yml` is a reusable workflow and `idpctl new app` scaffolds a repo.

The Flux Operator ships an **embedded Web UI** (the operator Service on `:9080`) — no separate
dashboard install — for watching reconciliations.

## The app contract

Beyond the basic shopping list above, `deploy.yaml` supports everything needed to run real,
multi-tier products:

| Field | What it does |
|---|---|
| `component` | One **product**, many components (e.g. Dim's `api` / `scanner` / `ui`). Each renders an isolated HelmRelease with component-aware names (`<app>-<component>`); SSM/secret roots stay app-level so siblings share. |
| `runtime.port: 0` | A **worker** — Deployment only, no Service / probes / ServiceMonitor (e.g. a scanner or queue consumer). |
| `db[] / cache[]` `provision: false` | **Share** a sibling component's store instead of provisioning a new one (one Postgres + Redis for the whole product). |
| `volumes[]` | Mount `nfs` / `emptyDir` / `pvc` volumes (read-only media libraries, shared RWX metadata, scratch caches). |
| `expose.lan` (+ `ip`) | Publish on the LAN via a **MetalLB** LoadBalancer — the *only* sanctioned LB (policy exempts the `platform/expose: lan` label); local-backend only. |
| `probes.type` | `http` (default), `tcp` (port-open, for services with no health route), or `none`. |
| `sizing.extraLimits` | Arbitrary resource limits, e.g. `gpu.intel.com/i915: "1"` for hardware transcode. |
| `connectsTo[]` | Wire app→app / component→component dependencies. The platform resolves the address per env and injects it into an env var — `clusterService` (in-cluster DNS), `publicRoute` (external), or `serviceToken` (Cloudflare Access); `scheme: none` yields a bare `host:port` for nginx upstreams / DSNs. |
| `routes[]` | Public hostnames. In prod these are served over a **Cloudflare Tunnel** with DNS managed by `idpctl dns` / `idpctl tunnel`; dev stays LAN-only. |

See `examples/carshowdb` (a single-service app) and `examples/dim` (a three-component media server
sharing one Postgres + Redis, an iGPU, an NFS media library, and a LAN-exposed UI).

## The module system

Every piece of cluster technology is a **toggleable module** (a Helm release). Enable or skip any of
them per environment in `environments/<env>/cluster.yaml`:

```yaml
modules:
  keda:         { enabled: true }      # workload autoscaling
  dev-postgres: { enabled: true }      # self-hosted Postgres for dev
  dev-redis:    { enabled: true }      # self-hosted Redis for dev
  external-secrets: { enabled: false }
  monitoring:   { enabled: false }     # wraps kube-prometheus-stack
  yscale:       { enabled: false }     # bursts ephemeral cloud nodes (prod, later)
  unifi-controller: { enabled: false } # homelab service — modules aren't just "platform" tech
```

`idpctl infra render --env <env>` sets the **enabled** modules in the umbrella; `charts/cluster`
then renders a Flux `HelmRelease` per module (plus a `HelmRepository` for each `chartRepo` module).

## CLI

```bash
idpctl validate --file deploy.yaml                 # schema + policy checks (fail before mutation)
idpctl plan     --env dev --file deploy.yaml       # show what would change
idpctl render   --env dev --file deploy.yaml --image <ref>   # upsert the app into the umbrella
idpctl remove   --env dev --app <name> [--component <c>]     # drop an app/component from the umbrella
idpctl infra render --env dev                      # set the enabled modules in the umbrella
idpctl dns    sync|prune  --env prod               # reconcile Cloudflare DNS for public routes
idpctl tunnel up|down     --env prod               # manage the Cloudflare Tunnel
idpctl new app <name>                              # scaffold an app repo (deploy.yaml + ship.yml)
```

## Layout

| Path | What |
|---|---|
| `cmd/idpctl`, `internal/*` | the Go CLI (`appconfig`, `render`, `policy`, `modules`, `clouddns`, `clusterenv`, `helmrunner`, `kube`, `secrets`, `scaffold`, `deploy`) |
| `charts/app` | the shared app chart (Deployment + ClusterIP Service + optional KEDA / ServiceMonitor / ExternalSecret / PDB / LAN LoadBalancer / volumes) |
| `charts/infra`, `modules/` | infra module charts (`dev-postgres`, `dev-redis`) + the module registry |
| `charts/cluster` | the **umbrella** chart — templates a HelmRelease per app / store / module from the env values |
| `environments/{dev,prod}` | per-env `cluster.yaml` (module matrix + flux source) — renderer **input**, not deployed |
| `clusters/{dev,prod}` | the per-env `FluxInstance` (the one bootstrap) + `platform.yaml` (the rendered umbrella HelmRelease) |
| `examples/{carshowdb,dim}` | onboarded apps — a single service and a multi-component product |
| `schemas/`, `test/` | the `deploy.yaml` JSON schema + unit/golden/e2e tests |

## Quickstart (dev)

```bash
make build && make test                            # compiles ./idpctl, runs unit/golden tests
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl render   --env dev --file examples/carshowdb/deploy.yaml --image ghcr.io/jakenesler/carshowdb-api:dev-<sha>
./idpctl infra render --env dev
make e2e          # spins an ephemeral k3d cluster, applies, asserts rollout, tears down (SKIPs if k3d absent)
```

## Background / design

This grew out of a Linode cost review (see [`COST_REVIEW.md`](COST_REVIEW.md)) and the design notes
in [`DEPLOY_GO_CLI.md`](DEPLOY_GO_CLI.md), [`ARCHITECTURE.md`](ARCHITECTURE.md), [`IDP.md`](IDP.md),
and [`ENV.md`](ENV.md). The driving idea: make the cheap, correct path the *default* — one ingress
path, ClusterIP-only apps, self-hosted data stores, elastic cloud burst only when load demands it.

> Operational docs in this repo still contain environment-specific details (LAN IPs, cluster IDs);
> these get scrubbed before any public release.

## License

Apache-2.0 (see `LICENSE`).
