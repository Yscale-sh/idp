# idp — deploying your app (agent quickstart)

This is the **idp** platform: you describe your app in ONE `deploy.yaml` and the platform
builds the image, wires secrets/DBs/routing, and ships it via GitOps (Flux). **You never
write Kubernetes manifests.** This file is the fast path; deeper docs are linked at the end.

## The model in one breath

`deploy.yaml` (a declaration of what your app needs) → `idpctl render` (expands it into the
cluster's umbrella) → git commit on the platform's env branch → **Flux** reconciles it onto
the cluster. Public apps get a Cloudflare Tunnel; DB/cache/secret URLs are injected as env.

## Minimal deploy.yaml

```yaml
app: myapp                       # unique app name (lowercase)
runtime:
  image: ghcr.io/jakenesler/myapp   # repo ONLY — NO tag (CI/--image supplies it)
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
| `app` / `component` | app name; `component` (api/ui/worker) → workload `<app>-<component>`. |
| `runtime.image` | `ghcr.io/<owner>/<name>` **tagless** — the tag comes from `idpctl render --image <tag>`. |
| `runtime.port` | the container port your server binds. |
| `routes[]` | `{host, public, access}`. `host` is a bare label composed under the env zone, or a full FQDN. `public: true` → Cloudflare Tunnel (prod) / LAN LB (dev). |
| `db` / `cache` | `[{name, type: postgres\|redis}]` → injects `DATABASE_URL` / `REDIS_URL` (managed via SSM in prod, in-cluster in dev). |
| `secrets[]` | list of KEY names → synced from SSM `/apps/<app>/<env>/<KEY>` into your env. Shared values live at `/shared/<group>/<KEY>`. |
| `env{}` | plain non-secret config (committed). |
| `probes` | `{path: /health}` (HTTP) or `{type: tcp}` (no HTTP health route) or `{type: none}`. A wrong path CrashLoops the pod. |
| `sizing` | `{profile: minimal\|small\|…, replicas: N, autoscale: {enabled}}`. |
| `logging.enabled` | ship structured logs to Loki (`LOKI_URL` is injected). |
| `metrics.enabled` | ServiceMonitor — **must be false in prod** (no Prometheus there yet). |
| `build` | `{context, dockerfile, submodules}` for building your image from a subdir/monorepo. |
| `components[]` | multi-part product (api + ui + worker + …) in ONE file — see below. |

## Images — built for you

Your `runtime.image` is **tagless**. The platform builds + tags it:
- **CI / shipper:** push to your app's default branch → the in-cluster image-builder clones,
  builds your `Dockerfile`, pushes `ghcr.io/<owner>/<app>:<sha>`, renders, commits → Flux.
- **Manual:** `idpctl build --repo <org>/<repo> --ref <sha> --build-context <dir> --image ghcr.io/<owner>/<app>:<sha>`
- **prod rejects `:latest`** — always a commit-SHA / version tag.

## Secrets & stores

- Declare secret KEY names in `secrets:`; seed the values in **SSM** at `/apps/<app>/<env>/<KEY>`
  (the platform syncs them via external-secrets). Shared creds: `/shared/<group>/*`.
- `db: postgres` → `DATABASE_URL` (+ `PRIMARY_DATABASE_URL`); `cache: redis` → `REDIS_URL`.
  In prod these come from SSM (a shared CNPG Postgres + a shared Redis); in dev they're
  spun up in-cluster automatically. **Don't hardcode connection strings — declare the store.**

## Multi-component apps (api + ui + worker …)

Top level = the shared BASE; each `components[]` entry carries only its deltas:

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
app share ONE image TAG** (`--image <tag>` → `<each component's repo>:<tag>`).

⚠️ **Path-based routing** (one domain, different paths → different components) is NOT a
deploy.yaml feature — idp routes are host→one-service. If you need it, add a small nginx
`router` component (see `examples/cartogopher` / `examples/openprophet`).

## Deploy workflow

```bash
idpctl validate -f deploy.yaml
idpctl render --env <dev|prod> -f deploy.yaml --image <tag>   # writes clusters/<env>/platform.yaml
git commit ... && push                                        # Flux reconciles
# public app, one-time: idpctl tunnel up --env <env> -f deploy.yaml --zone-id <cf-zone>
```
Or just let the **idp-shipper** CD do it: push to your app repo's default branch.

## Gotchas (read before your first deploy)

- `db: primary` injects `DATABASE_URL`+`PRIMARY_DATABASE_URL` env but does NOT auto-add them
  to the secret — **list both in `secrets:`** and seed them in SSM.
- `metrics.enabled` and any `monitoring.coreos.com` resource must be **off in prod**.
- A tunnel'd (`public: true`) app can't `scaleToZero` (the cloudflared sidecar dies) — keep
  `autoscale.enabled: false` (or fixed replicas) until a keda-http design lands.
- Wrong `probes.path` → CrashLoop. Use `type: tcp` if you have no HTTP health route.

## Go deeper

`docs/IDP.md` (architecture) · `docs/ENV.md` + `docs/SECRETS.md` (env/secrets contract) ·
`docs/PLATFORM_DX.md` (DX) · `docs/POC_CARSHOWDB.md` (a worked example) ·
`agents/PROD_ROLLOUT.md` (prod cutover playbook + landmines) · `examples/` (real deploy.yamls).
