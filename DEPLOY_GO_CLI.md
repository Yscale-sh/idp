# Go-first deployment module — design sketch

Status: research/design. No production action taken.

## Goal

Build deployment around a versioned Go CLI instead of shell scripts. App repos submit a small
`deploy.yaml`; CI builds an image; the platform CLI validates the contract, deploys the shared app
chart, updates Cloudflare Tunnel routing/DNS, and provisions any requested platform identity or
secret wiring.

The same contract should deploy to both environments:

- `prod`: the larger Linode k3s production cluster.
- `dev`: the local k3s cluster for development/testing.

The current `DEPLOY_MODULE.md` shape is still right:

- app repo owns code, Dockerfile, and `deploy.yaml`
- a reusable workflow runs on the self-hosted in-cluster GitHub Actions runner
- the platform module is versioned and cloned by the runner
- every app deploys as `ClusterIP` only; public exposure goes through Cloudflare Tunnel

The change is replacing `bin/deploy.sh` and `ensure-*.sh` with one Go binary.

## Why Go

Shell is acceptable for one or two commands, but this platform needs real state handling:

- YAML parsing and schema/default validation
- Kubernetes object reads/writes with idempotency
- Helm install/upgrade/rollback boundaries
- Cloudflare DNS, Tunnel routes, and Access service-token provisioning
- AWS SSM/external-secrets integration
- structured logs and clear failure modes in CI
- a scaffolder that can grow without becoming fragile

Go fits the rest of the stack, builds to a single static-ish binary, and has stable libraries for
Kubernetes, Helm, Cloudflare, and AWS.

## Repository shape

```text
platform/
  cmd/platformctl/
    main.go
  internal/
    appconfig/       # deploy.yaml types, defaults, validation
    deploy/          # high-level deploy orchestration
    helmrunner/      # Helm SDK or command wrapper
    kube/            # Kubernetes clients, namespace/RBAC helpers
    cloudflare/      # DNS, Tunnel config, Access apps/tokens
    secrets/         # SSM + ExternalSecret generation
    scaffold/        # new app/repo/config generation
    policy/          # guardrails: no LoadBalancer, resource limits, naming
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
  clusters/          # the rendered, deployed desired state (what Flux syncs)
    prod/
      platform.yaml       # the ONE umbrella HelmRelease (spec.values.apps + .modules)
      flux-instance.yaml  # FluxInstance, sync.path = clusters/prod
    dev/
      platform.yaml
      flux-instance.yaml
  apps/
    metabase.yaml    # optional central catalog entries
  .github/workflows/
    deploy.yml
```

`charts/app` is for developer apps. `charts/infra` is for platform-owned infrastructure inside the
cluster: cloudflared, external-secrets, KEDA, monitoring hooks, shared Redis/Postgres operators,
and later anything yscale-specific. Infra deploys through the same upgradeable path as apps.

## CLI surface

Start small, but make the command model extensible:

```text
platformctl deploy --app cartogopher --file deploy.yaml --image stackmaster/cartogopher:prod-abc123
platformctl plan   --env prod --app cartogopher --file deploy.yaml --image stackmaster/cartogopher:prod-abc123
platformctl validate --file deploy.yaml
platformctl new app --name dummy-api --host api.dummyproducts.com --port 8080
platformctl infra plan --env prod
platformctl infra apply --env dev
platformctl route sync --file deploy.yaml
platformctl access ensure --file deploy.yaml
```

`deploy` is not the primary production path when Flux is installed. Flux is the constant
writer for both dev and prod. The normal path is:

1. Load `deploy.yaml`.
2. Apply environment defaults.
3. Validate schema and platform policy.
4. Upsert the app's desired state into the umbrella at `clusters/<env>/platform.yaml`
   (`spec.values.apps`; `infra render` sets `spec.values.modules`).
5. Commit or apply that desired state through the platform repo.
6. Flux installs the umbrella (`charts/cluster`), which fans out to an inner HelmRelease
   per app, store, and module, reconciling them into the target cluster.
7. Emit a concise plan/deploy summary and URLs.

`plan` should do every read/validation/render step without mutating cluster or Cloudflare state.
That gives CI a useful preflight and makes local debugging less risky.

## Flux model

