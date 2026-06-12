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

## Promotion mechanics (digest-forward)

The artifact never rebuilds after dev. Promotion re-renders the SAME image
digest with the target env's policy/values:

1. **dev (continuous):** push to app repo → shipper builds `ghcr.io/...@sha256:D`
   → `idpctl render --env dev` → commit to main → homelab Flux applies.
2. **stage (deliberate):** `idpctl promote <app> stage` reads the digest
   currently rendered in the dev umbrella, re-renders the app into the stage
   umbrella (`clusters/stage/platform.yaml`) pinned to `@sha256:D`, commits to
   main. Same cluster picks it up minutes later in `<app>-stage-*`.
3. **prod (deliberate):** `idpctl promote <app> prod` re-renders into
   `clusters/prod/platform.yaml` pinned to the digest **currently in stage**
   (refuses if stage ≠ a promoted digest), commits to main, then
   fast-forwards: `git push origin main:prod`. Prod Flux applies.
4. **Rollback:** `git revert` the promote commit (+ re-FF prod). Renders are
   pure functions of (deploy.yaml, cluster.yaml, digest) — reverting is safe.

Why FF instead of cherry-pick: `clusters/prod/` + `environments/prod/` only
ever change via promote commits, so main:prod FF moves exactly those; app/dev
churn on main is invisible to the prod Flux path filter.

## What's left to build

1. **Umbrella naming** (blocker for stage): the generated umbrella HelmRelease
   is `platform` in `flux-system` for every env — dev+stage on one cluster
   collide. `internal/render` must name it `platform-<env>` (keep `platform`
   for dev as legacy alias or migrate once).
2. **`clusters/stage/`**: kustomization + the generated `platform.yaml`
   (env=stage), plus a `sync-stage.yaml` Flux Kustomization added to
   `clusters/dev/kustomization.yaml` so the homelab Flux applies it.
3. **`idpctl promote <app> <env>`** (cmd/idpctl): read source-env umbrella →
   extract app image digest → render target env pinned to it → commit (+ FF
   for prod). Guardrails: refuse mutable tags; refuse prod unless the digest
   is in stage; print a diff first.
4. **Shipper stays dev-only** (`registry.env: dev`). Promotion is human-driven
   by design (auto-dev / manual-stage+prod).
5. **Prod bring-up order:** jaK3s cluster → flux-operator +
   `clusters/prod/flux-instance.yaml` (branch prod) → `platform-ssm`
   ClusterSecretStore creds → promote apps one by one, flipping each app's
   Cloudflare Tunnel route at cutover (instant rollback = flip back).

## Reliability (prod)

- **Cluster state:** k3s etcd snapshots every 6h, local + R2 (`ETCD_S3_*`
  baked into the jaK3s node env). Restore: `k3s server --cluster-reset
  --cluster-reset-restore-path=...` (JaK3s README runbook).
- **App data:** Linode managed PG (daily + PITR) until the CNPG-with-WAL→R2
  phase; R2 fs-tar CronJobs for any stray PVCs (same pattern as homelab DR).
- **Everything else is git:** umbrellas + env definitions here; images in
  ghcr; nodes are cattle from the golden image.
