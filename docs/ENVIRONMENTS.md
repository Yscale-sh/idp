# Environments & promotion: dev -> prod (stage deferred)

The environment and promotion design (decided 2026-06-12).
Status: **dev -> prod is the live path; stage is scaffolded but deferred.** See
"What's left to build" for the gaps.

The per-app `deploy.yaml` "shopping list" is the product. A developer declares a
small app contract (app, runtime, routes, sizing, db, cache, volumes,
connectsTo). The platform derives all of the Kubernetes from it. This doc covers
where that contract runs (the environments) and how a workload moves between
them (promotion). The developer never writes a Deployment, Service, or
LoadBalancer; they declare intent in one file per env-aware contract.

## Topology

```
HOMELAB CLUSTER (k3s/kine)                      LINODE PROD (jaK3s: 3× 2GB,
├─ FluxInstance "flux" ← idp@main               embedded etcd, kube-vip, Cilium)
│   └─ clusters/dev   → umbrella env=dev        ├─ FluxInstance "flux" ← idp@prod
│   └─ clusters/stage → umbrella env=stage      │   └─ clusters/prod → umbrella env=prod
│      (scaffolded, deferred)                   ├─ apps: <app>-prod-* namespaces
├─ apps: <app>-dev-*   namespaces               ├─ exposure: Cloudflare Tunnels ONLY
├─ apps: <app>-stage-* namespaces (deferred)    ├─ DB: in-cluster CNPG (DATABASE_URL via SSM)
└─ shared infra modules (dev umbrella owns)     ├─ >3 nodes: yscale bursts (jaK3s image)
                                                └─ DR: etcd→R2 6h (baked in image)
```

- **One cluster, one FluxInstance.** Stage does NOT get its own Flux. The
  homelab FluxInstance gains a second Flux Kustomization applying
  `clusters/stage` from the same `main` source (`clusters/dev/sync-stage.yaml`).
  That second Kustomization is scaffolded but the stage promotion path is
  deferred.
- **Prod isolation is the `prod` branch.** The prod cluster syncs
  `refs/heads/prod`; prod only moves when a promote commits to that branch.

## Why one FluxInstance per cluster (the load-bearing decision)

**Environments are content; the reconciler is plumbing.** An environment is
data: a `cluster.yaml` plus a generated umbrella. The thing that applies it is
infrastructure. Conflating them (one Flux per env) means every new env ships a
control plane. Keeping them separate means adding an env to a cluster is **one
Flux Kustomization object**, additive, inspectable, removable.

Why not two Flux instances on the homelab (dev + stage):
1. **Flux controllers are cluster-singletons in practice.** Two
   kustomize-controllers mean two SSA field managers racing over shared CRDs,
   namespaces, and cluster-scoped objects. Every ugly incident this platform
   has had (KEDA vs Flux replicas ownership, twice) lived exactly at
   "two controllers co-own a field." Don't multiply that surface.
2. **One pane of glass per cluster.** `kubectl get kustomizations -n
   flux-system` shows every env on the box with revision and health. Drift
   debugging has one address.
3. **Isolation does not need duplication.** Each env's umbrella is its own
   HelmRelease reconciled atomically: a broken stage render fails the *stage*
   Kustomization and dev keeps reconciling. Namespaces plus umbrella boundaries
   provide the blast-radius wall; a second Flux adds roughly 500MB of
   controllers and zero additional isolation.
4. **One upgrade surface.** One Flux version per cluster to patch and test.

Why not ONE global Flux (hub on homelab pushing to prod via kubeconfig):
1. **Prod must self-heal with zero dependency on the homelab.** We literally
   power homelab hosts off at night. Pull-based Flux on the prod cluster
   reconciles from GitHub alone; the only coupling to dev is a git branch.
2. **No cross-cluster admin kubeconfig at rest on the dev cluster.** The
   hub model stores prod's keys on the least-trusted box. Pull-per-cluster is
   the fail-closed posture the fork model promises users.
3. **Symmetry for adopters.** Every cluster is the same shape: bootstrap
   flux-operator, apply `clusters/<name>/flux-instance.yaml`, done. One mental
   model, N clusters.

## idp stays generic: "prod" is declared behavior, not a magic name

For the fork-and-self-host story, environments must be pure data. A user
should be able to run `qa`, `eu-prod`, or `customer-staging`, any set of envs on
any clusters, and get identical machinery. What makes prod *prod* is what its
cluster.yaml DECLARES, and promotion honors the declarations:

- `flux.branch: prod` gives branch isolation (the prod cluster syncs that branch
  only).
- `allowMutableTags: false` enforces digest-only renders at promote time.
- `promotion.from: dev` declares the only legal source env for a promote.
- secrets backend, bounds, and zones are already per-env data.

