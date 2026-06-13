# idp

> A tiny, opinionated **Internal Developer Platform** for self-hosted Kubernetes (k3s).
> A developer writes one small `deploy.yaml`; the platform renders desired state; **Flux**
> (Flux Operator + `HelmRelease`/`GitRepository`) reconciles it into the cluster. No
> hand-written Kubernetes YAML, no per-app load balancers.

**Status:** work-in-progress, running real workloads. `dev` is a live homelab k3s cluster; the
full **dev → stage → prod** promotion pipeline is implemented and proven end-to-end against a
local prod stand-in (`prod` itself targets self-provisioned k3s on Linode — migration gated, see
below). The CLI is **`idpctl`** (Go module `github.com/jakenesler/idp`). Apache-2.0.

This repo is **both the platform and a live instance of it**: `clusters/` and `environments/`
hold the author's real rendered state. That's the model — you don't install idp, you
**fork it and make it yours** (see [Make it yours](#make-it-yours)).

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
  from its own published image; the in-cluster **idp-shipper** automates build → render → commit on push,
  and `idpctl new app` scaffolds a repo.

The Flux Operator ships an **embedded Web UI** (the operator Service on `:9080`) — no separate
dashboard install — for watching reconciliations.

## Promoting across environments (dev → stage → prod)

The shipper auto-deploys **dev** on every push. Moving to **stage** and **prod** is
deliberate and **digest-forward** — the artifact never rebuilds; `idpctl promote` reads
the image already running in the source env's umbrella and re-renders it with the target
env's policy, secrets backend, and namespaces:

```bash
idpctl promote yscale-website stage --from dev      # pins dev's running digest into stage
idpctl promote yscale-website prod  --from stage    # pins stage's digest into prod
```

Promotion is **policy as data** — no env name is special to the platform, so a fork can run
`qa`, `eu-prod`, anything. Each target env's `cluster.yaml` declares its own gate:

```yaml
# environments/prod/cluster.yaml
promotion: { from: stage }    # promote refuses any other source (--force overrides)
allowMutableTags: false       # and refuses :latest — promotion means a pinned artifact
flux: { branch: prod }        # the branch the prod cluster's Flux tracks
```

`promote` renders the file and prints which `flux.branch` to commit it on; the git push stays
CI-owned. Rollback is one `git revert` of that commit. One **FluxInstance per cluster** (stage
rides the dev cluster's Flux as a second Kustomization; prod is its own cluster on the `prod`
branch), so prod self-heals with zero dependency on dev. Full rationale + the proven walkthrough:
[`docs/ENVIRONMENTS.md`](docs/ENVIRONMENTS.md).

The prod target is a self-provisioned k3s cluster on Linode built from the **jaK3s** golden image
(hardened Debian + Cilium + embedded-etcd, etcd snapshots → R2). Migration is gated, LKE untouched
until cutover: [`docs/PROD_MIGRATION.md`](docs/PROD_MIGRATION.md).

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

See `examples/carshowdb` (a single-service app), `examples/dim` (a three-component media server
sharing one Postgres + Redis, an iGPU, an NFS media library, and a LAN-exposed UI), and
`examples/yscale-media` — a **fork of `dim`** that adds an *optionally-distributed sharded
transcoder*: a `replicas: 0` GPU worker pool that scales out to encode one stream across many
nodes. The real app idp runs; it exercises nearly the whole contract (`build` submodules, shared
stores, GPU `extraLimits`, four volume kinds, `connectsTo`, `expose.lan.host`).

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

## The seam contract (loose coupling, enforced)

idp couples to a cluster only through a handful of **seams** — interfaces a host
cluster provides: in-cluster data stores, LAN exposure, public routes, autoscaling,
volumes, secrets, observability. Each environment **declares** which seams it
provides, and that declaration is enforced from three sides:

```yaml
# environments/prod/cluster.yaml — prod is PVC-free + tunnel-only
seams:
  statefulStores: false   # no in-cluster db/cache — apps get DATABASE_URL from SSM
  lanExpose:      false   # Cloudflare Tunnels only, no MetalLB
  # publicRoutes / autoscale derive from zones / the keda module; volumes default true
```

- **`cluster.yaml` validates its own claims** — an env can't declare a seam it
  doesn't back (`publicRoutes` needs zones, `autoscale` needs the keda module,
  observability endpoints are required).
- **apps can't request undeclared seams** — a `deploy.yaml` with `db:`/`cache:`
  in a `statefulStores: false` env, or `expose.lan` where there's no MetalLB, is
  rejected at render time (fail-closed, not silently degraded).
- **`idpctl doctor`** probes the live cluster that every declared seam is actually
  present and healthy (Flux, ESO store, KEDA, ingress/tunnel, observability) — a
  CI pre-promote / pre-bootstrap gate.

Omit `seams` and they derive permissively from the rest of `cluster.yaml`, so
existing/forked envs keep working; tighten an env by declaring them.

## The seam contract (loose coupling, enforced)

idp deploys *apps*; the surrounding infra provides the *cluster* — and they meet at a few named
**seams** (interfaces): in-cluster data stores, LAN exposure, public routes, autoscaling, volumes.
Each environment **declares which seams it provides** as data, and the platform makes that
declaration a fail-closed contract:

```yaml
# environments/prod/cluster.yaml
seams:
  statefulStores: false   # PVC-free: apps get DATABASE_URL from the secrets backend, not an in-cluster PG
  lanExpose: false        # Cloudflare-Tunnel-only; no MetalLB
  # publicRoutes/autoscale derive from `zones` + the keda module; omit `seams` entirely to derive all
```

- **An env can't claim a seam it doesn't back** — `Validate` rejects e.g. `publicRoutes` with no
  `zones`, `autoscale` with no `keda` module, or missing observability endpoints.
- **An app can't request a seam the env doesn't provide** — a `deploy.yaml` declaring `db:` in a
  `statefulStores: false` env (or `expose.lan` in a tunnel-only env) fails policy at render/promote
  time, with a message pointing at the missing seam. So prod stays PVC-free *by contract*, not by hope.
- **`idpctl doctor`** verifies the live cluster actually backs the declared seams (Flux, ESO store,
  observability services, KEDA + its ownership, ingress/tunnel) — the runtime half of the contract.

## CLI

```bash
idpctl validate --file deploy.yaml                 # schema + policy checks (fail before mutation)
idpctl plan     --env dev --file deploy.yaml       # show what would change
idpctl render   --env dev --file deploy.yaml --image <ref>   # upsert the app into the umbrella
idpctl promote  <app> <env> --from <env> -f deploy.yaml       # digest-forward promote (dev→stage→prod)
idpctl remove   --env dev --app <name> [--component <c>]     # drop an app/component from the umbrella
idpctl infra render --env dev                      # set the enabled modules in the umbrella
idpctl catalog  --env dev [--format text|html|json] [--out f]  # read-only view of one env
idpctl catalog  --all --out-dir public             # whole-platform site (every env + index.html)
idpctl doctor   --env <env> [--context <ctx>]      # probe the live cluster for the seams this env declares
idpctl dns    sync|prune  --env prod               # reconcile Cloudflare DNS for public routes
idpctl tunnel up|down     --env prod               # manage the Cloudflare Tunnel
idpctl new app <name>                              # scaffold an app repo (deploy.yaml; registry from idp.yaml)
```

### Seeing it: the catalog viewer

The platform is CLI + GitOps, but `idpctl catalog` gives you **something to look at** — a
read-only projection of `clusters/<env>/platform.yaml` (the committed desired state Flux
reconciles). It never touches a cluster; it just renders what's already in git as a terminal
summary, a **self-contained HTML page**, or JSON:

```bash
idpctl catalog --env dev                                  # quick terminal glance
make catalog ENV=dev                                      # -> catalog.html (open in a browser)
idpctl catalog --env dev --format json                    # machine-readable model
make site                                                 # -> public/ : every env + an index page
```

`--all` builds the whole-platform site: one page per environment under `clusters/` plus an
`index.html` that links them with per-env counts. Because it's a pure function of git state with
no timestamp, the output is reproducible — the [`catalog` workflow](.github/workflows/catalog.yml)
publishes it to **GitHub Pages** on every push to `main` and the diff stays clean (enable it once
under *Settings → Pages → Source: GitHub Actions*). The rule it keeps: the UI is a **view, never a
writer** — all writes stay in git, and the site exposes nothing that isn't already in the repo.

## Layout

| Path | What |
|---|---|
| `cmd/idpctl`, `internal/*` | the Go CLI (`appconfig`, `render`, `policy`, `modules`, `clouddns`, `clusterenv`, `helmrunner`, `kube`, `secrets`, `scaffold`, `deploy`, `catalog`) |
| `charts/app` | the shared app chart (Deployment + ClusterIP Service + optional KEDA / ServiceMonitor / ExternalSecret / PDB / LAN LoadBalancer / volumes) |
| `charts/infra`, `modules/` | infra module charts (`dev-postgres`, `dev-redis`) + the module registry |
| `charts/cluster` | the **umbrella** chart — templates a HelmRelease per app / store / module from the env values |
| `environments/{dev,stage,prod}` | per-env `cluster.yaml` (module matrix, policy, promotion gate, flux source) — renderer **input**, not deployed |
| `clusters/{dev,stage,prod}` | the per-env `FluxInstance` + `platform-<env>.yaml` (rendered umbrella); `clusters/dev/sync-stage.yaml` bootstraps stage on the dev cluster |
| `examples/{carshowdb,dim}` | onboarded apps — a single service and a multi-component product |
| `schemas/`, `test/` | the `deploy.yaml` JSON schema + unit/golden/e2e tests |
| `docs/` | design notes & references — see [`ENVIRONMENTS.md`](docs/ENVIRONMENTS.md) (envs + promotion), [`PROD_MIGRATION.md`](docs/PROD_MIGRATION.md) (gated LKE→jaK3s plan), plus env tiers, secrets model, CLI design, backups, history |

## Quickstart (dev)

No cluster, cloud account, or credentials needed — render is local:

```bash
make build && make test                            # compiles ./idpctl, runs unit/golden tests
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl render   --env dev --file examples/carshowdb/deploy.yaml --image ghcr.io/jakenesler/carshowdb-api:dev-<sha>
./idpctl infra render --env dev
make e2e          # spins an ephemeral k3d cluster, applies, asserts rollout, tears down (SKIPs if k3d absent)
```

## Make it yours

The platform has exactly **one identity file**: [`idp.yaml`](idp.yaml). Nothing identity-shaped is
defaulted in Go code or charts — `idpctl` fails closed if it's missing. To run your own instance:

1. **Fork** this repo, then edit `idp.yaml`: your image registry prefix and your fork's URL/branch.
2. Edit `environments/<env>/cluster.yaml`: point `flux.repoURL`/`branch` at your fork and toggle
   the modules you want.
3. Re-render the state as your own: `./idpctl infra render --env dev`, then onboard your first app
   with `idpctl new app <name>` (or start from `examples/`).
4. Bootstrap a cluster: install the Flux Operator and apply `clusters/<env>/flux-instance.yaml`.
   From then on, deploying is a git commit.

The `clusters/` and `examples/` content you inherit from the fork is just the author's instance —
`idpctl remove` / re-render replaces it with yours.

Only the optional cloud features need accounts: `idpctl dns` / `idpctl tunnel` (Cloudflare) and the
`ssm` secrets backend (AWS). The dev environment runs without any of them.

## Background / design

Design notes live in [`docs/`](docs/): the CLI design ([`DEPLOY_GO_CLI.md`](docs/DEPLOY_GO_CLI.md)),
the secrets trust model ([`SECRETS.md`](docs/SECRETS.md)), env-var tiers ([`ENV.md`](docs/ENV.md)),
Postgres backups ([`POSTGRES_BACKUPS.md`](docs/POSTGRES_BACKUPS.md)), and design history
([`IDP.md`](docs/IDP.md)). The platform contract is [`CONVENTIONS.md`](CONVENTIONS.md); the target
production shape is [`ARCHITECTURE.md`](ARCHITECTURE.md).

The project grew out of a cloud cost review: seven $10/mo load balancers, a $32/mo managed
Postgres, and hand-rolled nginx made the wrong path the easy one. The driving idea: make the
cheap, correct path the *default* — one ingress path, ClusterIP-only apps, self-hosted data
stores, elastic cloud burst only when load demands it.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). TL;DR: `make test` and `make lint` must pass;
`make e2e` if you touched rendering or charts.

## License

Apache-2.0 (see `LICENSE`).
