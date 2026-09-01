# `idpctl` design rationale

Status: implemented design note. This document explains the decisions behind `idpctl`. The current
command surface, package layout, and app contract map to `cmd/idpctl`,
`internal/`, and `schemas/deploy.schema.json`.

## Design boundary

Deployment uses a versioned Go CLI instead of a collection of shell scripts. An application's
`deploy.yaml` declares runtime, routes, sizing, stores, storage, and connections. The platform
derives namespaces, a Deployment, a ClusterIP Service, secret references, environment variables,
probes, resource limits, autoscaling, and observability. Application repositories do not contain
Deployments, Services, or LoadBalancers.

CI builds an image; the CLI validates the contract, renders the app into the env umbrella, and (for
public routes) wires Cloudflare Tunnel routing and DNS plus any requested secret refs. Deploying is a
git commit. Flux is the only writer.

The included reference environments model the same contract in two shapes:

- `prod`: an existing production Kubernetes cluster.
- `dev`: a local/on-prem k3s cluster for development and testing.

The ownership boundary is:

- the app repo owns code, Dockerfile, and `deploy.yaml`
- the platform module is versioned and rendered by `idpctl`
- every app deploys as `ClusterIP` only; public exposure goes through Cloudflare Tunnel (prod) or a
  labeled MetalLB LAN LoadBalancer (dev)

`idpctl` replaced the former `bin/deploy.sh` and `ensure-*.sh` scripts.

## Why Go

Shell is suitable for small wrappers, but this platform needs structured state handling:

- YAML parsing and schema/default validation
- Kubernetes object reads/writes with idempotency
- Helm release boundaries rendered as Flux `HelmRelease`s
- Cloudflare DNS, Tunnel routes, and Access service-token provisioning
- AWS SSM/external-secrets integration
- structured logs and clear failure modes in CI
- a maintainable scaffolder

Go fits the rest of the stack, builds to a single static-ish binary, and has stable libraries for
Kubernetes, Helm, Cloudflare, and AWS.

## Repository shape

The sketch below shows the main package boundaries; `internal/` names map to packages such as
`appconfig`, `render`, `clusterenv`, `policy`, `secrets`, `scaffold`,
`builder`, `clouddns`, `catalog`, `modules`, `tenant`, ...).

```text
idp/
  cmd/idpctl/
    main.go
  internal/
    appconfig/       # deploy.yaml types, defaults, validation
    render/          # high-level render orchestration (the render core)
    clusterenv/      # per-env policy: cluster.yaml types, seams, validation
    helmrunner/      # Helm SDK or command wrapper
    kube/            # Kubernetes clients, namespace/RBAC helpers
    clouddns/        # Cloudflare DNS + Tunnel config
    secrets/         # SSM + ExternalSecret generation
    scaffold/        # new app/repo/config generation
    policy/          # guardrails: no LoadBalancer, resource limits, naming, seams
  charts/
    app/
    infra/
    cluster/         # the umbrella chart: fans out to one inner HelmRelease per app/store/module
  schemas/
    deploy.schema.json
  environments/      # renderer INPUT, not deployed
    prod/
      cluster.yaml   # module matrix + flux source for prod
    dev/
      cluster.yaml   # module matrix + flux source for dev
    stage/
      cluster.yaml   # scaffolded, deferred (see ENVIRONMENTS.md)
  clusters/          # the rendered, deployed desired state (what Flux syncs)
    prod/
      platform.yaml       # environment umbrella HelmRelease (spec.values.apps + .modules)
      flux-instance.yaml  # FluxInstance, sync.path = clusters/prod
    dev/
      platform.yaml
      flux-instance.yaml
  apps/
    metabase.yaml    # optional central catalog entries
  .github/workflows/
    catalog.yml      # publishes the read-only catalog site to Cloudflare Pages
```

`charts/app` is for developer apps. `charts/infra` is for platform-owned infrastructure inside the
cluster: cloudflared, external-secrets, KEDA, monitoring hooks, image-builder, idp-shipper, the
per-app dev stores, and anything yscale-specific. Infra deploys through the same Flux path as apps.

