# platformctl conventions — the human contract

This is the authoritative, human-readable contract that the machine-readable
files implement:

- `internal/appconfig/types.go` — the deploy.yaml Go structs + defaults + naming.
- `schemas/deploy.schema.json` — JSON Schema for `platformctl validate`.
- `charts/app/values.yaml` — the render target the chart consumes.

If any of those disagree with this document, this document is the intent; fix the
code to match (and update this doc if the intent itself changed).

The flow is always:

```
deploy.yaml -> platformctl render --env <env> --file deploy.yaml --image <tag>
            -> clusters/<env>/platform.yaml  (the ONE umbrella HelmRelease; render
               UPSERTS this app into spec.values.apps)
            -> Flux installs charts/cluster, which templates an ISOLATED Flux
               HelmRelease per app (+ its Postgres) and per module
            -> Flux reconciles into the cluster
```

Flux (Flux Operator + `HelmRelease`/`GitRepository`) is the **only writer**.
"Deploying" is a git commit, never `kubectl`/`helm` by hand. The Flux Operator's
embedded Web UI (operator Service `:9080`) shows reconciliation state.

**Umbrella delivery model.** The rendered desired state for an env is ONE umbrella
`HelmRelease` at `clusters/<env>/platform.yaml` (in `flux-system`) that installs the
`charts/cluster` chart. That chart fans out — it templates an isolated Flux
`HelmRelease` per app (chart `./charts/app`, into `<app>-<env>-<purpose>`), per app
data store (`./charts/infra/dev-postgres`), and per enabled module (+ a
`HelmRepository` for `chartRepo` modules) — the Helm-native replacement for the old
Argo CD app-of-apps. `platformctl render` **upserts** an app entry into
`spec.values.apps`; `platformctl infra render` **sets** `spec.values.modules`. There
are no per-app `HelmRelease` files anymore. `environments/<env>/cluster.yaml` is
renderer **input** (the module matrix + flux source), not deployed.

---

## 1. Naming derivation

Developers never choose Kubernetes names, secret names, or SSM paths. Everything
derives from `app`, the target `env`, and capability `name`. Single source of
truth: the methods on `appconfig.App`.

| Thing | Value | Source method |
|---|---|---|
| Kubernetes namespace (app workload) | `<app>-<env>-<purpose>` | `App.Namespace(env)` |
| Workload purpose | `component` (default `app`) | `App.Purpose()` |
| Data-store namespace | `<app>-<env>-<tool>` (`-<name>` for a 2nd of a tool) | `App.StoreNamespace(env, tool, name, secondary)` |
| Helm release | `<app>` | `App.ReleaseName()` |
| Service (ClusterIP) | `<app>` | `App.ServiceName()` |
| Runtime Secret | `<app>-runtime` | `App.SecretName()` |
| Flux HelmRelease | `<app>` (in `flux-system`) | `App.ReleaseHandle()` |
| SSM app root | `/apps/<app>/<env>` | `App.SSMRoot(env)` |
| SSM capability path | `/apps/<app>/<env>/<capability>/<name>` | `App.SSMCapabilityPath(...)` |
| Shared secret group | `/shared/<group>/*` | (stripe, sendgrid, storage, google-oauth) |

### Namespace scheme — own namespace per workload, created from the rendered YAML

Every rendered workload gets its OWN namespace, sanitized to an RFC1123 DNS label
(lowercase, `[a-z0-9-]`, no leading/trailing/double dashes, max 63) by
`appconfig.SanitizeDNSLabel`:

- **App workload namespace = `<app>-<env>-<purpose>`**, where `purpose` is the
  deploy.yaml `component` (default `app` when unset). carshowdb sets
  `component: api`, so its namespace is `carshowdb-dev-api` (dev) /
  `carshowdb-prod-api` (prod).
- **Each declared data store gets its OWN namespace** `<app>-<env>-<tool>`
  (`carshowdb` db `[{primary, postgres}]` → `carshowdb-dev-postgres`; a redis
  cache → `<app>-<env>-redis`). A second store of the same tool disambiguates
  with the store name: `<app>-<env>-<tool>-<name>`.

