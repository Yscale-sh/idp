# idp

> A tiny, opinionated **Internal Developer Platform** for self-hosted Kubernetes (k3s).
> A developer writes one small `deploy.yaml`; the platform renders desired state; **Flux**
> (Flux Operator + `HelmRelease`/`GitRepository`) reconciles it into the cluster. No
> hand-written Kubernetes YAML, no per-app load balancers.

**Status:** work-in-progress, running real workloads. `dev` is a live homelab k3s cluster, and the
active promotion path is **dev to prod** (digest-forward via `idpctl promote --from dev`). A **stage**
tier is scaffolded in-repo but deferred for now. `prod` is a self-provisioned k3s cluster on Linode.
The CLI is **`idpctl`** (Go module `github.com/yscale-sh/idp`). Apache-2.0.

This repo is **the platform plus reference instance files**: `environments/` holds skeleton env
definitions a fork edits, `clusters/` holds the per-env `FluxInstance` skeletons, and `idpctl
render` writes the rendered umbrella into `clusters/<env>/` in your fork. You do not install idp,
you **fork it and make it yours** (see [Make it yours](#make-it-yours)).

---

## Fork model

Fork the repo → edit `idp.yaml` (registry prefix, repo URL/branch) → edit
`environments/<env>/cluster.yaml` (zones, secrets backend, modules) and
`clusters/<env>/flux-instance.yaml` (point `spec.sync.url` at your fork) →
install the Flux Operator and apply the `FluxInstance`. From then on, deploy
apps by writing a `deploy.yaml`, running `idpctl render` to upsert into the
umbrella, and committing — Flux is the only writer.

---

## What it is

The per-app `deploy.yaml` **shopping list is the product**. A developer declares a small app
contract, and `idpctl` derives all the Kubernetes from it:

```
 deploy.yaml  ──►  idpctl render  ──►  clusters/<env>/platform.yaml   ──►  Flux  ──►  cluster
 (the dev's         (CLI: validate /     (ONE umbrella HelmRelease whose          (the only
  shopping list)     plan / render)        values list every app; charts/cluster    writer)
                                           templates an isolated HelmRelease each)
```

The contract is a **shopping list**, not Kubernetes:

```yaml
app: carshowdb
runtime: { image: ghcr.io/yscale-sh/carshowdb-api, port: 8080 }
routes:  [{ host: carshowdb, public: true }]   # bare label: carshowdb.local in dev, carshowdb.<domain> over a tunnel in prod
sizing:  { profile: minimal, replicas: 2, autoscale: { enabled: true, max: 5 } }
db:      [{ name: primary, type: postgres, size: minimal }]   # platform provisions + wires DATABASE_URL
cache:   [{ name: default, type: redis }]
```

The platform derives namespaces, the Deployment, a ClusterIP Service, secret refs, env vars
(`DATABASE_URL`, `REDIS_URL`, ...), probes, resource limits, autoscaling, and observability wiring
from that. The developer never writes a `Deployment`, a `Service`, or (by policy) a `LoadBalancer`.

## How you deploy (with Flux)

**Deploying is a git commit, never `kubectl apply` or `helm upgrade`. Flux is the only writer.**

`idpctl render` upserts the app into `clusters/<env>/platform.yaml`, the ONE umbrella `HelmRelease`
whose values list every app. You commit `platform.yaml` on the env's `flux.branch`, and Flux (the
Flux Operator plus the umbrella `HelmRelease`) reconciles it.

For day-to-day commands and failure runbooks, start with [`docs/USAGE.md`](docs/USAGE.md).

- **Operator, once:** install the **Flux Operator** and apply one `FluxInstance`
  (`clusters/<env>/flux-instance.yaml`). Its `spec.sync` points Flux at `clusters/<env>`, which holds
  the **umbrella HelmRelease** (`platform.yaml`). Flux installs the `charts/cluster` umbrella, and that
  chart templates one **isolated `HelmRelease` per app** (plus its dedicated stores) and per enabled
  module: the Helm-native replacement for the old Argo CD app-of-apps. `disableWait` isolates a
  failing app so it cannot wedge its siblings.
- **Developer, every day:** push to your branch and it deploys. The in-cluster **idp-shipper** reads
  the head SHA of your app repo+branch every interval, builds only the images whose inputs changed,
  renders each component into `clusters/dev/platform.yaml` with `idpctl`'s render core, and commits +
  pushes the platform branch for Flux to reconcile. You only ever touch your `deploy.yaml`. A new app
  is **one new entry** in the umbrella's `spec.values.apps`, and each app stays its own isolated Helm
  release. For a new or unregistered app, deploy by hand: `idpctl render --env dev --file deploy.yaml
  --image <ref>`, then commit + push `clusters/dev/platform.yaml`. `idpctl new app` scaffolds a repo.
- **Multi-component products** (api + workers + ui) are **one shopping list** with a `components:`
  list: a shared base plus per-component deltas that `Expand()`s into one isolated HelmRelease per
  component (stores provision once; the shipper builds each unique image once). See `examples/yscale-media`
  and [`docs/DEPLOY_GO_CLI.md`](docs/DEPLOY_GO_CLI.md), "Multi-component apps".

The Flux Operator ships an **embedded Web UI** (the operator Service on `:9080`), with no separate
dashboard install, for watching reconciliations.

### How dev ships automatically

The **idp-shipper** (`cmd/idp-shipper`) is infra-owned, in-cluster, and runs one instance per environment. In dev it realizes "push to
your branch, it deploys." Per registered app, every interval, it:

1. reads the GitHub head SHA for the app's repo+branch;
2. if it changed, derives the build set from the shopping lists (dedup by image);
3. builds **only** the images whose inputs (`build.context`/`dockerfile`/`submodules`) changed, via
   the in-cluster **image-builder** (rootless BuildKit to GHCR, tag `<image>:<short-sha>`); unchanged
   images reuse the tag already pinned in the umbrella;
4. renders each component into `clusters/dev/platform.yaml` with `idpctl`'s render core;
5. commits + pushes the platform branch, and Flux reconciles.

Which apps the shipper ships (repo, branch, `deploy.yaml` paths) is **infra-owned config**, a
ConfigMap declared in `environments/dev/cluster.yaml` and applied via `idpctl infra render`. The
developer's repo holds none of this; the developer only ever touches their `deploy.yaml`.

## Promoting across environments (dev to prod)

Each environment runs its own shipper: dev commits `main`, and prod runs its own instance that
commits the `prod` branch, brought online app by app as prod comes up. Prod also has a deliberate,
**digest-forward** path that never rebuilds the artifact. `idpctl promote <app> prod --from dev` reads the
image digest already running in dev's umbrella and re-renders the app with prod's policy, secrets
backend, and namespaces:

```bash
idpctl promote yscale-website prod --from dev      # pins dev's running digest into prod
```

> A **stage** tier (`idpctl promote <app> stage --from dev`, then `prod --from stage`) is
> scaffolded in-repo for combining PRs before prod, but is **deferred** for now. prod promotes
> from dev directly. See [`docs/ENVIRONMENTS.md`](docs/ENVIRONMENTS.md).

Promotion is **policy as data**. No env name is special to the platform, so a fork can run
`qa`, `eu-prod`, anything. Each target env's `cluster.yaml` declares its own gate:

```yaml
# environments/prod/cluster.yaml
promotion: { from: dev }      # promote refuses any other source (--force overrides)
allowMutableTags: false       # and refuses :latest (promotion means a pinned artifact)
flux: { branch: prod }        # the prod cluster syncs refs/heads/prod
```

prod is hard-rejected in code for mutable tags (`:latest`) regardless of `allowMutableTags`. `promote`
renders the file and prints which `flux.branch` to commit it on; commit the promote on the `prod`
branch. Rollback is one `git revert` of that commit. There is one **FluxInstance per cluster** (stage,
when enabled, rides the dev cluster's Flux as a second Kustomization; prod is its own cluster), so
prod self-heals with zero dependency on dev. Full rationale: [`docs/ENVIRONMENTS.md`](docs/ENVIRONMENTS.md).

The prod target is a self-provisioned k3s cluster on Linode built from the **jaK3s** golden image
(hardened Debian + Cilium + embedded etcd + kube-vip, etcd snapshots to R2). prod exposure is
Cloudflare Tunnel only (no MetalLB), and `statefulStores` is false, so apps get `DATABASE_URL` from
the SSM secrets backend and stay PVC-free by contract. prod Flux tracks the dedicated `prod` branch
(see `environments/prod/cluster.yaml`); promotions are commits there.

## The app contract

Beyond the basic shopping list above, `deploy.yaml` supports everything needed to run real,
multi-tier products:

| Field | What it does |
|---|---|
| `component` | One **product**, many components (e.g. Dim's `api` / `scanner` / `ui`). Each renders an isolated HelmRelease with component-aware names (`<app>-<component>`); SSM/secret roots stay app-level so siblings share. |
| `runtime.port: 0` | A **worker**: Deployment only, no Service / probes / ServiceMonitor (e.g. a scanner or queue consumer). |
| `db[] / cache[]` `provision: false` | **Share** a sibling component's store instead of provisioning a new one (one Postgres + Redis for the whole product). |
| `volumes[]` | Mount `nfs` / `emptyDir` / `pvc` volumes (read-only media libraries, shared RWX metadata, scratch caches). |
| `expose.lan` (+ `ip`) | *Advanced/optional.* A public route already gets a LAN LoadBalancer in dev; use `expose` only to pin a specific IP/pool/port or expose on the LAN with no route. The *only* sanctioned LB (policy exempts the `platform/expose: lan` label). |
| `probes.type` | `http` (default), `tcp` (port-open, for services with no health route), or `none`. |
| `sizing.extraLimits` | Arbitrary resource limits, e.g. `gpu.intel.com/i915: "1"` for hardware transcode. |
| `connectsTo[]` | Wire app-to-app / component-to-component dependencies. The platform resolves the address per env and injects it into an env var: `clusterService` (in-cluster DNS), `publicRoute` (external), or `serviceToken` (Cloudflare Access). `scheme: none` yields a bare `host:port` for nginx upstreams / DSNs. |
| `routes[]` | Where users reach the app: **one declaration, env-aware**. Mark a route `public: true` and the platform exposes it over a **Cloudflare Tunnel** in prod and a **MetalLB LAN LoadBalancer** (auto IP + hostname) in dev. A bare `host:` label composes per env (`web` to `web.local` / `web.example.com`), so the *same* `deploy.yaml` works everywhere, with no env-specific fields. |

See `examples/carshowdb` (a single-service app), `examples/dim` (a three-component media server
sharing one Postgres + Redis, an iGPU, an NFS media library, and a LAN-exposed UI), and
`examples/yscale-media`, a **fork of `dim`** that adds an *optionally-distributed sharded
transcoder*: a `replicas: 0` GPU worker pool that scales out to encode one stream across many
nodes. It is the real app idp runs; it exercises nearly the whole contract (`build` submodules, shared
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
  unifi-controller: { enabled: false } # homelab service: modules aren't just "platform" tech
```

`idpctl infra render --env <env>` sets the **enabled** modules in the umbrella; `charts/cluster`
then renders a Flux `HelmRelease` per module (plus a `HelmRepository` for each `chartRepo` module).

## The seam contract (loose coupling, enforced)

idp deploys *apps*; the surrounding infra provides the *cluster*, and they meet at a few named
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

- **An env can't claim a seam it doesn't back.** `Validate` rejects, e.g., `publicRoutes` with no
  `zones`, `autoscale` with no `keda` module, or missing observability endpoints.
- **An app can't request a seam the env doesn't provide.** A `deploy.yaml` declaring `db:` in a
  `statefulStores: false` env (or `expose.lan` in a tunnel-only env) fails policy at render/promote
  time, with a message pointing at the missing seam. So prod stays PVC-free *by contract*, not by hope.
- **`idpctl doctor`** verifies the live cluster actually backs the declared seams (Flux, ESO store,
  observability services, KEDA + its ownership, ingress/tunnel): the runtime half of the contract.

Omit `seams` entirely and they derive permissively from the rest of `cluster.yaml`, so
existing/forked envs keep working; tighten an env by declaring them.

## CLI

```bash
idpctl init     --env dev                          # scaffold environments/<env>/cluster.yaml from idp.yaml (fork's first step)
idpctl validate --file deploy.yaml                 # schema + policy checks (fail before mutation)
idpctl plan     --env dev --file deploy.yaml       # validate + policy + render to stdout (no writes)
idpctl render   --env dev --file deploy.yaml --image <ref>   # upsert the app into the umbrella
idpctl build    --repo <org/name> --ref <sha> --image <repo:tag>  # build+push via the in-cluster image-builder (no local Docker)
idpctl promote  <app> <env> --from <env> -f deploy.yaml       # digest-forward promote (dev -> prod; stage deferred)
idpctl remove   --env dev --app <name> [--component <c>]     # drop a workload/component; Flux prunes
idpctl infra render --env dev                      # set the enabled modules in the umbrella
idpctl catalog  --env dev [--format text|html|json] [--out f]  # read-only view of one env
idpctl catalog  --all --out-dir public             # whole-platform site (every env + index.html)
idpctl doctor   --env <env> [--context <ctx>]      # probe the live cluster for the seams this env declares
idpctl dns    sync|prune  --env prod               # reconcile Cloudflare DNS for public routes
idpctl tunnel up|down     --env prod               # manage the Cloudflare Tunnel
idpctl new app --name <name> [--dir <path>]        # scaffold a starter deploy.yaml (registry from idp.yaml)
```

Reads (`validate`, `plan`, `catalog`, `doctor`) are free; writes (`render`, `promote`, `remove`,
`build`, `infra`, `dns`, `tunnel`) mutate real state. The pre-built `./idpctl` binary in the repo may
be wrong-arch; build from source with `make build` if you need to run it.

### Seeing it: the catalog viewer

The platform is CLI + GitOps, but `idpctl catalog` gives you **something to look at**: a
read-only projection of `clusters/<env>/platform.yaml` (the committed desired state Flux
reconciles). It never touches a cluster; it renders what is already in git as a terminal
summary, a **self-contained HTML page**, or JSON:

```bash
idpctl catalog --env dev                                  # quick terminal glance
make catalog ENV=dev                                      # -> catalog.html (open in a browser)
idpctl catalog --env dev --format json                    # machine-readable model
make site                                                 # -> public/ : every env + an index page
```

`--all` builds the whole-platform site: one page per environment under `clusters/` plus an
`index.html` that links them with per-env counts. Because it is a pure function of git state with
no timestamp, the output is reproducible. The [`catalog` workflow](.github/workflows/catalog.yml)
publishes it to **Cloudflare Pages**; it runs on `workflow_dispatch` (manual) for now, with a `push:`
trigger to re-enable publish-on-push later. It targets the same Cloudflare the platform already uses
for Tunnel ingress and DNS. One-time setup:

```bash
# 1. create the Pages project (direct-upload)
npx wrangler pages project create idp-catalog --production-branch=main
# 2. add two repo secrets (Settings → Secrets and variables → Actions):
#    CLOUDFLARE_API_TOKEN   (Account → Cloudflare Pages → Edit)
#    CLOUDFLARE_ACCOUNT_ID
# 3. (optional) deploy from your machine instead of CI:
make deploy-catalog        # needs CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID in your env
```

The default site is `https://idp-catalog.pages.dev`; add a custom domain (e.g.
`catalog.<your-zone>`) in the Pages project. The rule it keeps: the UI is a **view, never a
writer**. All writes stay in git, and the site exposes nothing that is not already in the repo.

## Layout

| Path | What |
|---|---|
| `cmd/idpctl`, `internal/*` | the Go CLI (`appconfig`, `render`, `policy`, `modules`, `clouddns`, `clusterenv`, `helmrunner`, `kube`, `secrets`, `scaffold`, `deploy`, `catalog`) |
| `cmd/idp-shipper` | the in-cluster CD orchestrator, one instance per env (poll repos -> build -> render -> commit -> Flux) |
| `charts/app` | the shared app chart (Deployment + ClusterIP Service + optional KEDA / ServiceMonitor / ExternalSecret / PDB / LAN LoadBalancer / volumes) |
| `charts/infra`, `modules/` | infra module charts (`dev-postgres`, `dev-redis`) + the module registry |
| `charts/cluster` | the **umbrella** chart: templates a HelmRelease per app / store / module from the env values |
| `environments/{dev,stage,prod}` | per-env `cluster.yaml` (module matrix, policy, promotion gate, flux source): renderer **input**, not deployed |
| `clusters/{dev,stage,prod}` | the per-env `FluxInstance` + `platform.yaml` (rendered umbrella); `clusters/dev/sync-stage.yaml` bootstraps stage on the dev cluster |
| `examples/{carshowdb,dim}` | onboarded apps: a single service and a multi-component product |
| `schemas/`, `test/` | the `deploy.yaml` JSON schema + unit/golden/e2e tests |
| `docs/` | design notes & references: see [`ENVIRONMENTS.md`](docs/ENVIRONMENTS.md) (envs + promotion), plus env tiers, secrets model, CLI design, backups, history |

## Quickstart (dev)

No cluster, cloud account, or credentials needed: render is local.

```bash
make build && make test                            # compiles ./idpctl, runs unit/golden tests
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl render   --env dev --file examples/carshowdb/deploy.yaml --image ghcr.io/yscale-sh/carshowdb-api:dev-<sha>
./idpctl infra render --env dev
make e2e          # spins an ephemeral k3d cluster, applies, asserts rollout, tears down (SKIPs if k3d absent)
```

## Make it yours

The platform has exactly **one identity file**: [`idp.yaml`](idp.yaml). Nothing identity-shaped is
defaulted in Go code or charts; `idpctl` fails closed if it is missing. To run your own instance:

1. **Fork** this repo, then edit `idp.yaml`: your image registry prefix and your fork's URL/branch.
2. **Generate your own env** instead of inheriting this instance's: `idpctl init --env dev`
   writes a fresh `environments/dev/cluster.yaml` wired to *your* `idp.yaml` (no homelab IPs,
   module matrix, or registry orgs carried over). Repeat for `stage`/`prod`; then edit each to
   toggle the modules you run and point the secret store at yours.
3. Re-render the state as your own: `./idpctl infra render --env dev`, then onboard your first app
   with `idpctl new app --name <name>` (or start from `examples/`).
4. Bootstrap a cluster: install the Flux Operator and apply `clusters/<env>/flux-instance.yaml`.
   From then on, deploying is a git commit.

`examples/` are the author's real app contracts kept as reference material; `clusters/` ships only
the `FluxInstance` skeletons you point at your own fork. `idpctl remove` plus re-render replaces
the umbrella with yours.

Only the optional cloud features need accounts: `idpctl dns` / `idpctl tunnel` (Cloudflare) and the
`ssm` secrets backend (AWS). The dev environment runs without any of them.

> **Known caveat:** `internal/clusterenv` still hardcodes `if c.Env == "prod"` for some strictness,
> so `prod` is currently a reserved name even though environments are otherwise data. A fork can use
> any other env name freely.

## Background / design

Design notes live in [`docs/`](docs/): the CLI design ([`DEPLOY_GO_CLI.md`](docs/DEPLOY_GO_CLI.md)),
the secrets trust model ([`SECRETS.md`](docs/SECRETS.md)), env-var tiers ([`ENV.md`](docs/ENV.md)),
and design history ([`IDP.md`](docs/IDP.md)). Postgres backups are CNPG barman→R2 (chart
`charts/infra/cnpg-db`). The platform contract is [`CONVENTIONS.md`](CONVENTIONS.md); the target
production shape is [`ARCHITECTURE.md`](ARCHITECTURE.md).

The project grew out of a cloud cost review: seven $10/mo load balancers, a $32/mo managed
Postgres, and hand-rolled nginx made the wrong path the easy one. The driving idea: make the
cheap, correct path the *default*. One ingress path, ClusterIP-only apps, self-hosted data
stores, and elastic cloud burst only when load demands it.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md). TL;DR: `make test` and `make lint` must pass;
`make e2e` if you touched rendering or charts.

## License

Apache-2.0 (see `LICENSE`).