## CLI surface

Read commands (`validate`, `plan`, `catalog`, `doctor`) do not
touch real state. Writes (`render`, `promote`, `remove`, `build`, `infra render/apply`, `dns`,
`tunnel`) mutate git or cloud state.

```text
idpctl init --env dev                                          # scaffold environments/<env>/cluster.yaml from idp.yaml
idpctl new app --name dummy-api --host api.dummyproducts.com --port 8080   # scaffold a starter deploy.yaml
idpctl validate -f deploy.yaml                                 # structural, env-agnostic
idpctl plan --env prod -f deploy.yaml --image <ref>            # validate + policy + render to stdout, no writes
idpctl render --env dev --root <idp clone> -f deploy.yaml --image <ref>    # upsert app into the env umbrella
idpctl promote <app> prod --from dev -f deploy.yaml            # digest-forward; never rebuilds
idpctl remove --app dummy-api --env dev                        # drop a workload/component; Flux prunes
idpctl build --repo <owner>/<repo> --ref <sha> --image <ref>  # in-cluster image-builder -> GHCR
idpctl catalog --env prod --format html                       # read-only view of clusters/<env>/platform.yaml
idpctl doctor --env dev                                        # probe the live cluster for declared seams
idpctl dns sync -f deploy.yaml                                 # Cloudflare CNAMEs for public routes
idpctl tunnel up -f deploy.yaml                                # Cloudflare Tunnel wiring
idpctl infra plan --env prod                                  # enabled modules in the umbrella
idpctl infra render --env dev                                 # write enabled modules into the umbrella
```

`render` is the default deploy verb, but it is a git operation, not a cluster operation. Flux is the
constant writer for both dev and prod. The normal path is:

1. Load `deploy.yaml`.
2. Apply environment defaults (from `environments/<env>/cluster.yaml`).
3. Validate schema and platform policy (including the seam contract).
4. Upsert the app's desired state into the umbrella at `clusters/<env>/platform.yaml`
   (`spec.values.apps`; `infra render` sets `spec.values.modules`).
5. Commit that desired state on the env's `flux.branch` and push.
6. Flux installs the umbrella (`charts/cluster`), which fans out to one inner `HelmRelease`
   per app, store, and module, reconciling them into the target cluster.
7. Emit a concise render summary and URLs.

`plan` does every read/validation/render step without mutating cluster, git, or Cloudflare state.
That gives CI a useful preflight and makes local debugging lower-risk. `disableWait` on a module or
app isolates a failing release so it cannot wedge its siblings.

## Flux model

Use Flux (the Flux Operator plus `HelmRelease`/`GitRepository`) for steady-state reconciliation and
upgrades in every environment. This is the fixed deployment boundary:

```text
developer intent -> idpctl render -> clusters/<env>/platform.yaml (the umbrella HelmRelease)
                 -> Flux installs charts/cluster -> one inner HelmRelease per app/store/module -> cluster
```

`idpctl` generates or updates one umbrella `HelmRelease` per environment at
`clusters/<env>/platform.yaml`, whose values list every app, store, and module. `render` upserts an
app into `spec.values.apps`; `infra render` sets `spec.values.modules`. Flux installs the umbrella
(`charts/cluster`), which templates an isolated inner `HelmRelease` per workload. Never run
`kubectl apply` or `helm upgrade` for a normal application deployment. The Flux
Operator's embedded Web UI (operator Service `:9080`) shows reconciliation state, so there is no
separate dashboard.

```text
platform repo
  clusters/prod/platform.yaml        # umbrella HelmRelease: spec.values.apps + .modules
  clusters/prod/flux-instance.yaml   # FluxInstance (sync.path = clusters/prod)
  clusters/dev/platform.yaml
  clusters/dev/flux-instance.yaml
  charts/cluster/                    # the umbrella chart that fans out to inner HelmReleases
```

