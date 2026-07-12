# idpctl conventions: the human contract

The per-app `deploy.yaml` is the product. A developer declares a small app contract
(app, runtime, routes, sizing, db, cache, volumes, connectsTo, ...) and the platform
derives every Kubernetes object from it: namespaces, a Deployment, a ClusterIP
Service, secret refs, env vars (`DATABASE_URL`, `REDIS_URL`, ...), probes, resource
limits, autoscaling, and observability. The developer never writes a Deployment, a
Service, or (by policy) a LoadBalancer.

This is the authoritative, human-readable contract that the machine-readable files
implement:

- `internal/appconfig/types.go`: the deploy.yaml Go structs, defaults, and naming.
- `schemas/deploy.schema.json`: the JSON Schema for `idpctl validate`.
- `charts/app/values.yaml`: the render target the chart consumes.

If any of those disagree with this document, this document is the intent. Fix the
code to match (and update this doc if the intent itself changed).

The flow is always:

```
deploy.yaml -> idpctl render --env <env> --file deploy.yaml --image <tag>
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
`charts/cluster` chart. That chart fans out. It templates an isolated Flux
`HelmRelease` per app (chart `./charts/app`, into `<app>-<env>-<purpose>`), per app
data store (`./charts/infra/dev-postgres`), and per enabled module (plus a
`HelmRepository` for `chartRepo` modules). This is the Helm-native replacement for the
old Argo CD app-of-apps. `idpctl render` **upserts** an app entry into
`spec.values.apps`; `idpctl infra render` **sets** `spec.values.modules`. There are no
per-app `HelmRelease` files anymore. `environments/<env>/cluster.yaml` is renderer
**input** (the module matrix + flux source), not deployed. `disableWait` isolates a
failing app so it cannot wedge its siblings.

**Lifecycle.** Two paths exist, and both end in a git commit that Flux reconciles.

- **dev (continuous, automatic).** The in-cluster `idp-shipper` (`cmd/idp-shipper`)
  is infra-owned and enabled per environment. It realizes "push to your branch, it deploys." Per
  registered app, every interval it reads the GitHub head SHA for the app's
  repo+branch; if the SHA changed, it derives the build set from the shopping lists
  (dedup by image), builds ONLY images whose inputs (`build.context`/`dockerfile`/
  submodules) changed via the in-cluster image-builder (rootless BuildKit to GHCR,
  tag `<image>:<short-sha>`), reuses the tag already pinned in the umbrella for
  unchanged images, renders each component into `clusters/dev/platform.yaml` with
  `idpctl`'s render core, then commits and pushes the platform branch for Flux to
  reconcile. The developer only ever touches `deploy.yaml`. The shipper registry
  (which apps to ship: repo, branch, deploy.yaml paths) is infra-owned config (a
  ConfigMap) declared in `environments/dev/cluster.yaml` and applied via
  `idpctl infra render`. A new or unregistered app deploys manually with
  `idpctl render --env dev --root <idp clone> -f deploy.yaml --image <ref>`, then a
  commit + push of `clusters/dev/platform.yaml` to `main`.
- **prod (deliberate, digest-forward).** Prod promotes FROM dev directly; stage is
  deferred. `idpctl promote <app> prod --from dev -f deploy.yaml` reads the image
  digest already running in dev's umbrella and re-renders the app into
  `clusters/prod/platform.yaml` with prod's policy, secrets backend, and namespaces.
  It NEVER rebuilds the artifact. Prod refuses mutable tags (`allowMutableTags:false`,
  and prod is hard-rejected in code regardless). The prod cluster syncs
  `refs/heads/prod`, so you commit the promote on the `prod` branch (the command
  prints the branch). Rollback is a `git revert` of that one commit. Prod also runs its own shipper
  instance (`registry.env: prod`, `platformBranch: prod`) that commits the `prod` branch, brought
  online app by app. Promote pins an exact dev digest with no rebuild; the prod shipper builds fresh
  from the app branch. Both coexist during bring-up.

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

### Namespace scheme: own namespace per workload, created from the rendered YAML

Every rendered workload gets its OWN namespace, sanitized to an RFC1123 DNS label
(lowercase, `[a-z0-9-]`, no leading/trailing/double dashes, max 63) by
`appconfig.SanitizeDNSLabel`:

- **App workload namespace = `<app>-<env>-<purpose>`**, where `purpose` is the
  deploy.yaml `component` (default `app` when unset). carshowdb sets
  `component: api`, so its namespace is `carshowdb-dev-api` (dev) and
  `carshowdb-prod-api` (prod).
- **Each declared data store gets its OWN namespace** `<app>-<env>-<tool>`
  (`carshowdb` db `[{primary, postgres}]` yields `carshowdb-dev-postgres`; a redis
  cache yields `<app>-<env>-redis`). A second store of the same tool disambiguates
  with the store name: `<app>-<env>-<tool>-<name>`.

**Create-from-yaml.** The umbrella chart templates one inner `HelmRelease` per
app, store, and module, and reconciling each CREATES its namespace. Every inner Flux
`HelmRelease` (the app, each per-app store, and the shared infra modules such as
keda) sets `spec.targetNamespace` (and `spec.storageNamespace`) to its computed
namespace AND sets `spec.install.createNamespace: true` so Flux creates the
namespace on first install. The inner HelmRelease itself is templated into
`flux-system` (the umbrella's source namespace) and references the shared
`GitRepository` source cross-namespace; the platform inventory labels
(`platform/app`, `platform/env`, `platform/component`,
`platform/managed-by=platformctl`) are stamped on it. There is no separate
namespace manifest and no `kubectl create ns`: the HelmRelease is self-contained.

An app deploying into another app's (or another workload's) namespace is a policy
violation. `policy.CheckHelmReleaseTarget` asserts the rendered app HelmRelease's
`spec.targetNamespace` equals `App.Namespace(env)`.

### Per-app dev data stores (dev only)

dev-postgres is NOT a shared module. In **dev**, when an app declares
`db: postgres`, the render path attaches a DEDICATED dev-postgres store to the
app's umbrella entry (`apps[].postgres`): the umbrella chart then templates a
dev-postgres Flux `HelmRelease` (chart `./charts/infra/dev-postgres`) with
`targetNamespace: <app>-<env>-postgres`, the per-app database name (the app name),
a DEV PLACEHOLDER password, the configured dev Postgres node pin, and
`install.createNamespace: true`. The store rides inside the same
`clusters/<env>/platform.yaml` upserted by `idpctl render` (there is no
separate per-store file). The same applies to `cache: redis` once a dev-redis
chart exists.

The dev "data-store defaults" (per-app DB name, placeholder password, node pin,
service/port) live in `internal/clusterenv` (`DevPostgres*` consts +
`DevPostgresNamespace`/`DevPostgresService`/`DevDatabaseURL`) and
`internal/render/store.go`, NOT in `cluster.yaml`.

**`DATABASE_URL` host (dev)** is the CROSS-NAMESPACE FQDN of the per-app Postgres:

```
postgresql://<user>:<pw>@<pg-service>.<app>-<env>-postgres.svc.cluster.local:5432/<db>?sslmode=disable
```

where `<pg-service>` = `<app>-postgres-dev-postgres` (the dev-postgres chart
fullname for release `<app>-postgres`) and `<db>` = the app name. The wiring into
the runtime Secret / `secretKeyRef` is unchanged (Tier D).

**Prod.** Prod runs the dev-store render path NOWHERE: `statefulStores:false`, so the
per-app dev-postgres/dev-redis HelmReleases are never emitted and prod stays PVC-free
by contract. `DATABASE_URL` stays a `secretKeyRef` whose value comes from the SSM
secrets backend (the ExternalSecret path). That SSM value is a full connection string,
so the actual store can be a managed instance or the shared in-cluster CNPG cluster
without the platform render path changing. The prod app namespace is still
`<app>-prod-<component>`.

### Container images

- Registry prefix comes from **`idp.yaml`**. This reference repo uses
  **`ghcr.io/yscale-sh/<app>`**; forks set their own registry prefix there.
- The deploy.yaml `runtime.image` carries the **repository only** (no tag). CI
  injects the concrete immutable tag with `--image`.

### Env-var prefix rule

Capability `name` to `SCREAMING_SNAKE_CASE` prefix, prepended to provider vars
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

## 2. Environment variable tiers (from docs/ENV.md, verbatim intent)

How env reaches a pod: (1) the chart injects Tier-A into every app; (2) a
per-app `ExternalSecret` pulls the app's SSM root into the runtime Secret the pod
`envFrom`s; (3) shared groups are referenced from `/shared/*` so a key is defined
once.

### Tier A: Platform-injected into EVERY app

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

**`IMAGE_NAME` is intentionally NOT injected.** Helm sets the image directly.

### Tier B: Sidecar / infra env (platform-managed)

- **Cloudflare Tunnel**: `TUNNEL_TOKEN` drives the per-app cloudflared sidecar the
  app chart renders for a `public: true` route in a TUNNEL env (prod). In a LAN env
  (dev) the same `public: true` route is served by a MetalLB LoadBalancer instead, with
  no tunnel and no token (see "Routing & exposure" in §7). It is PLATFORM-managed, not a
  developer secret: the operator provisions it in SSM at `/apps/<app>/<env>/TUNNEL_TOKEN`
  when a public route is onboarded (one tunnel + token per app), and the renderer pins it
  as a remoteRef into the app's runtime Secret (so a missing token fails loudly at the
  ExternalSecret). The developer never lists it in deploy.yaml.
- **Tailscale egress**: `TAILSCALE_*` / `TS_*` are injected **only when
  `tailscaleEgress: true`** (e.g. reaching an out-of-cluster DB). Most apps drop
  these once in-cluster.

### Tier C: Shared secret groups (define once, reference from many)

Stored under `/shared/<group>/*`; referenced via the ExternalSecret `remoteRefs`.

| Group (SSM path) | Used by |
|---|---|
| `/shared/stripe` | many apps (e.g. carshowdb) |
| `/shared/sendgrid` | nearly all |
| `/shared/storage` | object-storage creds (one unified R2/S3 convention) |
| `/shared/google-oauth` | end-user "Sign in with Google" |

### Tier D: Data stores

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

These exist for inventory and for label-scoped cleanup (`idpctl remove` drops a
workload or component and Flux prunes it). Label everything from day one.

**ServiceMonitor** additionally needs `release: prometheus` (the
`serviceMonitor.releaseLabel` value, default `prometheus`) so the environment's
kube-prometheus-stack discovers it.

---

## 4. Per-environment secrets backend

The secrets backend is chosen **per environment**, not globally. The env's
backend is configured in `environments/<env>/cluster.yaml`; the app chart's
`ExternalSecret` references that env's SecretStore/ClusterSecretStore.

| Env tier | `externalSecret.backend` | Implementation | `storeRef` |
|---|---|---|---|
| `dev` / on-prem / `local` | `local` | external-secrets Kubernetes provider, or a plain in-cluster Secret for dev | the env's local SecretStore (e.g. `platform-local`) |
| `stage` | `local` | same as dev (stage rides the dev cluster as a second Flux Kustomization, deferred) | the env's local SecretStore |
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

- `localChart`: chart lives in this repo under `charts/` (e.g.
  `charts/app`, `charts/infra/<x>`); `version` comes from the chart itself.
- `chartRepo`: chart pulled from a Helm repo or OCI registry; `version` is
  required and pinned.

`idpctl infra render --env <env>` SETS the ENABLED modules into the umbrella's
`spec.values.modules` (in `clusters/<env>/platform.yaml`). The `charts/cluster`
umbrella then templates, per module, a Flux `HelmRelease` (in `flux-system`,
`install.createNamespace: true`). A `localChart` module references the shared
`GitRepository` source (chart = the in-repo path); a `chartRepo` module ALSO emits
a `HelmRepository` (`source.toolkit.fluxcd.io/v1`) for the chart's Helm repo and
references it from `chart.spec.sourceRef`. Because the module list is fully
re-derived from `cluster.yaml` each run, `infra render` is a set (replace), not an
upsert. `infra apply` is the non-default escape hatch; the default path stays render
plus commit.

**KEDA + the HTTP add-on.** dev enables two related modules: `keda` (chart `keda`)
and `keda-http-add-on` (chart `keda-add-ons-http`). The first provides the
`ScaledObject` CRD (cpu/memory/cron/prometheus triggers); the second provides the
interceptor + scaler that power `HTTPScaledObject` and scale-to-zero (see §8). Both
are required for `autoscale.scaleToZero` apps.

**yscale** is a real chart (`ghcr.io/yscale-sh/yscale-controller`) but ships
`enabled: false`. It bursts ephemeral cloud nodes, so there is nothing to do on a
LAN cluster. It is a `chartRepo` module, disabled by default.

---

## 6. Policy guardrails

`idpctl` must fail **before mutating anything** if any of these hold. They
are explicit policy errors, never buried Helm failures:

1. A rendered Service is `LoadBalancer`. (The chart also cannot express it: there
   is no `service.type` key and the template hardcodes ClusterIP. The only sanctioned
   LoadBalancer is `expose.lan`, a labeled exemption `platform/expose: lan`.)
2. An image tag is mutable (`latest`) in **prod**.
3. A route host is outside the env's approved zones.
4. Resources are missing or outside allowed bounds (profile not in
   minimal|small|medium|large, or limits out of range).
5. An app tries to deploy into another workload's namespace (the rendered app
   HelmRelease's `spec.targetNamespace` != `<app>-<env>-<purpose>`).
6. Required SSM paths / external-secret references are missing.
7. An app requests a seam the env does not provide (`policy.checkSeams`); prod stays
   PVC-free by contract.

---

## 7. Quick reference: deploy.yaml shape

```yaml
app: <app>                     # required; derives all names
product: <product>             # optional
component: api|ui              # optional
runtime:
  image: ghcr.io/<owner>/<app>     # repo only; CI adds the tag
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
secrets:                       # EXTRA secret env keys beyond the universal set (dev placeholder; prod from SSM)
  - AWS_ACCESS_KEY_ID
logging: { enabled: true }
metrics: { enabled: true }
tailscaleEgress: false
```

### Routing & exposure: one route, env-aware

A `route` with `public: true` means **"expose this to users"**; the platform fulfills it per
environment, so ONE deploy.yaml works everywhere:

- **Tunnel env (prod):** a Cloudflare Tunnel + DNS for the host (cloudflared sidecar + the
  `TUNNEL_TOKEN` above).
- **LAN env (dev):** a MetalLB LAN `LoadBalancer`, the on-prem twin of the tunnel, with an IP
  auto-assigned from the env's `lanPool` and the host published via external-dns.

The `host` may be a **bare label** (`web`), composed per env under the env's wildcard zone
(`web.local` in dev, `web.example.com` in prod), or a full hostname (used as-is). A public route is
allowed where the env provides **either** a tunnel (`publicRoutes`) **or** LAN exposure (`lanExpose`),
and denied where it provides neither (fail-closed). dev currently has no Cloudflare Tunnel
(`publicRoutes:false`), so a `public:true` route reaches the LAN via `expose.lan` / MetalLB; the
tunnel path is the prod fulfillment.

`expose` is **optional, advanced pinning**. A public route already gives a LAN `LoadBalancer` in a
LAN env; set `expose.{ip,pool,host,port}` only to pin a specific address or to expose on the LAN with
no route. `expose.lan` is the ONLY sanctioned LoadBalancer (labeled `platform/expose: lan`).

**Multi-component apps: one file for a whole product.** The top level is the shared base; each
`components:` entry carries only its deltas (pointer fields override; `env` merges; `secrets` union;
`db`/`cache`/`volumes` inherit-or-replace, `db: []` opts out; `port:`/`logging:`/`metrics:` override).
App-level stores provision **once** (the first component provisions, siblings auto-share). Each renders
to its own `<app>-<component>` HelmRelease, identical to separate files:

```yaml
app: media
runtime: { image: ghcr.io/<owner>/media-api, port: 8000 }     # base image
db: [{ name: primary, type: postgres }]                        # provisioned once
components:
  - { component: api }                                          # inherits base; provisions the store
  - { component: scanner, port: 0, env: { ROLE: scanner } }     # worker; auto-shares the store
  - { component: ui, runtime: { image: ghcr.io/<owner>/media-ui, port: 80 }, db: [] }    # own image; opts out
```

---

## 8. Autoscaling and scale-to-zero

`sizing.autoscale` is KEDA-driven and picks between two KEDA object kinds:

- **`kind: ScaledObject`** (the default): standard `keda.sh` autoscaling on
  cpu/memory/cron/prometheus triggers. The floor (`min`) stays at or above the seeded
  replica count; the chart REQUIRES at least one trigger, so the renderer always
  emits one (explicit `triggers`, the `metric`/`target` pair, or a default CPU
  trigger).
- **`kind: HTTPScaledObject`**: request-rate autoscaling via the KEDA HTTP add-on,
  capable of scaling all the way to **0**.

**`autoscale.scaleToZero: true`** is sugar for `kind: HTTPScaledObject` + `min: 0`
(authoritative: it overrides both `kind` and `min`). An idle app then scales
fully to **0 replicas** and wakes on the first request:

- the app's route flows through the KEDA HTTP add-on **interceptor**
  (`keda-add-ons-http-interceptor-proxy`), which parks the first request, wakes the
  Deployment from 0, then forwards it, so the **first request after idle eats a
  cold start**;
- it requires the **`keda-http-add-on`** module (chart `keda-add-ons-http`) enabled
  in the env, in addition to the `keda` module;
- the app's **DB does NOT scale to zero**: only the stateless app workload does.

`carshowdb` is the worked example (`autoscale: { enabled: true, scaleToZero: true,
max: 5 }`): rarely hit, so it parks at 0 and wakes on demand. Best for spiky /
rarely-hit apps; latency-sensitive services should keep `min ≥ 1`.

---

## 9. Cloudflare Access dev exposure

An app's `routes[]` shape determines how each host is served per environment:

- **`.local` host** (e.g. `frigate.local`): served via MetalLB LAN LoadBalancer
  only; no Cloudflare in front. Gate access at the network layer.
- **CF-zone host** (e.g. `litewindow-dev.yscale.sh`, matching the dev env's
  `cloudflareZone: yscale.sh`): served via Cloudflare Tunnel + the wildcard
  Access app on `*.yscale.sh`. Policy **requires** `access.humans: true` (or
  `access.serviceToken: true`) on these routes — an unguarded CF-zone route is
  rejected at render time.
- **Both**: an app can carry a `.local` route (LAN, unrestricted) alongside a
  CF-zone route (tunnelled, Access-gated) — declare both in `routes[]`.

**Flow — one-time dev tunnel bootstrap:**

```sh
# 1. Create/adopt the tunnel, mint its connector token, upsert CF DNS.
idpctl tunnel up -e dev --token-out /tmp/tunnel-token

# 2. Stash the token in the cluster so the dev ExternalSecret resolves it.
scripts/setup-dev-tunnel-secret.sh <app> /tmp/tunnel-token
rm /tmp/tunnel-token    # delete after stashing

# 3. Render and commit so Flux picks up the cloudflared sidecar + ExternalSecret.
idpctl render -e dev --file deploy.yaml --image <tag>
# ... git commit + push, wait for Flux reconcile

# 4. Confirm every CF-zone host redirects to Cloudflare Access (re-run if needed).
idpctl tunnel up -e dev --verify-access --skip-dns
```

`--verify-access` (on by default when `cloudflareZone` is set) probes each
CF-zone host with a no-follow GET and expects a 30x redirect to
`.cloudflareaccess.com`. It retries with backoff up to ~90 s to allow for DNS
and certificate propagation, then fails naming any unprotected host.