**Create-from-yaml.** The umbrella chart templates one inner `HelmRelease` per
app / store / module, and reconciling each CREATES its namespace. Every inner Flux
`HelmRelease` — the app, each per-app store, and the shared infra modules (e.g.
keda) — sets `spec.targetNamespace` (and `spec.storageNamespace`) to its computed
namespace AND sets `spec.install.createNamespace: true` so Flux creates the
namespace on first install. The inner HelmRelease itself is templated into
`flux-system` (the umbrella's source namespace) and references the shared
`GitRepository` source cross-namespace; the platform inventory labels
(`platform/app`, `platform/env`, `platform/component`,
`platform/managed-by=platformctl`) are stamped on it. There is no separate
namespace manifest and no `kubectl create ns` — the HelmRelease is self-contained.

An app deploying into another app's (or another workload's) namespace is a policy
violation; `policy.CheckHelmReleaseTarget` asserts the rendered app HelmRelease's
`spec.targetNamespace` equals `App.Namespace(env)`.

### Per-app dev data stores (dev only)

dev-postgres is NOT a shared module. In **dev**, when an app declares
`db: postgres`, the render path attaches a DEDICATED dev-postgres store to the
app's umbrella entry (`apps[].postgres`): the umbrella chart then templates a
dev-postgres Flux `HelmRelease` (chart `./charts/infra/dev-postgres`) with
`targetNamespace: <app>-<env>-postgres`, the per-app database name (the app name),
a DEV PLACEHOLDER password, the `optiplex-pg` node pin, and
`install.createNamespace: true`. The store rides inside the same
`clusters/<env>/platform.yaml` upserted by `platformctl render` (there is no
separate per-store file). The same applies to `cache: redis` once a dev-redis
chart exists.

The dev "data-store defaults" (per-app DB name, placeholder password, node pin,
service/port) live in `internal/clusterenv` (`DevPostgres*` consts +
`DevPostgresNamespace`/`DevPostgresService`/`DevDatabaseURL`) and
`internal/render/store.go` — NOT in `cluster.yaml`.

**`DATABASE_URL` host (dev)** is the CROSS-NAMESPACE FQDN of the per-app Postgres:

```
postgresql://<user>:<pw>@<pg-service>.<app>-<env>-postgres.svc.cluster.local:5432/<db>?sslmode=disable
```

where `<pg-service>` = `<app>-postgres-dev-postgres` (the dev-postgres chart
fullname for release `<app>-postgres`) and `<db>` = the app name. The wiring into
the runtime Secret / `secretKeyRef` is unchanged (Tier D).

**Prod.** Prod provisions NO per-app Postgres (the DB is external/managed): no
prod Postgres namespace, no dev-postgres HelmRelease. `DATABASE_URL` stays a
`secretKeyRef` whose value comes from SSM (the ExternalSecret path). The prod app
namespace is still `<app>-prod-<component>`.

### Container images

- Primary registry: **`ghcr.io/jakenesler/<app>`** (GitHub Container Registry).
- Legacy `stackmaster/*` (Docker Hub) images still exist and are being
  republished to ghcr. New work targets ghcr.
- The deploy.yaml `runtime.image` carries the **repository only** (no tag). CI
  injects the concrete immutable tag with `--image`.

### Env-var prefix rule

Capability `name` -> `SCREAMING_SNAKE_CASE` prefix, prepended to provider vars
(`appconfig.EnvPrefix`):

- `publicAssets` -> `PUBLIC_ASSETS_*`
- `private-uploads` -> `PRIVATE_UPLOADS_*`

The **first/default** resource of each class also gets the bare alias
(`DATABASE_URL`, `REDIS_URL`); every named resource always gets the prefixed form
so multiple resources never collide.

```
db:   primary  -> DATABASE_URL (alias) + PRIMARY_DATABASE_URL
db:   analytics-> ANALYTICS_DATABASE_URL
cache:sessions -> SESSIONS_REDIS_URL
storage:uploads-> UPLOADS_BUCKET / UPLOADS_ENDPOINT / UPLOADS_ACCESS_KEY_ID / UPLOADS_SECRET_ACCESS_KEY
```

---

## 2. Environment variable tiers (from ENV.md — verbatim intent)

How env reaches a pod: (1) the chart injects Tier-A into every app; (2) a
per-app `ExternalSecret` pulls the app's SSM root into the runtime Secret the pod
`envFrom`s; (3) shared groups are referenced from `/shared/*` so a key is defined
once.

### Tier A — Platform-injected into EVERY app

The chart sets these; apps stop hard-coding them. Rendered into
`env.tierA` in values.yaml.

| Var | Source |
|---|---|
| `ENVIRONMENT` | chart (e.g. `dev` / `prod`) |
| `LOKI_URL` | platform Loki endpoint |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | platform OTEL collector |
| `CONSOLE_LOGGING` | chart |
| `DEPLOY_TIME` | CI (commit/build stamp) |
| `PORT` | from `deploy.yaml` (`runtime.port`) |