Use Flux (the Flux Operator + `HelmRelease`/`GitRepository`) for steady-state reconciliation and
upgrades in every environment. This is the fixed deployment boundary:

```text
developer intent -> platformctl render -> clusters/<env>/platform.yaml (the umbrella HelmRelease)
                 -> Flux installs charts/cluster -> one inner HelmRelease per app/store/module -> cluster
```

`platformctl` generates or updates desired state — ONE umbrella `HelmRelease` per environment at
`clusters/<env>/platform.yaml`, whose values list every app, store, and module. `render` upserts an
app into `spec.values.apps`; `infra render` sets `spec.values.modules`. Flux installs the umbrella
(`charts/cluster`), which templates an isolated inner `HelmRelease` per workload. Avoid direct
cluster mutation for normal app deploys so there is one source of truth. The Flux Operator's embedded
Web UI (operator Service `:9080`) shows reconciliation state — no separate dashboard.

```text
platform repo
  clusters/prod/platform.yaml        # umbrella HelmRelease: spec.values.apps + .modules
  clusters/prod/flux-instance.yaml   # FluxInstance (sync.path = clusters/prod)
  clusters/dev/platform.yaml
  clusters/dev/flux-instance.yaml
  charts/cluster/                    # the umbrella chart that fans out to inner HelmReleases
```

Each environment has one `FluxInstance` (`clusters/<env>/flux-instance.yaml`) whose `spec.sync`
creates the shared `GitRepository` source plus a Kustomization that applies `clusters/<env>` — the
umbrella `HelmRelease` (`platform.yaml`) and the FluxInstance itself. Installing the umbrella, and
its fan-out into one inner `HelmRelease` per app / store / module, is the GitOps replacement for the
old Argo CD app-of-apps. Prod and dev use the same charts and app contract, with environment-specific
implementations:

| Concern | dev local k3s | prod Linode k3s |
|---|---|---|
| replicas | usually 1 | profile driven, usually 2+ |
| storage | local MinIO/R2-dev/S3-dev as declared | R2/S3 as declared |
| routes | local hostnames or tunnel-disabled | Cloudflare Tunnel + DNS |
| secrets | dev SSM path or local external-secrets backend | prod SSM path |
| observability | lightweight | full logging/metrics/tracing |

The production deploy path can be:

```text
app repo push
  -> CI builds image
  -> platformctl render --env prod --file deploy.yaml --image <tag>
     (upserts the app into clusters/prod/platform.yaml)
  -> commit/PR generated artifact into platform repo
  -> Flux syncs prod
```

For the first implementation, manual commits of the rendered `clusters/*/platform.yaml` are fine.
Automating that comes after the chart and YAML contract settle.

### Local developer services

Local dev is not a fake environment. If the app declares services, the platform should deploy the
dev implementations needed to make the app run:

- `db: postgres` -> local/dev Postgres release, database/user/secret wiring
- `cache: redis` -> local/dev Redis release and `REDIS_URL`
- `storage: r2` or `storage: s3` -> local/dev S3-compatible endpoint or dev cloud bucket wiring
- `routes` -> local route/ingress/tunnel configuration
- `logging` -> lightweight Loki or stdout-first config, depending on environment settings
- `metrics` -> lightweight scrape config or disabled by env policy

The app contract is stable. The environment decides which implementation backs each capability.

## App contract

The app contract should feel like a developer shopping list, not like Kubernetes. A developer says
what the app needs; the platform decides how to wire it.

```yaml
app: dummy-api

runtime:
  image: stackmaster/dummy-api
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

### Stackable capabilities

Capabilities are always lists because real apps often need more than one of the same type:

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

Only well-known default aliases like `DATABASE_URL` and `REDIS_URL` are optional conveniences. The
canonical names are always prefixed by capability name so multiple resources never collide.

### Platform-derived naming

Developers should not manually choose SSM paths, Kubernetes Secret names, bucket credentials, or
standard env var names. `platformctl` derives them from app, environment, and capability name.

Default conventions:

```text
SSM app root:        /apps/<app>/<env>
SSM capability root: /apps/<app>/<env>/<capability>/<name>
Kubernetes ns:       <app>
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

For app code, the platform should inject predictable env vars:

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

For multiple resources of the same class, the first/default resource can get the plain conventional
name (`DATABASE_URL`, `REDIS_URL`). Named resources also get prefixed names.

