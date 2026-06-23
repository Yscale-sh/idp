# idp: deploying your app (agent quickstart)

This is the **idp** platform. You describe your app in ONE `deploy.yaml` and the platform
derives all the Kubernetes for you: it builds the image, wires secrets, DBs, cache, and
routing, then ships it via GitOps (Flux). The `deploy.yaml` shopping list IS the product:
you declare a small app contract (app, runtime, routes, sizing, db, cache, volumes,
connectsTo) and the platform produces the namespaces, Deployment, ClusterIP Service, secret
refs, env vars, probes, resource limits, autoscaling, and observability. **You never write a
Deployment, Service, or LoadBalancer.** This file is the fast path. Deeper docs are linked at
the end.

> **Bundled agent skill.** This repo ships `.claude/skills/idp/SKILL.md`, which Claude Code
> auto-loads in any session here. It covers the idpctl command surface, the shopping-list
> contract, the dev and prod lifecycle, and the fail-closed rails. Read it before running idpctl.

## The model in one breath

`deploy.yaml` (a declaration of what your app needs) -> `idpctl render` (expands it into the
env's ONE umbrella HelmRelease) -> git commit on the env's Flux branch -> **Flux** reconciles
it onto the cluster. Flux is the only writer: never `kubectl apply` or `helm upgrade`. Public
apps get a Cloudflare Tunnel in prod. DB, cache, and secret URLs are injected as env.

## Minimal deploy.yaml

```yaml
app: myapp                       # unique app name (lowercase)
runtime:
  image: ghcr.io/jakenesler/myapp   # repo ONLY, NO tag (CI/--image supplies it)
  port: 8080                     # the port your server listens on
routes:
  - host: api                    # bare label -> api.<env-zone> (e.g. api.myapp.com in prod)
    public: true                 # expose to the internet (Cloudflare Tunnel in prod)
db:
  - { name: primary, type: postgres }   # -> DATABASE_URL injected (from SSM in prod)
secrets:                         # keys pulled from SSM /apps/myapp/<env>/<KEY>
  - DATABASE_URL                 # NOTE: db: primary needs DATABASE_URL + PRIMARY_DATABASE_URL
  - PRIMARY_DATABASE_URL         #       listed here too, or the pod CreateContainerConfigErrors
  - JWT_SECRET
env:                             # non-secret config (committed, injected as plain env)
  LOG_LEVEL: info
probes:
  path: /health                  # your HTTP health route (or `type: tcp` if none)
sizing:
  profile: minimal
  replicas: 2
```

## The fields you'll use

| field | what it does |
|---|---|
| `app` / `component` | app name; `component` (api/ui/worker) -> workload `<app>-<component>`. |
| `runtime.image` | `ghcr.io/<owner>/<name>` **tagless**: the tag comes from `idpctl render --image <tag>`. |
| `runtime.port` | the container port your server binds (`0` makes a worker: Deployment only, no Service or probes). |
| `routes[]` | `{host, public, access}`. `host` is a bare label composed under the env zone, or a full FQDN. `public: true` -> Cloudflare Tunnel (prod) / MetalLB LAN LoadBalancer (dev). |
| `db` / `cache` | `[{name, type: postgres\|redis}]` -> injects `DATABASE_URL` / `REDIS_URL` (from SSM in prod, in-cluster dev-postgres/dev-redis in dev). |
| `secrets[]` | list of KEY names -> synced from SSM `/apps/<app>/<env>/<KEY>` into your env. Shared values live at `/shared/<group>/<KEY>`. |
| `env{}` | plain non-secret config (committed; reserved keys are dropped). |
| `probes` | `{path: /health}` (HTTP) or `{type: tcp}` (no HTTP health route) or `{type: none}`. A wrong path CrashLoops the pod. |
| `sizing` | `{profile: minimal\|small\|..., replicas: N, autoscale: {enabled}}`. |
| `logging.enabled` | ship structured logs to Loki (`LOKI_URL` is injected). |
| `metrics.enabled` | ServiceMonitor: **must be false in prod** (no Prometheus there yet). |
| `connectsTo[]` | app-to-app wiring, resolved per env (clusterService / publicRoute / serviceToken; `scheme: none` -> bare `host:port`). |
| `build` | `{context, dockerfile, submodules}` for building your image from a subdir/monorepo. |
| `components[]` | multi-component product (api + ui + worker) in ONE file: see below. |

## Images: built for you

Your `runtime.image` is **tagless**. The platform builds and tags it:
- **dev (idp-shipper):** push to your app's registered branch. The in-cluster idp-shipper
  reads the new head SHA, builds only the images whose inputs changed via the in-cluster
  image-builder (rootless BuildKit), pushes `ghcr.io/<owner>/<app>:<short-sha>`, renders,
  commits, and pushes the platform branch. Flux reconciles. See the lifecycle section below.
- **Manual:** `idpctl build --repo <org>/<repo> --ref <sha> --build-context <dir> --image ghcr.io/<owner>/<app>:<sha>`
- **prod rejects `:latest`:** always a commit-SHA or version tag.

## Secrets and stores

- Declare secret KEY names in `secrets:`, then seed the values in **SSM** at
  `/apps/<app>/<env>/<KEY>` (the platform syncs them via external-secrets). Shared creds live
  at `/shared/<group>/*`.
- `db: postgres` -> `DATABASE_URL` (plus `PRIMARY_DATABASE_URL`); `cache: redis` -> `REDIS_URL`.
  In prod these come from the SSM secrets backend (DATABASE_URL points at the in-cluster CNPG
  service; prod stays PVC-free by contract). In dev they're spun up in-cluster automatically.
  **Don't hardcode connection strings: declare the store.**

## Multi-component apps (api + ui + worker)

A multi-component product is ONE file. The top level is the shared BASE and each
`components[]` entry carries only its deltas. `Expand()` yields one isolated HelmRelease per
component:

```yaml
app: myapp
runtime: { image: ghcr.io/jakenesler/myapp-api, port: 8080 }   # base (api inherits)
components:
  - component: api
    db: [{ name: primary, type: postgres }]
    secrets: [DATABASE_URL, PRIMARY_DATABASE_URL]
  - component: worker
    port: 0                       # no HTTP server
    db: []                        # opt out of the base stores with []
  - component: ui
    runtime: { image: ghcr.io/jakenesler/myapp-ui, port: 80 }
    probes: { type: tcp }
    routes: [{ host: app, public: true }]   # routes live on the component that serves them
```

Merge rules: pointer fields (runtime/probes/sizing) override; `env` merges; `secrets` union;
slice fields (db/cache/routes/volumes) inherit, or set `[]` to opt out. **All components of one
app share ONE image TAG** (`--image <tag>` -> `<each component's repo>:<tag>`).

> **Path-based routing** (one domain, different paths -> different components) is NOT a
> deploy.yaml feature: idp routes are host-to-one-service. If you need it, add a small nginx
> `router` component (see `examples/cartogopher` / `examples/openprophet`).

## Deploy lifecycle

Deploying is a git commit. `idpctl render` upserts your app into the env's ONE umbrella
HelmRelease at `clusters/<env>/platform.yaml`, whose values list every app. The
`charts/cluster` chart templates one isolated HelmRelease per app (plus its dedicated stores
and each enabled module). You commit `platform.yaml` on the env's Flux branch and Flux
reconciles it. `disableWait` isolates a failing app so it cannot wedge its siblings.

### dev (continuous and automatic)

The in-cluster **idp-shipper** realizes "push to your branch, it deploys." It is
infra-owned, in-cluster, and enabled per environment (dev and prod each run their own instance;
prod commits the `prod` branch). Per registered app, every interval it reads the GitHub
head SHA for the app's repo and branch; if it changed, it derives the build set from the
shopping lists (deduped by image), builds only the images whose inputs
(`build.context`/`dockerfile`/`submodules`) changed via the in-cluster image-builder, renders
each component into `clusters/dev/platform.yaml` using idpctl's render core, then commits and
pushes the platform branch (`main`). Flux reconciles. Unchanged images reuse the tag already
pinned in the umbrella. You only ever touch your `deploy.yaml`.

The shipper registry (which apps to ship: repo, branch, deploy.yaml paths) is infra-owned
config (a ConfigMap) declared in `environments/dev/cluster.yaml` and applied via
`idpctl infra render`.

To deploy a new or unregistered app manually (or from the cockpit):

```bash
idpctl validate -f deploy.yaml
idpctl render --env dev --root <idp clone> -f deploy.yaml --image <ref>
git commit ... && git push     # clusters/dev/platform.yaml on main; Flux reconciles
# public app, one-time: idpctl tunnel up --env <env> -f deploy.yaml --zone-id <cf-zone>
```

### prod (deliberate and digest-forward)

Prod promotes FROM dev directly. Stage is scaffolded in-repo but deferred, so the live path
today is dev -> prod (`environments/prod/cluster.yaml` declares `promotion.from: dev`).

```bash
idpctl promote <app> prod --from dev -f deploy.yaml
git commit ... && git push     # clusters/prod/platform.yaml on the prod branch
```

`idpctl promote` reads the image digest already running in dev's umbrella and re-renders the
app into `clusters/prod/platform.yaml` with prod's policy, secrets backend, and namespaces.
**It never rebuilds the artifact.** Commit the promote on the **prod** branch (the command
prints the branch); the prod cluster syncs `refs/heads/prod`. Rollback is `git revert` of that
one commit.

Prod refuses mutable tags: `allowMutableTags: false`, and prod is hard-rejected in code
regardless. The prod cluster is self-provisioned k3s on Linode (the jaK3s golden image:
hardened Debian, Cilium, embedded etcd, kube-vip; etcd snapshots ship to R2). Prod exposure is
Cloudflare Tunnel only (no MetalLB); `statefulStores: false`, so apps get `DATABASE_URL` from
the SSM secrets backend.

## Gotchas (read before your first deploy)

- `db: primary` injects `DATABASE_URL` and `PRIMARY_DATABASE_URL` env but does NOT auto-add
  them to the secret. **List both in `secrets:`** and seed them in SSM.
- `metrics.enabled` and any `monitoring.coreos.com` resource must be **off in prod**.
- A tunnel'd (`public: true`) app can't `scaleToZero` (the cloudflared sidecar dies). Keep
  `autoscale.enabled: false` (or fixed replicas) until a keda-http design lands.
- dev has NO Cloudflare Tunnel (`publicRoutes: false`), so a `public: true` route is rejected
  in dev and stage today. LAN reach is via `expose.lan` (the only sanctioned LoadBalancer).
- Wrong `probes.path` -> CrashLoop. Use `type: tcp` if you have no HTTP health route.
- The pre-built `./idpctl` binary in the repo may be wrong-arch. Build from source with
  `make build` if you need to run it.

## Go deeper

`docs/IDP.md` (architecture) · `docs/ENV.md` + `docs/SECRETS.md` (env/secrets contract) ·
`docs/PLATFORM_DX.md` (DX) · `docs/POC_CARSHOWDB.md` (a worked example) ·
`agents/PROD_ROLLOUT.md` (prod cutover playbook plus landmines) · `examples/` (real deploy.yamls).
