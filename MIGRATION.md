# Migration plan: 2 clusters → 1 production cluster + an Internal Developer Platform

**Status: PLAN / RESEARCH ONLY — nothing executed.**
Goal: collapse the two LKE clusters into **one production cluster**, standardize deploys on the
patterns AnyRent already uses well, expose services through **Cloudflare Tunnel** (kills the
NodeBalancer bill), and self-host Postgres with cheap R2 backups. Target bill: **$235 → ~$95–115/mo**.

> Evidence for every claim below is in this folder (`README.md`, the `raw/` dumps) and was gathered
> read-only from the live clusters + your repos. Cost figures use the **actual June invoice**, not
> the stale `Tofu/variables.tf` comment (which says managed PG is "$15" — it's really **$32/mo**).

---

## 1. What you already have (the migration builds on this, doesn't reinvent it)

Your stack already contains every ingredient of a good IDP — they're just applied inconsistently
and split across two clusters.

| Capability | Today | Source |
|---|---|---|
| Image builds | Multi-stage Dockerfiles → **Docker Hub `stackmaster/*`**, tag `prod-<sha>` + `production` | AnyRent{API,UI}/.github/workflows/CI-CD-Go-Production.yml |
| CI/CD runner | **Self-hosted GH Actions runner** in-cluster (`stackmaster/anyrent-k8s-runner`, 3 ephemeral replicas, label `[self-hosted,k8s,optimized]`) | k8s-actions-runner/anyrent-simple-runners.yaml |
| Secrets | **AWS SSM Parameter Store** `/anyrent/prod/*` → synced to k8s secrets | scripts/push-secrets-to-ssm.sh, sync-secrets-to-k8s.sh |
| TLS | cert-manager + Let's Encrypt (anyrentcloud.com etc.) | AnyRentUI/.github/certificate.yml |
| **Cloudflare Tunnel** | **carshowdb already exposes its API via a `cloudflared` sidecar — no NodeBalancer** | carshowdb deployment (live, prod cluster) |
| Object storage | **Cloudflare R2** for file uploads, Loki logs, AND Postgres backups | r2_service.go, loki-r2-values.yaml |
| Postgres backups | **Daily `pg_dump` → R2** (`anyrentcloudprod` bucket), 14-day retention | k8s/backup/postgres-backup-cronjob.yaml |
| Conn pooling | pgbouncer (2 replicas, transaction mode) | k8s/pgbouncer.yaml |
| Namespaces | `namespaces.yaml` defines prod/staging/arc/arc-infra/github/keda/cert-manager **with ResourceQuotas + LimitRanges** | AnyRentInfrastructure/k8s/namespaces/namespaces.yaml |
| Autoscaling | KEDA installed (used only for documenso today) | anyrent cluster `keda` ns |
| Observability | Prometheus + Grafana + Loki(→R2) | anyrent cluster `monitoring` ns |

**Gaps to fix in the IDP:** replicas hardcoded to `1` everywhere (no HA), no PodDisruptionBudgets,
no HPA on the main services, NodeBalancer-per-service instead of Tunnel, two clusters, managed
Postgres at $32/mo when you already have the backup machinery to self-host.

---

## 2. The big lever you weren't fully using: Cloudflare Tunnel

`carshowdb` already runs a `cloudflared` container that publishes its API through a Cloudflare
Tunnel — **zero NodeBalancers, zero public IPs.** Cloudflare Tunnel is **free with no bandwidth
limits** (confirmed: only Zero-Trust *Access* policies cost money, and that's free up to 50 users).

**If every public service routes through Cloudflare Tunnel instead of a `type=LoadBalancer`
Service, the entire $70/mo NodeBalancer line goes to ~$0** — and you gain Cloudflare's edge TLS,
caching, and DDoS protection for free.

**IDP pattern (cleaner than carshowdb's per-pod sidecar):** run **one `cloudflared` Deployment per
cluster** (2 replicas for HA) whose tunnel config maps hostnames → internal ClusterIP services:
```
anyrentcloud.com      → anyrent-ui.arc.svc:3000
api.anyrentcloud.com  → anyrent-api.arc.svc:8080
cartogopher.com       → cartogopher-landing.cartogopher.svc:80
openprophet.io        → openprophet-api.openprophet.svc:3738
carshowdb.<domain>    → carshowdb-api.carshowdb.svc:8080
...
```
Adding a new app = add one route + a DNS CNAME (both automatable via the Cloudflare API). No new
infra, no NodeBalancer. This *replaces* the hand-rolled `nginx:alpine` proxy on prod_cluster.

> Keep cert-manager around for any internal/mTLS needs, but public TLS terminates at Cloudflare.

---

## 3. Target architecture — ONE production cluster

> **UPDATED DIRECTION → see `ARCHITECTURE.md`.** Instead of "one fat LKE cluster right-sized to ~4
> nodes," the chosen shape is a **minimal always-on k3s host on Linode + the yscale dynamically-
> scaling mesh (KEDA-driven, multi-DC, scale-to-zero) + Cloudflare Tunnel.** Lower floor (~$12–24
> baseline vs ~$48), elastic peak/job capacity that only costs money while serving load, and it runs
> on your own stack. Target drops to **~$50–70/mo**. The LKE option below is kept as the
> lower-ops fallback. Everything else in this doc (Tunnel ingress, self-hosted Postgres, IDP chart)
> is unchanged.

**Fallback (managed-ops) option — keep:** the **anyrent cluster (550390, us-ord)** as the single prod cluster. It's the newer one
and already has the best foundation: KEDA, Prometheus/Grafana/Loki, cert-manager, the namespace
convention. **Decommission `prod_cluster` (222618, us-east)** after migrating its apps in.

```
                         Cloudflare (DNS + edge TLS + free Tunnel)
                                        │
                          ┌─────────────┴──────────────┐
                          │   cloudflared (2 replicas)  │   ← one tunnel, all hostnames, $0
                          └─────────────┬──────────────┘
   ┌──────────┬──────────┬─────────────┼───────────┬───────────┬───────────┐
  arc       cartogopher openprophet  ecommerce  unbelievable dummy-api  carshowdb   ← namespace per app
 (api+ui)                                                                            (each: ClusterIP only,
                                                                                      2+ replicas, PDB, HPA)
                          ┌──────────────────────────────┐
                          │ platform ns: postgres (STS,   │  ← self-hosted PG primary+replica,
                          │  primary+replica), pgbouncer, │     pgbouncer Service, pg_dump→R2
                          │  cloudflared, ingress (opt)   │
                          └──────────────────────────────┘
   Node pool: ~4× g6-standard-1 (or 3× g6-standard-2) right-sized to fit everything
```

### Node sizing
Today: 7× 2GB nodes across two clusters = **$84/mo**. Consolidated onto one cluster, the same
workloads fit in **~4× 2GB ($48) or 3× 4GB ($72)** nodes. Start at 4× 2GB, watch utilization,
right-size. (Fewer, larger nodes reduce per-node system-pod overhead.)

---

## 4. The IDP, concretely

### 4.1 Standard app contract — one Helm chart, per-app `values.yaml`
Replace the per-repo hand-written `deployment.yml`/`service-production.yml` with a shared
`platform/app-chart` that renders Deployment + **ClusterIP** Service + Cloudflare Tunnel route +
HPA/KEDA + PodDisruptionBudget + ServiceMonitor + resource requests/limits. The chart **forbids
`type: LoadBalancer`** so nobody can accidentally spawn a NodeBalancer again.
```yaml
name: cartogopher
namespace: cartogopher
image: stackmaster/cartogopher-api:prod-<sha>   # your existing build output
port: 3001
hostnames: [cartogopher.com, www.cartogopher.com]   # → cloudflared route + DNS
replicas: 2                                          # HA by default (fixes the replicas=1 gap)
autoscale: { enabled: true, min: 2, max: 5 }         # KEDA/HPA
pdb: { minAvailable: 1 }
secrets: anyrent-style SSM sync                       # reuse sync-secrets-to-k8s.sh
monitor: true                                         # ServiceMonitor for Prometheus
```

### 4.2 Image builds & CI/CD — keep AnyRent's model, make it reusable
- Keep **Docker Hub `stackmaster/<app>`**, `prod-<sha>` tagging, multi-stage Dockerfiles, BuildKit.
- Keep the **self-hosted k8s runner** (`[self-hosted,k8s,optimized]`).
- Turn the AnyRent `CI-CD-Go-Production.yml` into **one reusable GitHub Actions workflow** that any
  repo calls with its `values.yaml`: test → build/push → `helm upgrade --install <app> platform/app-chart`.
- Standardize the manual `workflow_dispatch` "type PRODUCTION" guard across all apps.

### 4.3 Namespaces — extend the existing convention
One namespace per app (`arc`, `cartogopher`, `openprophet`, `ecommerce`, `unbelievable`,
`dummy-api`, `carshowdb`) + `platform` (postgres, pgbouncer, cloudflared) + `monitoring` +
`cert-manager` + `keda` + `github`. Attach ResourceQuota + LimitRange per app ns (the
`namespaces.yaml` already shows the pattern).

### 4.4 Secrets
Keep **SSM → k8s** (`sync-secrets-to-k8s.sh`). Recommended upgrade later: **external-secrets
operator** so secrets sync continuously from SSM instead of one-shot at deploy, and aren't stored
plaintext-base64 in etcd.

### 4.5 Self-hosted Postgres (replaces the $32/mo managed DBaaS)
You already do `pg_dump → R2` daily, run pgbouncer, and have a dev Postgres StatefulSet pattern.
Bring it to prod **on the cluster**:
- **StatefulSet**: 1 primary + 1 read replica (streaming replication) → satisfies your "multiple
  pods each" ask and gives basic HA.
- **Storage**: a **Linode Block Storage PVC** (e.g. 40Gi = **$4/mo**) — *not* local-path, so it
  survives node failure. Set `max_slot_wal_keep_size` to cap WAL (the cause of both April disk
  incidents — self-inflicted orphaned replication slots, not infra failures).
- **Service exposure (your explicit ask):** pgbouncer fronts it as a **ClusterIP Service**
  (`pgbouncer.platform.svc:5432`); add a **NodePort** (or Tailscale) for admin/external access
  without a public LB.
- **Backups — must MATCH managed (daily backups, 14-day retention, PITR):** a nightly `pg_dump`
  alone does NOT match managed (24h RPO, no point-in-time recovery). Use **CloudNativePG**, which
  does continuous WAL archiving + scheduled base backups + 14-day retention + PITR to R2, plus
  automatic primary/replica failover. **Keep the existing `pg_dump → R2` cronjob as an extra
  logical safety net.** Full parity recipe + manifests in **`POSTGRES_BACKUPS.md`**.
- **Cost:** ~$4 block storage + <$1/mo R2 (no egress fees) vs **$32/mo managed** → **save ~$28/mo**,
  with equal-or-better recovery (a replica means no maintenance downtime, unlike managed single-node).
- **Tradeoff (be honest):** you now own upgrades, failover, disk monitoring, and **restore drills**
  (add a monthly automated restore-and-verify job — managed hides this from you). The April
  incidents showed your ops maturity is already there; the fix (cap WAL via `max_slot_wal_keep_size`)
  is baked into the config.

---

## 5. Migration sequence (no action until you approve each phase)

**Phase 0 — IDP scaffolding (no prod impact).** Author `platform/app-chart`, the reusable CI
workflow, and the shared `cloudflared` Deployment manifest. Stand up the Cloudflare Tunnel + routes
in a staging hostname. Validate end-to-end with one low-risk app.

**Phase 1 — Cloudflare Tunnel cutover on the EXISTING clusters (fast NodeBalancer savings).**
For each app, add a Tunnel route → flip its Service to ClusterIP → move DNS to the Tunnel CNAME →
delete its NodeBalancer. Order by risk: metabase (dead, already), then prod side-projects, then
anyrent api/ui. **This alone reclaims ~$70/mo and is independent of the cluster move.**

**Phase 2 — Self-host Postgres on the anyrent cluster.** Stand up the StatefulSet + replica,
replicate from managed DB, verify, cut pgbouncer over in a low-traffic window, then stop the
managed DB. **−$28/mo.**

**Phase 3 — Migrate prod_cluster apps into the anyrent cluster.** Per app: build is already
`stackmaster/*`, so just `helm install` into a new namespace on anyrent, sync secrets from SSM,
add Tunnel route, verify, move DNS. Apps: cartogopher (+ its nginx routes become Tunnel routes),
openprophet, ecommerce, unbelievable, dummy-api, carshowdb. Migrate the in-cluster `postgres`/
`redis` from the cartogopher namespace too.

**Phase 4 — Decommission prod_cluster (222618).** Once nothing serves from it and DNS is fully on
the anyrent cluster's tunnel: delete the cluster (removes 3 nodes + its NodeBalancers + control
plane). Right-size the surviving cluster's node pool.

**Phase 5 — Hardening.** HPA/KEDA + PDB on every app, external-secrets, ServiceMonitors, a
Released-PV janitor (so volumes stop leaking), burst-reaper + Linode billing alert for the GPU
workers.

---

## 6. Side quest: fix carshowdb (independent of the migration)
`carshowdb-api` is crash-looping (750+ restarts) because its `DATABASE_URL` points at a **Supabase**
pooler (`aws-1-us-east-2.pooler.supabase.com:6543`) and the tenant/user
`postgres.kabsznvjxtqqellykxfd` no longer exists — "FATAL: tenant/user not found". Its `cloudflared`
and `tailscale` sidecars are healthy; only the app can't reach its DB. Fix = update the Supabase
credentials in `carshowdb-api-secrets`, **or** repoint it at the new self-hosted Postgres as part
of Phase 3. (Until then, scale it to 0 to stop the restart churn.)

---

## 7. Cost outcome

| Line | Today | After migration | Saving |
|---|---:|---:|---:|
| LKE worker nodes | $84 (7 nodes, 2 clusters) | ~$48 (4 nodes, 1 cluster) | $36 |
| NodeBalancers | $70 (7) | ~$0 (Cloudflare Tunnel) | $70 |
| Managed Postgres | $32 | ~$4 (self-hosted + R2 backups) | $28 |
| Orphaned volumes | $6 | $0 | $6 |
| GPU/ephemeral burst | ~$14 | ~$14 (+ guardrails) | — |
| Object storage | $5 | $5 | — |
| Misc/tax | ~$24 | ~$15 | ~$9 |
| **Total** | **~$235** | **~$95–115** | **~$120–140/mo** |

Cloudflare Tunnel adds a small new dependency (traffic via Cloudflare) but **$0** cost. The durable
win is the IDP: every future app is a `values.yaml` that deploys to one cluster, exposes via Tunnel
($0), gets HA + TLS + dashboards automatically, and **can never add a NodeBalancer**.

## Sources
- [Cloudflare: Free Tunnels for Everyone](https://blog.cloudflare.com/tunnel-for-everyone/) · [Cloudflare plans](https://www.cloudflare.com/plans/)
- [Akamai/Linode Managed PostgreSQL](https://www.linode.com/products/databases/) (2GB tier; actual cost from your June invoice = $32/mo)
- Internal: `AgenticAnyRentWorkers/**` (build/deploy/secrets/backup), live cluster inspection (see `README.md`)