Each cluster has one `FluxInstance` (`clusters/<env>/flux-instance.yaml`) whose `spec.sync` creates
the shared `GitRepository` source plus a Kustomization that applies `clusters/<env>`: the umbrella
`HelmRelease` (`platform.yaml`) and the FluxInstance itself. Installing the umbrella, and its fan-out
into one inner `HelmRelease` per app / store / module, is the GitOps replacement for the old Argo CD
app-of-apps. Dev and prod use the same charts and app contract, with environment-specific
implementations:

| Concern | dev k3s cluster | prod k3s cluster |
|---|---|---|
| replicas | usually 1 | profile driven, usually 2+ |
| storage | per-app dev Postgres/Redis; R2/S3 as declared | shared CNPG `prod-pg` + Redis; R2/S3 as declared |
| routes | LAN via expose.lan / MetalLB (no tunnel) | Cloudflare Tunnel + DNS |
| secrets | local external-secrets backend | SSM secrets backend |
| observability | in-cluster Loki | logs shipped to the configured prod Loki endpoint |

### Dev lifecycle: continuous and automatic

The in-cluster `idp-shipper` (`cmd/idp-shipper`) provides continuous development delivery. It is
infra-owned, in-cluster, and enabled per environment (dev's instance uses `registry.env=dev`). Per registered app, every interval it:

1. reads the GitHub head SHA for the app's repo and branch;
2. if the SHA changed, derives the build set from the app manifests (dedup by image);
3. builds only images whose inputs (`build.context` / `build.dockerfile` / `build.submodules`)
   changed, via the in-cluster image-builder (rootless BuildKit to GHCR, tag `<image>:<short-sha>`);
   unchanged images reuse the tag already pinned in the umbrella;
4. renders each component into `clusters/dev/platform.yaml` using `idpctl`'s render core;
5. git commits and pushes to the platform branch;
6. Flux reconciles.

The developer only ever touches their `deploy.yaml`. The shipper registry (which apps to ship: repo,
branch, deploy.yaml paths) is infra-owned config: a ConfigMap declared in
`environments/dev/cluster.yaml` and applied via `idpctl infra render`.

Manual dev deploy (a new or unregistered app, or a deploy from the cockpit):

```text
idpctl render --env dev --root <idp clone> -f deploy.yaml --image <ref>
# then commit + push clusters/dev/platform.yaml on main
```

### Prod lifecycle: deliberate and digest-forward

Production promotes directly from development. Stage is deferred. `environments/prod/cluster.yaml` declares
`promotion.from: dev` (the comment reads "stage is deferred for now, promote dev -> prod directly").

```text
idpctl promote <app> prod --from dev -f deploy.yaml
```

`promote` reads the image digest already running in dev's umbrella and re-renders the app into
`clusters/prod/platform.yaml` with production policy, secrets backend, and namespaces. It does not rebuild
the artifact. Prod refuses mutable tags: `allowMutableTags: false`, and prod is hard-rejected in code
regardless of that flag. Prod's `flux.branch` is `prod`; the prod cluster syncs `refs/heads/prod`.
Commit the promote on the prod branch (the command prints the branch). Rollback is a `git revert` of
that one commit.

Prod also runs its own shipper instance (`registry.env=prod`, `platformBranch: prod`) that builds
from each registered app's branch and commits the `prod` branch, brought online app by app as prod
comes up. The deliberate promote above and the prod shipper coexist: promote pins an exact
dev-tested digest with no rebuild, while the shipper builds fresh from the app branch. For a first
cut, manual commits of the rendered `clusters/*/platform.yaml` are also fine.

### Per-env implementations of declared services

When an application declares services, the development environment supplies the configured
implementation:

- `db: postgres` -> a dedicated per-app dev Postgres release plus database/user/secret wiring and
  `DATABASE_URL`
- `cache: redis` -> a dedicated per-app dev Redis release and `REDIS_URL`
- `storage: r2` or `storage: s3` -> a dev S3-compatible endpoint or dev cloud bucket wiring
- `routes` -> LAN route handling via `expose.lan` / MetalLB (dev has no Cloudflare Tunnel)
- `logging` -> the in-cluster Loki endpoint
- `metrics` -> a scrape config or disabled by env policy

The app contract is stable. The environment decides which implementation backs each capability. In
prod the same `db: postgres` declaration is backed by the shared CloudNativePG `prod-pg` cluster, and
`DATABASE_URL` comes from the SSM secrets backend (prod is PVC-free by contract: `statefulStores:
false`).

## App contract

The app contract describes application requirements rather than Kubernetes resources. The schema lives at
`schemas/deploy.schema.json`; `app` and `runtime` are the only required fields.

```yaml
app: dummy-api

runtime:
  image: ghcr.io/<owner>/dummy-api
  port: 8080

routes:
  - host: api.dummyproducts.com
    public: true
    access:
      humans: false
      serviceToken: true

sizing:
  profile: minimal
  replicas: 2
  autoscale:
    enabled: true
    max: 5

db:
  - name: primary
    type: postgres
    size: minimal

cache:
  - name: default
    type: redis
    size: minimal

storage:
  - name: uploads
    type: r2
    bucket: dummy-api-uploads
    public: false
  - name: exports
    type: s3
    bucket: dummy-api-exports
    public: false

logging:
  enabled: true

metrics:
  enabled: true
```

CI injects the exact image tag with `--image`. The file describes desired app capabilities and
runtime behavior, not Kubernetes resources and not a mutable build artifact.

### Multi-component apps (one file, many parts)

A product that ships several workloads (API, scanner, UI) declares them in one app manifest
through `components:`. The top level is the shared base; each
component carries only its deltas:

```yaml
app: media
runtime: { image: ghcr.io/<owner>/media-api, port: 8000 }     # base image
build:   { submodules: [vendor/transcoder] }                   # declared once
db:      [{ name: primary, type: postgres }]                   # app-level store
cache:   [{ name: events,  type: redis }]

components:
  - component: api                       # inherits the base; provisions the stores (first user)
    sizing: { profile: large }
  - component: scanner                   # portless worker; auto-shares the stores
    port: 0
    env:  { ROLE: scanner }
  - component: ui                        # its own image; opts out of the stores
    runtime: { image: ghcr.io/<owner>/media-ui, port: 80 }
    db: []
    cache: []
    expose: { lan: true }
```

Merge rules (`App.Expand`): pointer fields (`runtime`/`build`/`probes`/`sizing`/`expose`) override
the base. `port:` overrides the port while retaining the base image. `env`
merges (component wins per key). `secrets` union. Slice fields (`volumes`/`routes`/`connectsTo`/
`db`/`cache`/`storage`) are inherit-or-replace: omit to inherit, set (including an explicit `[]`) to
override. App-level stores provision once: the first component to use a store provisions it and
later components auto-share it (no hand-written `provision: false`). A component with `port: 0` is a
worker: Deployment only, no Service and no probes. Each component renders to its own
`<app>-<component>` HelmRelease, identical to separate files. A file with no `components:` is a
plain single-component app. The shipper lists one `deployFile` per app and builds each unique image
once.

### Stackable capabilities

Capabilities are lists because real apps often need more than one of the same type:

- `storage`: multiple R2/S3-compatible buckets for uploads, public assets, exports, backups, etc.
- `db`: one or more databases, usually Postgres first, with room for Mongo or external databases.
- `cache`: multiple Redis instances or logical uses, such as sessions, jobs, and rate limits.
- `routes`: multiple hostnames with different access policies.
- `connectsTo`: multiple app-to-app connections.

The names become stable handles inside the app. For example, `storage: uploads` can produce
`UPLOADS_BUCKET`, `UPLOADS_ENDPOINT`, `UPLOADS_ACCESS_KEY_ID`, and `UPLOADS_SECRET_ACCESS_KEY`.
Another storage entry named `exports` can produce a separate `EXPORTS_*` set, even if it uses a
different provider.

Example mixed storage:

```yaml
storage:
  - name: publicAssets
    type: r2
    bucket: dummy-public-assets
    public: true
  - name: privateUploads
    type: s3
    bucket: dummy-private-uploads
    public: false
  - name: backups
    type: r2
    bucket: dummy-backups
    public: false
```

Generated env vars:

```text
PUBLIC_ASSETS_BUCKET
PUBLIC_ASSETS_ENDPOINT
PUBLIC_ASSETS_ACCESS_KEY_ID
PUBLIC_ASSETS_SECRET_ACCESS_KEY
PRIVATE_UPLOADS_BUCKET
PRIVATE_UPLOADS_ENDPOINT
PRIVATE_UPLOADS_ACCESS_KEY_ID
PRIVATE_UPLOADS_SECRET_ACCESS_KEY
BACKUPS_BUCKET
BACKUPS_ENDPOINT
BACKUPS_ACCESS_KEY_ID
BACKUPS_SECRET_ACCESS_KEY
```

The same rule applies to databases and caches:

```yaml
db:
  - name: primary
    type: postgres
  - name: analytics
    type: postgres

cache:
  - name: sessions
    type: redis
  - name: jobs
    type: redis
```

Generated env vars:

```text
DATABASE_URL              # alias for primary
PRIMARY_DATABASE_URL
ANALYTICS_DATABASE_URL
SESSIONS_REDIS_URL
JOBS_REDIS_URL
```

Only well-known default aliases like `DATABASE_URL` and `REDIS_URL` are optional conveniences (the
first `db` yields `DATABASE_URL`, the first `cache` yields `REDIS_URL`). The canonical names are
always prefixed by capability name so multiple resources never collide.

### Platform-derived naming

Developers do not manually choose SSM paths, Kubernetes Secret names, bucket credentials, or standard
env var names. `idpctl` derives them from app, environment, and capability name.

Naming conventions:

```text
SSM app root:        /apps/<app>/<env>
SSM capability root: /apps/<app>/<env>/<capability>/<name>
Kubernetes namespace:<app>-<env>-<purpose>
Kubernetes Secret:   <app>-runtime
Helm release:        <app>
Service name:        <app>
```

Example derived SSM paths for the YAML above:

```text
/apps/dummy-api/prod/db/primary/DATABASE_URL
/apps/dummy-api/prod/cache/default/REDIS_URL
/apps/dummy-api/prod/storage/uploads/R2_ENDPOINT
/apps/dummy-api/prod/storage/uploads/R2_BUCKET
/apps/dummy-api/prod/storage/uploads/R2_ACCESS_KEY_ID
/apps/dummy-api/prod/storage/uploads/R2_SECRET_ACCESS_KEY
```

For app code, the platform injects predictable env vars:

```text
PORT
ENVIRONMENT
DATABASE_URL
REDIS_URL
UPLOADS_BUCKET
UPLOADS_ENDPOINT
UPLOADS_ACCESS_KEY_ID
UPLOADS_SECRET_ACCESS_KEY
EXPORTS_BUCKET
EXPORTS_ENDPOINT
EXPORTS_ACCESS_KEY_ID
EXPORTS_SECRET_ACCESS_KEY
LOKI_URL
OTEL_EXPORTER_OTLP_ENDPOINT
```

For multiple resources of the same class, the first/default resource gets the plain conventional
name (`DATABASE_URL`, `REDIS_URL`). Named resources also get prefixed names.

Naming rule: convert the capability name to screaming snake case and prepend it to provider-specific
vars. `publicAssets` becomes `PUBLIC_ASSETS_*`; `private-uploads` becomes `PRIVATE_UPLOADS_*`.

### Built-in platform wiring

Some things are automatic unless explicitly disabled:

- logging: Loki endpoint and app labels
- metrics: ServiceMonitor or OpenTelemetry endpoint
- traces: OTEL endpoint when tracing is enabled
- deploy metadata: commit SHA, image tag, deploy time
- probes: generated from `runtime.port` unless the app overrides paths (`probes.type: http|tcp|none`)
- resource defaults: selected from `sizing.profile`
- secrets: ExternalSecret generated from the derived SSM paths

The platform supplies defaults. Reserved `env` keys are dropped;
extra secret env keys go in `secrets[]` (a dev placeholder, the prod backend value).

## API/UI app pairs

Some products are a pair: a frontend UI plus a backend API. Dummy UI and dummy API follow the same
convention as any multi-component product instead of becoming unrelated deploys.

Use either two deploy files:

```text
dummy-api/deploy.yaml
dummy-ui/deploy.yaml
```

or a product-level file with components:

```yaml
product: dummy

components:
  api:
    runtime:
      image: ghcr.io/<owner>/dummy-api
      port: 8080
    routes:
      - host: api.dummyproducts.com
    db:
      - name: primary
        type: postgres
        size: minimal
    cache:
      - name: default
        type: redis

  ui:
    runtime:
      image: ghcr.io/<owner>/dummy-ui
      port: 3000
    routes:
      - host: dummyproducts.com
      - host: www.dummyproducts.com
    connectsTo:
      - component: api
        env: API_BASE_URL
```

Recommendation: start with separate `deploy.yaml` files, because it keeps CI simple and maps cleanly
to separate repos. Add product-level grouping in the platform repo so the UI and API can be viewed,
synced, and cleaned up together later.

Because UI and API live in different repos, each repo gets its own deploy file:

```yaml
app: dummy-api
product: dummy
component: api
```

```yaml
app: dummy-ui
product: dummy
component: ui
connectsTo:
  - app: dummy-api
    env: API_BASE_URL
```

`connectsTo` is declarative. The developer does not hardcode `API_BASE_URL` differently for dev and
prod. `idpctl` resolves it from the target app and environment.

Example resolution:

| Environment | `API_BASE_URL` |
|---|---|
| dev | `http://dummy-api.dummy-api.svc.cluster.local:8080` or the configured LAN route |
| prod | `https://api.dummyproducts.com` when browser-facing, or internal service DNS for service-to-service |

The connection needs a mode so the platform can choose the correct address:

```yaml
connectsTo:
  - app: dummy-api
    env: API_BASE_URL
    mode: publicRoute   # browser calls the API
```

Supported modes:

- `publicRoute`: inject the target app's external route for browser-facing UI-to-API calls.
- `clusterService`: inject the target app's internal Kubernetes service DNS for server-to-server calls.
- `serviceToken`: inject route plus Cloudflare Access service-token refs when the caller must authenticate.
- `scheme: none`: inject a bare `host:port`.

The generated env vars come from the deployment, not from app-owned config. For the UI, the chart
renders:

```text
API_BASE_URL=<resolved target URL>
```

For service-token connections it also renders secret refs such as:

```text
API_CF_ACCESS_CLIENT_ID
API_CF_ACCESS_CLIENT_SECRET
```

Derived conventions:

```text
namespaces:      dummy-api, dummy-ui
SSM roots:       /apps/dummy-api/<env>, /apps/dummy-ui/<env>
Flux HelmReleases: dummy-api, dummy-ui (in flux-system)
product label:   platform/product=dummy
component label: platform/component=api|ui
```

Those labels matter for later cleanup and inventory.

## Upgrade and cleanup

Stable ownership metadata supports upgrades and cleanup.

Upgradeability requirements:

- everything deploys through Helm releases reconciled by Flux (`HelmRelease`)
- app and infra charts are versioned
- generated resources carry stable labels and annotations
- environment-specific overrides live under `environments/<env>/`
- no app hand-writes Kubernetes resources that bypass the chart

Cleanup is possible because all generated resources share ownership metadata:

```text
platform/app=<app>
platform/product=<product>
platform/component=<component>
platform/env=<env>
platform/managed-by=idpctl
```

`idpctl remove --app dummy-api --env dev` drops a workload or component from the umbrella, and Flux
prunes it.

## Developer workflow

The primary interfaces are files, CLI commands, Git history, and Flux status. `idpctl catalog`
provides a read-only inventory; committed desired state remains authoritative.

The normal workflow is:

```text
edit deploy.yaml
run idpctl validate / plan / render
open PR (or push the registered branch and let idp-shipper render)
watch Flux reconcile (flux CLI or the operator Web UI on :9080)
debug with normal Kubernetes/GitOps tools
```

## Potential extensions

These ideas are not part of the current contract.

### Crossplane compatibility

Crossplane would add composition and operational overhead before the core contract requires it.

Still, keep the generated desired-state model compatible with it later. A future `storage: r2`,
`db: postgres`, or `cache: redis` capability could render a Crossplane claim instead of bespoke
provider-specific resources. Crossplane composite resources are relevant because they represent a
set of created resources behind one Kubernetes API and carry ownership metadata that can help with
cleanup.

Source: [Crossplane composite resources][crossplane-composite]

[crossplane-composite]: https://docs.crossplane.io/latest/composition/composite-resources/

### Flux roots

Use one `FluxInstance` per cluster:

```text
clusters/dev/flux-instance.yaml   (sync.path = clusters/dev)
clusters/prod/flux-instance.yaml  (sync.path = clusters/prod)
```

The FluxInstance's sync Kustomization applies `clusters/<env>`: the umbrella `HelmRelease`
(`platform.yaml`) plus the FluxInstance itself. Installing the umbrella (`charts/cluster`) fans out,
from `spec.values`, into one inner `HelmRelease` per:

- platform infra module (the module `HelmRelease`s plus `HelmRepository`s)
- shared service
- app workload (the per-app and per-store `HelmRelease`s)
- product group such as `dummy-api` and `dummy-ui`

This umbrella fan-out is the GitOps replacement for App-of-Apps/ApplicationSet. The point is
inventory and clear sync boundaries, not a portal. Dev and stage share the dev cluster's FluxInstance
(stage is a second Flux Kustomization on the same main source); prod is its own cluster with its own
FluxInstance syncing the prod branch.

### Preview environments

The contract could later support preview environments:

```yaml
environments:
  preview:
    enabled: true
```

A PR can then produce:

```text
dummy-api-pr-123
dummy-ui-pr-123
postgres/redis/storage for that preview
temporary routes
Flux HelmReleases for the preview
```

Preview resources would require ownership labels and deterministic cleanup.

### Feature flags

OpenFeature could provide a vendor-neutral feature-flag contract without coupling applications to a
provider.

Potential contract:

```yaml
flags:
  enabled: true
```

Source: [OpenFeature introduction][openfeature-intro]

[openfeature-intro]: https://openfeature.dev/docs/reference/intro

### Progressive delivery

Start with normal Kubernetes `Deployment`, but keep the chart shape ready for Flagger (the
Flux-native progressive-delivery controller) later.

Potential contract:

```yaml
rollout:
  strategy: canary
```

Future values could include canary weights, analysis checks, manual promotion, or blue/green. This
stays an optional app capability, not the default complexity.

### Other Kubernetes-native pieces to keep in mind

- External Secrets Operator for SSM-backed runtime secrets.
- CloudNativePG for Postgres clusters and backups (the reference prod skeleton uses `prod-pg`).
- KEDA for event-driven scaling and scale-to-zero where appropriate.
- cert-manager where cluster-issued certificates are required; Cloudflare handles public edge TLS.
- NetworkPolicies once namespace boundaries stabilize.
- ResourceQuota and LimitRange per app namespace.
- PodDisruptionBudgets and topology spread for production services.
- ServiceMonitor/PodMonitor or OTEL Collector wiring for observability.

## GitHub Actions path

For apps that are not in the dev shipper registry, the workflow stays thin: build the image, then
render against an idp checkout.

```yaml
name: Ship
on:
  workflow_dispatch:
    inputs:
      confirm:
        description: type PRODUCTION
        required: true

jobs:
  ship:
    runs-on: [self-hosted, k8s, optimized]
    steps:
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v4.2.0
      - name: Build and push image
        run: |
          TAG=prod-${GITHUB_SHA::8}
          IMG=ghcr.io/your-org/${{ github.event.repository.name }}:$TAG
          docker buildx build --platform linux/amd64 -t "$IMG" --push .
          echo "IMG=$IMG" >> "$GITHUB_ENV"
      - uses: actions/checkout@3d3c42e5aac5ba805825da76410c181273ba90b1 # v4.2.0
        with:
          repository: your-org/idp
          ref: v1.0.0
          path: .idp
          token: ${{ secrets.PLATFORM_REPO_TOKEN }}
      - name: Render
        run: .idp/idpctl render --env prod --root .idp --file deploy.yaml --image "$IMG"
```

The CI job then commits or opens a PR against the platform repo on the env's `flux.branch`. Flux
syncs from there. For registered dev apps, the in-cluster `idp-shipper` does all of this on its own;
the only developer action is pushing to the app's branch.

## Helm boundary

Use Helm for the app chart, but keep ownership clear:

- Go owns validation, defaults, policy, cloud provider side effects, and orchestration.
- Helm owns Kubernetes YAML templating for Deployment, Service, PDB, HPA/KEDA, ServiceMonitor, and
  ExternalSecret resources.
- The chart never exposes `Service type=LoadBalancer`, with one labeled exemption: `expose.lan`
  (a MetalLB LAN LoadBalancer carrying `platform/expose: lan`), the only sanctioned LoadBalancer.

The implementation renders Helm/Flux desired state (a `HelmRelease`); it does not run
`helm upgrade --install` directly. A direct Helm apply can remain an emergency/debug command later,
but it is never the default path once Flux is the source of truth.

## Internal extensibility

Use typed internal interfaces before adding a plugin system:

```go
type Step interface {
    Name() string
    Plan(ctx context.Context, app AppConfig) (*ChangeSet, error)
    Apply(ctx context.Context, app AppConfig) error
}
```

Initial steps:

- `ValidateConfig`
- `ResolveEnvironment`
- `RenderNamespace`
- `RenderSecrets`
- `RenderHelmValues`
- `RenderHelmRelease`
- `RenderTunnelRoute`
- `RenderAccess`
- `EmitSummary`

Later additions can be steps without changing the app contract: database provisioning, scheduled
jobs, static asset buckets, preview environments, or yscale-specific scheduling.

## Guardrails

The CLI fails before mutating anything if:

- a rendered Service is `LoadBalancer` and is not the labeled `expose.lan` exemption
- requested hostnames are not in the env's approved `zones`
- resources are missing or outside the env's `resourceBounds`
- an app tries to deploy into another app's namespace
- an image tag is mutable like `latest` and the env sets `allowMutableTags: false` (always true in prod)
- required SSM paths or external secret references are missing
- an app requests a seam the env does not provide (`policy.checkSeams`), e.g. a stateful store or a
  public route in dev (which has no Cloudflare Tunnel)

These are explicit policy errors, not buried Helm failures. The seam contract is data: each env's
`cluster.yaml` declares which seams it backs (`statefulStores`, `lanExpose`, `publicRoutes`,
`autoscale`, `volumes`), `clusterenv.Validate` rejects an env that claims a seam it cannot back, and
`idpctl doctor` probes the live cluster for the declared seams.

## Initial implementation scope

1. Build `idpctl validate`, `idpctl plan`, and `idpctl render`.
2. Port the shared chart skeleton from the existing IDP docs.
3. Upsert one app into the dev umbrella (`clusters/dev/platform.yaml`), which Flux fans out into a per-app Flux HelmRelease.
4. Render declared dev services for that app, starting with the per-app dev Postgres and Redis.
5. Add S3-compatible local/dev storage wiring.
6. Add Cloudflare Tunnel route sync for prod and LAN route handling for dev.
7. Add SSM/ExternalSecret generation for dev and prod paths.
8. Add Cloudflare Access service-token minting.
9. Use `idpctl new app` to make onboarding repeatable.

The initial milestone was one application deployed from `deploy.yaml` through Flux into a
development k3s cluster, including its declared services and without handwritten Kubernetes YAML.

> Note: the pre-built `./idpctl` binary committed in the repo may be wrong-arch. Build from source
> with `make build` if you need to run it.