Naming rule: convert the capability name to screaming snake case and prepend it to provider-specific
vars. `publicAssets` becomes `PUBLIC_ASSETS_*`; `private-uploads` becomes `PRIVATE_UPLOADS_*`.

### Built-in platform wiring

Some things should be automatic unless explicitly disabled:

- logging: Loki endpoint and app labels
- metrics: ServiceMonitor or OpenTelemetry endpoint
- traces: OTEL endpoint when tracing is enabled
- deploy metadata: commit SHA, image tag, deploy time
- sane probes: generated from `runtime.port` unless the app overrides paths
- resource defaults: selected from `sizing.profile`
- secrets: ExternalSecret generated from the derived SSM paths

The app YAML stays small because the platform owns the defaults.

## API/UI app pairs

Some products are naturally a pair: a frontend UI plus a backend API. Dummy UI and dummy API should
follow the same convention as AnyRent UI/API instead of becoming unrelated deploys.

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
      image: stackmaster/dummy-api
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
      image: stackmaster/dummy-ui
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
prod. `platformctl` resolves it from the target app and environment.

Example resolution:

| Environment | `API_BASE_URL` |
|---|---|
| dev | `http://dummy-api.dummy-api.svc.cluster.local:8080` or the configured local route |
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

The generated env vars should come from the deployment, not app-owned config. For the UI, the chart
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

## Upgrade and cleanup posture

Design for upgradeability now, cleanup automation later.

Upgradeability requirements:

- everything deploys through Helm releases reconciled by Flux (`HelmRelease`)
- app and infra charts are versioned
- generated resources carry stable labels and annotations
- environment-specific overrides live under `environments/<env>/`
- no app hand-writes Kubernetes resources that bypass the chart

Cleanup later should be possible because all generated resources share ownership metadata:

```text
platform/app=<app>
platform/product=<product>
platform/component=<component>
platform/env=<env>
platform/managed-by=platformctl
```

Future `platformctl destroy --env dev --app dummy-api` can use those labels to preview and remove
resources deliberately. Do not make destroy the first milestone, but do label everything from day
one so cleanup is not painful later.

## Developer experience posture

This is developer experience, not button-clicker experience. The primary interface should be files,
CLI commands, Git history, and Flux status (CLI or the Flux Operator Web UI). A portal can exist
later as an inventory/read-only view, but it should not become the source of truth or hide the
platform contract from developers.

The good developer path is:

```text
edit deploy.yaml
run platformctl validate/plan/render
open PR
watch Flux reconcile (flux CLI or the operator Web UI on :9080)
debug with normal Kubernetes/GitOps tools
```

## Future platform notes

These are worth designing around, but not necessarily implementing in the first build.

### Crossplane compatibility, not day-one Crossplane

Do not start with Crossplane. It adds a lot of machinery and composition design work before the core
platform contract has proven itself.

Still, keep the generated desired-state model compatible with it later. A future `storage: r2`,
`db: postgres`, or `cache: redis` capability could render a Crossplane claim instead of bespoke
provider-specific resources. Crossplane composite resources are relevant because they represent a
set of created resources behind one Kubernetes API and carry ownership metadata that can help with
cleanup.

Source: https://docs.crossplane.io/latest/composition/composite-resources/

### Flux roots

Use one `FluxInstance` per environment:

```text
clusters/dev/flux-instance.yaml   (sync.path = clusters/dev)
clusters/prod/flux-instance.yaml  (sync.path = clusters/prod)
```

The FluxInstance's sync Kustomization applies `clusters/<env>` — the umbrella
`HelmRelease` (`platform.yaml`) plus the FluxInstance itself. Installing the umbrella
(`charts/cluster`) fans out, from `spec.values`, into one inner `HelmRelease` per:

- platform infra module (the module `HelmRelease`s + `HelmRepository`s)
- shared services
- app workload (the per-app + per-store `HelmRelease`s)
- product groups such as `dummy-api` and `dummy-ui`

This umbrella fan-out is the GitOps replacement for App-of-Apps/ApplicationSet. The point is
inventory and clear sync boundaries, not a portal.

### Preview environments

Preview environments are one of the highest-value developer experience features. The contract should
eventually allow:

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