**Productization TODO:** `internal/clusterenv` still hardcodes
`if c.Env == "prod"` for some strictness (the SSM backend default, for example).
Replace it with explicit fields so the behavior follows the declaration, not the
string. Until then, "prod" is a reserved name; document it as such.

## Environment contract

| | dev | stage (deferred) | prod |
|---|---|---|---|
| cluster | homelab | homelab | Linode jaK3s |
| branch | main | main | **prod** |
| namespaces | `<app>-dev-*` | `<app>-stage-*` | `<app>-prod-*` |
| deploys | **auto** (idp-shipper on app push) | scaffolded, not in the promote path | **auto** (prod shipper, `prod` branch) + deliberate `idpctl promote <app> prod --from dev` |
| image tags | mutable OK | immutable only | immutable only |
| secrets | local store | local store | SSM (`platform-ssm`) |
| modules | full shared set | **none** (dev umbrella owns the cluster) | keda + ESO + CNPG (+ yscale, deferred) |
| data stores | per-app dev pg/redis | per-app dev pg/redis (when active) | in-cluster CNPG via SSM `DATABASE_URL` |
| exposure | **LAN only** (`expose.lan` → MetalLB VIP, by IP) | **LAN only** (MetalLB VIP, by IP) | Cloudflare Tunnel (`routes: public`) |
| seams | `publicRoutes: false` | `publicRoutes: false` | `statefulStores: false`, `lanExpose: false`, tunnel on |
| promotion | source for prod | `from: stage` is wired but deferred | `promotion.from: dev` |

**Stage is available but deferred.** `environments/stage`, `clusters/stage`, and
`clusters/dev/sync-stage.yaml` exist in the repo, and stage carries prod-strict
policy (`allowMutableTags: false`, prod-shaped bounds, `flux.branch: main`, all
modules disabled). It is NOT in the active promotion path today: prod promotes
directly from dev. Bring stage online by flipping `promotion.from: stage` in
`environments/prod/cluster.yaml` and promoting through it.

**NOT YET (deliberate):** dev and stage have **no Cloudflare Tunnel**; they are
LAN-only, so both declare `seams.publicRoutes: false`. A `deploy.yaml` route with
`public: true` is rejected there (it would get no real exposure); apps reach the
LAN via `expose.lan` (the only sanctioned LoadBalancer, labeled
`platform/expose: lan`). Public internet routing is a **prod-only** capability
today, served by a Cloudflare Tunnel (`cloudflared` sidecar; the app stays
ClusterIP). Stage therefore cannot rehearse public-route behavior until a tunnel
is added to the homelab: flip the seam and provide the tunnel when that is
wanted.

## Promotion mechanics (digest-forward, dev -> prod)

The artifact never rebuilds after dev. Promotion re-renders the SAME image
digest with the target env's policy and values:

1. **dev (continuous, automatic):** push to your app repo. The in-cluster
   `idp-shipper` (dev's instance, `registry.env: dev`) reads the GitHub head SHA for
   each registered app, builds only the images whose inputs changed via the
   in-cluster image-builder (rootless BuildKit -> GHCR, tag `<image>:<short-sha>`),
   re-renders each component into `clusters/dev/platform.yaml` with idpctl's
   render core, then commits and pushes to `main`. The homelab Flux reconciles.
   The developer only ever touches their `deploy.yaml`. A new or unregistered app
   deploys manually: `idpctl render --env dev --root <idp clone> -f deploy.yaml
   --image <ref>`, then commit and push `clusters/dev/platform.yaml` to main.
2. **prod (deliberate):** `idpctl promote <app> prod --from dev -f deploy.yaml`
   reads the image digest already running in the dev umbrella and re-renders the
   app into `clusters/prod/platform.yaml` with prod's policy, secrets backend,
   and namespaces. It NEVER rebuilds the artifact. The render targets the `prod`
   branch as a single commit. The command does not push: it prints the
   `flux.branch` commit step, so any CI shape can own the push. Prod refuses
   mutable tags (`allowMutableTags: false`, and `:latest` is hard-rejected in
   code regardless). Because dev and prod umbrellas live on different branches,
   read the source from a `main` checkout while writing the target into the prod
   tree: `idpctl promote <app> prod --from dev --source-root <main-checkout>
   --root <prod-checkout>`.
3. **Rollback:** `git revert` that one commit on the prod branch. Renders are
   pure functions of (deploy.yaml, cluster.yaml, digest), so reverting is safe.

Stage, when activated, is a third hop with the same mechanics: promote
`<app> stage --from dev`, then `<app> prod --from stage` once
`environments/prod/cluster.yaml` declares `promotion.from: stage`.

## Coupling: prod cluster vs prod deployments

Couple their **location** (one repo, one FluxInstance, everything inside idp),
decouple their **change streams**:

- **Cluster existence is not Flux's at all.** Nodes, the jaK3s image, Linode
  instances, and etcd are provisioned and rebuilt out-of-band from the golden
  image plus etcd snapshots. Flux only manages what runs *on* the cluster. A full
  cluster rebuild (Gate 0 restore drill) never touches deployment state, and
  vice versa.
- **Two Flux Kustomizations inside the one sync:** `infra` (modules: keda,
  ESO, CNPG, yscale when enabled) and `apps` (the app umbrella). Separate
  health, separate failure domains: a broken app render cannot block an infra
  security bump, and an infra upgrade gone wrong does not freeze app rollbacks.
- **Two promote verbs, same ledger:** `idpctl promote <app> prod --from dev`
  and `idpctl infra promote prod` each write one commit to the prod branch.
  Deployments and cluster-config changes never share a commit, so they never
  share a rollback.

## Status: dev -> prod proven; stage deferred

1. ~~Umbrella naming~~ **DONE.** `platform-<env>` (dev keeps the bare legacy
   name); `render.platformReleaseName`.
2. ~~`clusters/stage/`~~ **DONE + scaffolded on homelab.** `platform-stage`
   umbrella (0 modules) plus `clusters/dev/sync-stage.yaml` (a Flux Kustomization
   the dev sync applies, reconciling `clusters/stage`). Second env, same
   FluxInstance, one object. The stage *promotion path* is deferred: prod
   promotes directly from dev.
3. ~~`idpctl promote`~~ **BUILT + PROVEN** (cmd/idpctl/promote_cmd.go):
   `idpctl promote <workload> <env> --from <env> -f deploy.yaml`. Provenance
   from the source umbrella (refuses unknown workloads and mutable tags), gate
   via `promotion: {from: <env>}` (`--force` overrides), prints the `flux.branch`
   commit step (CI-agnostic). Live gate today: `environments/prod/cluster.yaml`
   declares `promotion.from: dev`, so prod accepts a dev digest. TODO:
   deploy.yaml-at-built-SHA auto-fetch.
4. **Shipper runs per environment.** Dev's instance (`registry.env: dev`) commits
   `main`; prod runs its own (`registry.env: prod`, `platformBranch: prod`), brought
   online app by app, committing the `prod` branch. The digest-forward `idpctl promote
   <app> prod --from dev` is the deliberate alternative that pins an exact dev digest
   with no rebuild; it and the prod shipper coexist during bring-up.
5. **Prod bring-up order** (rehearsed): jaK3s cluster -> flux-operator +
   `clusters/prod/flux-instance.yaml` (branch prod) -> `platform-ssm` store ->
   promote apps one by one, flip the CF Tunnel route at cutover (rollback = flip
   back).

### Cross-branch promote: SOLVED (`--source-root`)

The dev and stage umbrellas live on `main`; the prod umbrella lives on the
`prod` branch. `idpctl promote ... prod --from dev --source-root <main-checkout>`
READS the source env from the main checkout while WRITING the target into the
(prod-branch) `--root`. No hand `git checkout` of source dirs. (The
alternative, git-cat-file the source umbrella from its `flux.branch`, is left
as a future no-checkout convenience.) prod-local's Flux only needs
`clusters/prod`; `environments/prod/cluster.yaml` (gate plus seams) is a
RENDER-time input, never applied, so the prod-branch promote commit stays
clean (just `clusters/prod/platform.yaml`).

### The seam contract (built)

Loose coupling is enforced as data, not convention. Each env's `cluster.yaml`
declares a `seams:` block (statefulStores, lanExpose, publicRoutes, autoscale,
volumes; each a `*bool`, nil derives). `clusterenv.Validate` rejects an env that
claims a seam it does not back; `policy.checkSeams` rejects an app requesting a
seam the env does not provide (so prod stays PVC-free *by contract*);
`idpctl doctor` probes the live cluster for them. dev and stage both declare
`publicRoutes: false`. prod declares `statefulStores: false`, `lanExpose: false`;
publicRoutes and autoscale derive from zones plus the keda module. See the README
"seam contract" section.

## Reliability (prod)

- **Cluster state:** k3s etcd snapshots every 6h, local plus R2 (`ETCD_S3_*`
  baked into the jaK3s node env). Restore: `k3s server --cluster-reset
  --cluster-reset-restore-path=...` (jaK3s README runbook).
- **App data:** in-cluster CloudNativePG (one lean HA `prod-pg` cluster with a
  per-app database and login role; apps get `DATABASE_URL` from the SSM backend,
  pointed at the CNPG service). R2 fs-tar CronJobs cover any stray PVCs (same
  pattern as the homelab DR).
- **Everything else is git:** umbrellas plus env definitions live here; images
  live in GHCR; nodes are cattle from the golden image.