**`IMAGE_NAME` is intentionally NOT injected** — Helm sets the image directly.

### Tier B — Sidecar / infra env (platform-managed, NOT in per-app SSM)

- **Cloudflare Tunnel**: `TUNNEL_TOKEN` — platform-owned (shared `cloudflared` or
  per-app token).
- **Tailscale egress**: `TAILSCALE_*` / `TS_*` — injected **only when
  `tailscaleEgress: true`** (e.g. reaching an out-of-cluster DB). Most apps drop
  these once in-cluster.

### Tier C — Shared secret groups (define once, reference from many)

Stored under `/shared/<group>/*`; referenced via the ExternalSecret `remoteRefs`.

| Group (SSM path) | Used by |
|---|---|
| `/shared/stripe` | many (carshowdb, cartogopher, anyrent, ...) |
| `/shared/sendgrid` | nearly all |
| `/shared/storage` | object-storage creds (one unified R2/S3 convention) |
| `/shared/google-oauth` | end-user "Sign in with Google" |

### Tier D — Data stores

The chart wires these from the runtime Secret; the data stores are separate
releases/modules pointed at in-cluster Postgres/Redis.

| Engine | Vars |
|---|---|
| Postgres | `DATABASE_URL` (first/default db) + `<NAME>_DATABASE_URL` |
| Redis | `REDIS_URL` (first/default cache) + `<NAME>_REDIS_URL` |
| MongoDB | external exception (`MONGO_USERNAME` / `MONGO_PASSWORD`) |

---

## 3. Label set (stamped on every generated object)

Rendered from `App.Labels(env)` into `platform.*` in values.yaml and applied by
the chart's label helper:

```
app.kubernetes.io/name      = <app>
app.kubernetes.io/instance  = <app>
platform/app                = <app>
platform/env                = <env>
platform/managed-by         = platformctl
platform/product            = <product>     # only when set
platform/component          = <component>   # only when set
```

These exist for inventory and future `platformctl destroy --env <env> --app <app>`
label-scoped cleanup. Label everything from day one.

**ServiceMonitor** additionally needs `release: prometheus` (the
`serviceMonitor.releaseLabel` value, default `prometheus`) so the homelab
kube-prometheus-stack discovers it.

---

## 4. Per-environment secrets backend

The secrets backend is chosen **per environment**, not globally. The env's
backend is configured in `environments/<env>/cluster.yaml`; the app chart's
`ExternalSecret` references that env's SecretStore/ClusterSecretStore.

| Env tier | `externalSecret.backend` | Implementation | `storeRef` |
|---|---|---|---|
| `dev` / on-prem / `local` | `local` | external-secrets Kubernetes provider, or a plain in-cluster Secret for dev | the env's local SecretStore (e.g. `platform-local`) |
| `prod` / cloud | `ssm` | AWS SSM Parameter Store via external-secrets | the env's SSM ClusterSecretStore (e.g. `platform-ssm`) |

SSM path convention:

- App secrets: `/apps/<app>/<env>/...`
- Shared groups: `/shared/<group>/*` (stripe, sendgrid, storage, google-oauth)

The renderer sets `externalSecret.backend` from the target env and points
`storeRef` at the store named in that env's `cluster.yaml`. No backend choice
leaks into the app's deploy.yaml.

---

## 5. Module registry schema

Platform infra and shared services (cloudflared, external-secrets, KEDA,
monitoring, shared Postgres/Redis operators, yscale) are declared as a registry,
typically in `environments/<env>/cluster.yaml` (or a sibling `modules.yaml`).
Each entry:

```yaml
modules:
  <module-name>:
    enabled: true|false
    source: localChart | chartRepo   # in-repo charts/ path vs a Helm repo/OCI ref
    chart: <path-or-name>            # charts/infra/<x> (localChart) or repo chart name
    version: <semver>                # required for chartRepo; ignored for localChart
    namespace: <ns>                  # target namespace for the release
    values: { ... }                  # inline values overrides (optional)
```

- `localChart` -> chart lives in this repo under `charts/` (e.g.
  `charts/app`, `charts/infra/<x>`); `version` comes from the chart itself.
- `chartRepo` -> chart pulled from a Helm repo or OCI registry; `version` is
  required and pinned.