Cleanup matters more here than anywhere else, so all preview resources must carry ownership labels
from day one.

### Feature flags

Feature flags should be part of the platform vocabulary even if the first implementation only wires
configuration. OpenFeature is the CNCF vendor-neutral standard to keep apps from coupling to one flag
provider.

Potential contract:

```yaml
flags:
  enabled: true
```

Source: https://openfeature.dev/docs/reference/intro

### Progressive delivery

Start with normal Kubernetes `Deployment`, but keep the chart shape ready for Flagger (the
Flux-native progressive-delivery controller) later.

Potential contract:

```yaml
rollout:
  strategy: canary
```

Future values could include canary weights, analysis checks, manual promotion, or blue/green. This
should remain an optional app capability, not the default complexity.

### Other Kubernetes-native pieces to keep in mind

- External Secrets Operator for SSM-backed runtime secrets.
- CloudNativePG for Postgres clusters and backups.
- KEDA for event-driven scaling and scale-to-zero where appropriate.
- cert-manager only where cluster-issued certs are actually needed; Cloudflare handles public edge TLS.
- NetworkPolicies once namespace boundaries stabilize.
- ResourceQuota and LimitRange per app namespace.
- PodDisruptionBudgets and topology spread for production services.
- ServiceMonitor/PodMonitor or OTEL Collector wiring for observability.

## GitHub Actions path

The workflow stays thin:

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
      - uses: actions/checkout@v4
      - name: Build and push image
        run: |
          TAG=prod-${GITHUB_SHA::8}
          IMG=stackmaster/${{ github.event.repository.name }}:$TAG
          docker buildx build --platform linux/amd64 -t "$IMG" --push .
          echo "IMG=$IMG" >> "$GITHUB_ENV"
      - uses: actions/checkout@v4
        with:
          repository: JakeNesler/platform
          ref: v1
          path: .platform
          token: ${{ secrets.PLATFORM_REPO_TOKEN }}
      - name: Deploy
        run: .platform/bin/platformctl render --env prod --file deploy.yaml --image "$IMG"
```

The CI job should then commit or open a PR against the platform repo environment path. Flux
syncs from there. For early development, running `platformctl render --env dev` locally and
committing the generated files manually is acceptable.

## Helm boundary

Use Helm for the app chart, but keep ownership clear:

- Go owns validation, defaults, policy, cloud provider side effects, and orchestration.
- Helm owns Kubernetes YAML templating for Deployment, Service, PDB, HPA/KEDA, ServiceMonitor, and
  ExternalSecret resources.
- The chart must never expose `Service type=LoadBalancer`.

Implementation should start by rendering Helm/Flux desired state (a `HelmRelease`), not by running
`helm upgrade --install` directly. Direct Helm apply can remain an emergency/debug command later,
but it must not be the default path once Flux is the source of truth.

## Extensibility model

Avoid plugin complexity on day one. Use typed interfaces internally:

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

Later additions can be steps without changing the app contract: database provisioning,
scheduled jobs, static asset buckets, preview environments, or yscale-specific scheduling.

## Guardrails

The CLI should fail before mutating anything if:

- a rendered Service is `LoadBalancer`
- requested hostnames are not in approved zones
- resources are missing or outside allowed bounds
- an app tries to deploy into another app's namespace
- an image tag is mutable like `latest` for production
- required SSM paths or external secret references are missing

These should be explicit policy errors, not buried Helm failures.

## First implementation slice

1. Create `platformctl validate`, `platformctl plan`, and `platformctl render`.
2. Port the shared chart skeleton from the existing IDP docs.
3. Upsert one app into the dev umbrella (`clusters/dev/platform.yaml`), which Flux fans out into a per-app Flux HelmRelease.
4. Render declared dev services for that app, starting with Postgres and Redis.
5. Add S3-compatible local/dev storage wiring.
6. Add Cloudflare Tunnel route sync for prod and local route handling for dev.
7. Add SSM/ExternalSecret generation for dev and prod paths.
8. Add Cloudflare Access service-token minting.
9. Use `platformctl new app` to make onboarding repeatable.

The first useful milestone is: one app can deploy from `deploy.yaml` through Flux into local k3s,
including the services it declared, with no handwritten Kubernetes YAML and no NodeBalancer.
