# Environments & promotion: dev → stage → prod

The pipeline design for the three-environment platform (decided 2026-06-12).
Status: **design + stage scaffold** — see "What's left to build" for the gaps.

## Topology

```
HOMELAB CLUSTER (k3s/kine)                      LINODE PROD (jaK3s: 3× 2GB,
├─ FluxInstance "flux" ← idp@main               embedded etcd, kube-vip, Cilium)
│   └─ clusters/dev   → umbrella env=dev        ├─ FluxInstance "flux" ← idp@prod
│   └─ clusters/stage → umbrella env=stage      │   └─ clusters/prod → umbrella env=prod
├─ apps: <app>-dev-*   namespaces               ├─ apps: <app>-prod-* namespaces
├─ apps: <app>-stage-* namespaces               ├─ exposure: Cloudflare Tunnels ONLY
└─ shared infra modules (dev umbrella owns)     ├─ DB: Linode managed PG (CNPG later)
                                                ├─ >3 nodes: yscale bursts (jaK3s image)
                                                └─ DR: etcd→R2 6h (baked in image)
```

- **One cluster, one FluxInstance.** Stage does NOT get its own Flux — the
  homelab FluxInstance gains a second Flux Kustomization applying
  `clusters/stage` from the same `main` source.
- **Prod isolation is the `prod` branch.** The prod cluster syncs
  `refs/heads/prod`; prod only moves when promotion fast-forwards it.

## Why one FluxInstance per cluster (the load-bearing decision)

**Environments are content; the reconciler is plumbing.** An environment is
data — a `cluster.yaml` + a generated umbrella. The thing that applies it is
infrastructure. Conflating them (one Flux per env) means every new env ships a
control plane; keeping them separate means adding an env to a cluster is **one
Flux Kustomization object** — additive, inspectable, removable.

Why not two Flux instances on the homelab (dev + stage):
1. **Flux controllers are cluster-singletons in practice.** Two
   kustomize-controllers mean two SSA field managers racing over shared CRDs,
   namespaces, and cluster-scoped objects. Every ugly incident this platform
   has had (KEDA vs Flux replicas ownership — twice) lived exactly at
   "two controllers co-own a field." Don't multiply that surface.
2. **One pane of glass per cluster.** `kubectl get kustomizations -n
   flux-system` shows every env on the box with revision + health. Drift
   debugging has one address.
3. **Isolation doesn't need duplication.** Each env's umbrella is its own
   HelmRelease reconciled atomically: a broken stage render fails the *stage*
   Kustomization and dev keeps reconciling. Namespaces + umbrella boundaries
   provide the blast-radius wall; a second Flux adds ~500MB of controllers and
   zero additional isolation.
4. **One upgrade surface.** One Flux version per cluster to patch and test.

Why not ONE global Flux (hub on homelab pushing to prod via kubeconfig):
1. **Prod must self-heal with zero dependency on the homelab.** We literally
   power homelab hosts off at night. Pull-based Flux on the prod cluster
   reconciles from GitHub alone; the only coupling to dev is a git branch.
2. **No cross-cluster admin kubeconfig at rest on the dev cluster** — the
   hub model stores prod's keys on the least-trusted box. Pull-per-cluster is
   the fail-closed posture the fork model promises users.
3. **Symmetry for adopters:** every cluster is the same shape — bootstrap
   flux-operator, apply `clusters/<name>/flux-instance.yaml`, done. One mental
   model, N clusters.

## idp stays generic: "prod" is declared behavior, not a magic name

For the fork-and-self-host story, environments must be pure data. A user
should be able to run `qa`, `eu-prod`, `customer-staging` — any set of envs on
any clusters — and get identical machinery. What makes prod *prod* is what its
cluster.yaml DECLARES, and promotion honors the declarations:

- `flux.branch: prod` → branch isolation (promote FFs instead of riding main)
- `allowMutableTags: false` → digest-only renders enforced at promote time
- secrets backend, bounds, zones → already per-env data

**Productization TODO:** `internal/clusterenv` still hardcodes
`if c.Env == "prod"` for policy strictness. Replace with explicit fields
(e.g. `policy.production: true`, `promotion.requiresSourceEnv: stage`) so the
behavior follows the declaration, not the string. Until then, "prod" is a
reserved name — document it as such.

## Environment contract

| | dev | stage | prod |
|---|---|---|---|
| cluster | homelab | homelab | Linode jaK3s |
| branch | main | main | **prod** |
| namespaces | `<app>-dev-*` | `<app>-stage-*` | `<app>-prod-*` |
| deploys | **auto** (shipper on app push) | `idpctl promote <app> stage` | `idpctl promote <app> prod` |
| image tags | mutable OK | immutable only | immutable only |
| secrets | local store | local store | SSM (`platform-ssm`) |
| modules | full shared set | **none** (dev umbrella owns the cluster) | keda + ESO (+ yscale) |
| data stores | per-app dev pg/redis | per-app dev pg/redis (stage copies) | managed PG via SSM `DATABASE_URL` |
| exposure | **LAN only** (`expose.lan` → MetalLB VIP, by IP) | **LAN only** (MetalLB VIP, by IP) | Cloudflare Tunnel (`routes: public`) |

**NOT YET (deliberate):** dev and stage have **no Cloudflare Tunnel** — they're
LAN-only, so both declare `seams.publicRoutes: false`. A `deploy.yaml` route with
`public: true` is rejected there (it would get no real exposure); apps reach the
LAN via `expose.lan`. Public internet routing is a **prod-only** capability today.
Stage therefore can't rehearse public-route behaviour until a tunnel is added to
the homelab — flip the seam + provide the tunnel when that's wanted.