`platformctl infra render --env <env>` SETS the ENABLED modules into the umbrella's
`spec.values.modules` (in `clusters/<env>/platform.yaml`). The `charts/cluster`
umbrella then templates, per module, a Flux `HelmRelease` (in `flux-system`,
`install.createNamespace: true`). A `localChart` module references the shared
`GitRepository` source (chart = the in-repo path); a `chartRepo` module ALSO emits
a `HelmRepository` (`source.toolkit.fluxcd.io/v1`) for the chart's Helm repo and
references it from `chart.spec.sourceRef`. Because the module list is fully
re-derived from `cluster.yaml` each run, `infra render` is a set (replace), not an
upsert.

**KEDA + the HTTP add-on.** dev enables two related modules: `keda` (chart `keda`)
and `keda-http-add-on` (chart `keda-add-ons-http`). The first provides the
`ScaledObject` CRD (cpu/memory/cron/prometheus triggers); the second provides the
interceptor + scaler that power `HTTPScaledObject` and scale-to-zero (see §8). Both
are required for `autoscale.scaleToZero` apps.

**yscale** is a real chart (`ghcr.io/jakenesler/yscale-controller`) but ships
`enabled: false` — it bursts ephemeral cloud nodes, so there is nothing to do on
a LAN cluster. It is a `chartRepo` module, disabled by default.

---

## 6. Policy guardrails

`platformctl` must fail **before mutating anything** if any of these hold. They
are explicit policy errors, never buried Helm failures:

1. A rendered Service is `LoadBalancer`. (The chart also cannot express it — there
   is no `service.type` key and the template hardcodes ClusterIP.)
2. An image tag is mutable (`latest`) in **prod**.
3. A route host is outside the env's approved zones.
4. Resources are missing or outside allowed bounds (profile not in
   minimal|small|medium|large, or limits out of range).
5. An app tries to deploy into another workload's namespace (the rendered app
   HelmRelease's `spec.targetNamespace` != `<app>-<env>-<purpose>`).
6. Required SSM paths / external-secret references are missing.

---

## 7. Quick reference: deploy.yaml shape

```yaml
app: <app>                     # required; derives all names
product: <product>             # optional
component: api|ui              # optional
runtime:
  image: ghcr.io/jakenesler/<app>   # repo only; CI adds the tag
  port: 8080
routes:
  - host: <host>
    public: true
    access: { humans: false, serviceToken: true }
sizing:
  profile: minimal             # minimal|small|medium|large
  replicas: 2
  autoscale: { enabled: true, min: 2, max: 5, kind: ScaledObject }
  # scale-to-zero alternative (sugar for kind: HTTPScaledObject + min: 0):
  # autoscale: { enabled: true, scaleToZero: true, max: 5 }
db:
  - { name: primary, type: postgres, size: minimal }
cache:
  - { name: default, type: redis, size: minimal }
storage:
  - { name: uploads, type: r2, bucket: <app>-uploads, public: false }
connectsTo:
  - { app: <other>, env: API_BASE_URL, mode: publicRoute }
logging: { enabled: true }
metrics: { enabled: true }
tailscaleEgress: false
```

---

## 8. Autoscaling and scale-to-zero

`sizing.autoscale` is KEDA-driven and picks between two KEDA object kinds:

- **`kind: ScaledObject`** (the default) — standard `keda.sh` autoscaling on
  cpu/memory/cron/prometheus triggers. The floor (`min`) stays ≥ the seeded
  replica count; the chart REQUIRES at least one trigger, so the renderer always
  emits one (explicit `triggers`, the `metric`/`target` pair, or a default CPU
  trigger).
- **`kind: HTTPScaledObject`** — request-rate autoscaling via the KEDA HTTP add-on,
  capable of scaling all the way to **0**.

**`autoscale.scaleToZero: true`** is sugar for `kind: HTTPScaledObject` + `min: 0`
(authoritative — it overrides both `kind` and `min`). An idle app then scales
fully to **0 replicas** and wakes on the first request:

- the app's route flows through the KEDA HTTP add-on **interceptor**
  (`keda-add-ons-http-interceptor-proxy`), which parks the first request, wakes the
  Deployment from 0, then forwards it — so the **first request after idle eats a
  cold start**;
- it requires the **`keda-http-add-on`** module (chart `keda-add-ons-http`) enabled
  in the env, in addition to the `keda` module;
- the app's **DB does NOT scale to zero** — only the stateless app workload does.

`carshowdb` is the worked example (`autoscale: { enabled: true, scaleToZero: true,
max: 5 }`): rarely hit, so it parks at 0 and wakes on demand. Best for spiky /
rarely-hit apps; latency-sensitive services should keep `min ≥ 1`.
