# platformctl

> A tiny, opinionated **Internal Developer Platform** for self-hosted Kubernetes (k3s).
> A developer writes one small `deploy.yaml`; the platform renders desired state; **Argo CD**
> reconciles it into the cluster. No hand-written Kubernetes YAML, no per-app load balancers.

**Status:** early / work-in-progress. Built and validated against a homelab k3s cluster (`dev`)
before it targets cloud k3s (`prod`). Repo is being kept private until it's ready to open-source.

---

## What it is

`platformctl` turns a small app contract into reconciled Kubernetes state:

```
 deploy.yaml  ──►  platformctl render  ──►  environments/<env>/apps/<app>.yaml   ──►  Argo CD  ──►  cluster
 (the dev's          (CLI: validate /        (an Argo CD Application + the              (the only
  shopping list)      plan / render)           shared app-chart's values)                writer)
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

## How you deploy (with Argo CD)

**Deploying is a git commit, never `kubectl apply`/`helm upgrade`. Argo CD is the only writer.**

- **Operator, once:** apply one root *app-of-apps* (`argocd/platform-<env>-root`) pointed at
  `environments/<env>/`. It fans out into the enabled infra modules and every app under `apps/`.
- **Developer, every day:** push your app repo → CI builds & pushes the image → runs
  `platformctl render --env <env> --file deploy.yaml --image <tag>` → commits the rendered
  `environments/<env>/apps/<app>.yaml` into this platform repo → Argo CD reconciles it. A new app
  is just **one new rendered file** the root app auto-discovers.

## The module system

Every piece of cluster technology is a **toggleable module** (a Helm release). Enable or skip any of
them per environment in `environments/<env>/cluster.yaml`:

```yaml
modules:
  keda:         { enabled: true }      # workload autoscaling
  dev-postgres: { enabled: true }      # self-hosted Postgres for dev
  redis:        { enabled: false }
  external-secrets: { enabled: false }
  monitoring:   { enabled: false }     # wraps kube-prometheus-stack
  yscale:       { enabled: false }     # bursts ephemeral cloud nodes (prod, later)
  unifi-controller: { enabled: false } # homelab service — modules aren't just "platform" tech
```

`platformctl infra render --env <env>` emits an Argo CD Application per **enabled** module.

## Layout

| Path | What |
|---|---|
| `cmd/platformctl`, `internal/*` | the Go CLI (validate / plan / render / infra / new) |
| `charts/app` | the shared app chart (Deployment + ClusterIP Service + optional KEDA / ServiceMonitor / ExternalSecret / PDB) |
| `charts/infra`, `modules/` | infra module charts + the module registry |
| `environments/{dev,prod}` | per-env `cluster.yaml` (modules) + rendered `apps/` desired state |
| `argocd/` | the root app-of-apps Applications |
| `examples/carshowdb` | the first onboarded app |
| `schemas/`, `test/` | the `deploy.yaml` JSON schema + unit/golden/e2e tests |

## Quickstart (dev)

```bash
go build ./... && go test ./...
./platformctl validate --file examples/carshowdb/deploy.yaml
./platformctl render   --env dev --file examples/carshowdb/deploy.yaml --image ghcr.io/jakenesler/carshowdb-api:dev-<sha>
./platformctl infra render --env dev
make e2e          # spins an ephemeral k3d cluster, applies, asserts rollout, tears down
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