## Promotion mechanics (digest-forward)

The artifact never rebuilds after dev. Promotion re-renders the SAME image
digest with the target env's policy/values:

1. **dev (continuous):** push to app repo → shipper builds `ghcr.io/...@sha256:D`
   → `idpctl render --env dev` → commit to main → homelab Flux applies.
2. **stage (deliberate):** `idpctl promote <app> stage` reads the digest
   currently rendered in the dev umbrella, re-renders the app into the stage
   umbrella (`clusters/stage/platform.yaml`) pinned to `@sha256:D`, commits to
   main. Same cluster picks it up minutes later in `<app>-stage-*`.
3. **prod (deliberate):** `idpctl promote <app> prod` renders **directly onto
   the `prod` branch** as a single commit (pinned to the digest currently in
   stage; refuses otherwise). No main→prod fast-forward: an FF would carry
   *everything* pending under the prod paths on main — an app promote could
   silently ship an unrelated infra change. The prod branch is an
   **append-only ledger of deliberate changes**, one unit per commit.
4. **Rollback:** `git revert` that one commit on the prod branch. Renders are
   pure functions of (deploy.yaml, cluster.yaml, digest) — reverting is safe.

## Coupling: prod cluster vs prod deployments

Couple their **location** (one repo, one FluxInstance — everything inside
idp), decouple their **change streams**:

- **Cluster existence isn't Flux's at all.** Nodes, the jaK3s image, Linode
  instances, etcd — provisioned/rebuilt out-of-band from the golden image +
  etcd snapshots. Flux only manages what runs *on* the cluster. A full
  cluster rebuild (Gate 0 restore drill) never touches deployment state, and
  vice versa.
- **Two Flux Kustomizations inside the one sync:** `infra` (modules: keda,
  ESO, yscale) and `apps` (the app umbrella). Separate health, separate
  failure domains — a broken app render can't block an infra security bump,
  and an infra upgrade gone wrong doesn't freeze app rollbacks.
- **Two promote verbs, same ledger:** `idpctl promote <app> prod` and
  `idpctl infra promote prod` each write one commit to the prod branch.
  Deployments and cluster-config changes never share a commit, so they
  never share a rollback.

## Status: full dev→stage→prod chain PROVEN end-to-end 2026-06-12

1. ~~Umbrella naming~~ **DONE** — `platform-<env>` (dev keeps the bare legacy
   name); `render.platformReleaseName`.
2. ~~`clusters/stage/`~~ **DONE + LIVE on homelab** — `platform-stage` umbrella
   (0 modules) + `clusters/dev/sync-stage.yaml` (a Flux Kustomization the dev
   sync applies → reconciles `clusters/stage`). Second env, same FluxInstance,
   one object.
3. ~~`idpctl promote`~~ **BUILT + PROVEN both hops** (cmd/idpctl/promote_cmd.go):
   `idpctl promote <workload> <env> --from <env> -f deploy.yaml`. Provenance
   from the source umbrella (refuses unknown workloads + mutable tags), gate
   via `promotion: {from: <env>}` (`--force` overrides), prints the flux.branch
   commit step (CI-agnostic). Demonstrated: dev→stage (homelab), prod gate
   `from: stage` REJECTS dev / accepts stage, stage→prod (prod-local). TODO:
   deploy.yaml-at-built-SHA auto-fetch.
4. **Shipper stays dev-only** (`registry.env: dev`) — promotion is human-driven
   by design.
5. **Prod bring-up order** (rehearsed on prod-local): jaK3s cluster →
   flux-operator + `clusters/prod/flux-instance.yaml` (branch prod) →
   `platform-ssm` store → promote apps one by one, flip the CF Tunnel route at
   cutover (rollback = flip back).

### Cross-branch promote — SOLVED (`--source-root`)

dev+stage umbrellas live on `main`; the prod umbrella lives on the `prod`
branch. `idpctl promote ... prod --from stage --source-root <main-checkout>`
now READS the source env from the main checkout while WRITING the target into
the (prod-branch) `--root`. No more hand `git checkout` of source dirs. (The
alternative — git-cat-file the source umbrella from its `flux.branch` — is left
as a future no-checkout convenience.) prod-local's Flux only needs
`clusters/prod`; `environments/prod/cluster.yaml` (gate + seams) is a
RENDER-time input, never applied — so the prod-branch promote commit stays
clean (just `clusters/prod/platform.yaml`).

### The seam contract (built)

Loose coupling is now enforced as data, not convention: each env's
`cluster.yaml` declares a `seams:` block (statefulStores / lanExpose /
publicRoutes / autoscale / volumes; omitted → derived). `clusterenv.Validate`
rejects an env that claims a seam it doesn't back; `policy.checkSeams` rejects
an app requesting a seam the env doesn't provide (so prod is PVC-free *by
contract*); `idpctl doctor` probes the live cluster for them. See the README
"seam contract" section.

## Reliability (prod)

- **Cluster state:** k3s etcd snapshots every 6h, local + R2 (`ETCD_S3_*`
  baked into the jaK3s node env). Restore: `k3s server --cluster-reset
  --cluster-reset-restore-path=...` (JaK3s README runbook).
- **App data:** Linode managed PG (daily + PITR) until the CNPG-with-WAL→R2
  phase; R2 fs-tar CronJobs for any stray PVCs (same pattern as homelab DR).
- **Everything else is git:** umbrellas + env definitions here; images in
  ghcr; nodes are cattle from the golden image.
