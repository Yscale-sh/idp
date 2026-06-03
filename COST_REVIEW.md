# Linode Cloud Cost Review — June 2026

**Current bill: $235.08/mo** (up from ~$91/mo in Sep 2025). Costs roughly doubled in
Feb 2026 when the second cluster (`anyrent-production`) + managed Postgres came online,
and have crept up since with GPU burst usage.

Raw API dumps are in `./raw/`. Analysis date: 2026-06-01.

---

## Spend trend

| Month | Total |
|---|---|
| 2025-09 | $91 |
| 2025-10 | $91 |
| 2025-11 | $89 |
| 2025-12 | $94 |
| 2026-01 | $108 |
| 2026-02 | **$188** ← second cluster + DBaaS added |
| 2026-03 | $188 |
| 2026-04 | $199 |
| 2026-05 | $219 |
| 2026-06 | **$235** |

## Where the $235 goes (June invoice)

| Category | $/mo | Notes |
|---|---|---|
| LKE worker nodes (7× Linode 2GB) | $84.64 | 3 on prod_cluster + 4 on anyrent-production |
| **NodeBalancers (7×)** | **$70.00** | **biggest easy win — should be 2, not 7** |
| Managed Postgres (DBaaS) | $32.00 | single node, no HA |
| GPU burst (RTX4000, ~20 short-lived) | $11.96 | variable, from agentic workers |
| Block Storage (11× 10GiB volumes) | $10.83 | **6 of 11 are detached/orphaned** |
| Standalone Nanode "GoUS" | $5.00 | from Mar 2024, untagged — purpose unknown |
| Object Storage (6 buckets) | $5.00 | flat minimum; total data <500 MB |
| Ephemeral burst compute (77 line items) | $1.90 | variable, agentic workers |
| Images / network transfer | ~$0.44 | negligible |
| (tax) | ~$13 | |

---

## Infrastructure inventory

### Two LKE clusters (different regions)
- **`prod_cluster`** (id 222618) — region **us-east**, k8s 1.35, **3 worker nodes** (~$36/mo)
- **`anyrent-production`** (id 550390) — region **us-ord**, k8s 1.35, **4 worker nodes** (~$48/mo)

### NodeBalancers — 7 total, $70/mo  ← THE PROBLEM
Each LBaaS is $10/mo. On LKE every `Service type=LoadBalancer` spins up its own NodeBalancer.

**prod_cluster (us-east) — 4 NodeBalancers = $40/mo**
| ID | Label | Listening | Backend health |
|---|---|---|---|
| 883011 | ccm-7c9a08cee4d7 | 80/tcp | 3 up — legacy ccm-style LB |
| 1012868 | ccm-a9c75bf0a44f | 80/tcp | 3 up — legacy ccm-style LB |
| 1887204 | lke222618-267c250f2899 | 80/tcp, 443/tcp | 80 up, **443 all DOWN** |
| 2136181 | lke222618-caa358152705 | 80/tcp, 443/tcp | 80 up, **443 all DOWN** |

> Four LBs on a 3-node cluster is excessive. The two `ccm-` ones use the old naming
> scheme (pre-dating LKE's `lke<id>-` convention) and only serve port 80. The 443
> backends being fully **down** on the newer two suggests TLS isn't actually terminating
> there / a half-finished migration.

**anyrent-production (us-ord) — 3 NodeBalancers = $30/mo**
| ID | Label | Listening | Backend health |
|---|---|---|---|
| 1952704 | lke550390-6acba99634cd | 80/http + 443/https | 4 up / 4 up — healthy ingress |
| 1952707 | lke550390-6af1deb6ebc7 | 80/http + 443/https | 4 up / 4 up — healthy ingress |
| 1957652 | lke550390-47bd1b18dc42 | 3000/tcp | **all 4 DOWN — dead service** |

> 1957652 (port 3000, every backend down) is almost certainly an orphaned LoadBalancer
> service for an app that no longer runs. The two healthy ones both terminate 80+443,
> i.e. two separate ingress-style services that could be merged behind one ingress.

### Managed Postgres
- `anyrent-prod-db` (id 406649) — postgresql 15.18, g6-standard-1 (2GB, 1 vCPU), **1 node**, active. $32/mo.
- Single node = no high availability, so you're paying the managed premium mainly for backups/patching.

### Block Storage volumes — 11× 10GiB = $11/mo, **6 orphaned (CONFIRMED safe)**
Attached (5): all `Bound` PVs on running nodes — keep.
**Orphaned (6) — verified against both clusters' PVs, all `Released` or no PV at all → safe to delete (~$6/mo):**
| Volume ID | Label | Cluster PV status |
|---|---|---|
| 10981975 | pvc-f29ad319465b4341 | prod: Released |
| 10981976 | pvc-3acef7e38d944a93 | prod: Released |
| 15824233 | pvc-34ef9802da034346 | prod: Released |
| 12974292 | pvc-9bf0b1bf0d374852 | anyrent: Released |
| 12929601 | pvc-3d0ccc128e354cf3 | no PV in either cluster |
| 12933597 | pvc-297febaf8bb84daa | no PV in either cluster |
> `Released` = the PVC was deleted but the underlying Linode volume wasn't reclaimed
> (Retain policy / CSI left it). These bill $1/mo each forever until manually deleted.

### Standalone instance
- **GoUS** (id 56012954) — g6-nanode-1, us-east, running since 2024-03-14, **no tags**, $5/mo. Purpose unclear.

### GPU / ephemeral burst (the "agentic workers")
- `ys-burst-*`, `ys-gpu-*`, `ys-debug-*`, `headscale-*`, `wg-test-*` instances + RTX4000 Ada GPUs.
- **Good news: none are currently running** — they're short-lived and being cleaned up.
- ~$14/mo of variable cost. Not leaking now, but it's the line most likely to spike.

### Object Storage — 6 buckets, $5/mo flat
`arc`, `cartogopher-public-assets`, `cathedral-notifications`, `ecommerce`, `freeecomapi`,
`history-of-freeland`. Total data <500 MB → you're paying the $5 minimum regardless.

---

## CONFIRMED: what is on each of the 7 NodeBalancers

Mapped via kubectl (prod_cluster kubeconfig in `openprophet-saas/k8s/`, anyrent kubeconfig
in Freelens). Every `Service type=LoadBalancer` gets its own $10/mo NodeBalancer — that's
why there are 7.

### prod_cluster (222618, us-east) — 4 NodeBalancers = $40/mo
| NB id | IP | k8s Service | Project | Ports | Health |
|---|---|---|---|---|---|
| 2136181 | 143.42.178.227 | cartogopher/cartogopher-loadbalancer | **CartoGopher** | 80,443 | 443 backends DOWN |
| 1887204 | 139.144.241.6 | dummy-api/dummy-api-service | **dummyproductapi** | 80,443 | 443 backends DOWN |
| 883011 (ccm) | 69.164.223.41 | ecommerce/ecommerce-service | **my-ecommerce** | 80 | up |
| 1012868 (ccm) | 139.144.241.31 | unbelievable/unbelievable-service | **unbelievablesite** | 80 | up |

### anyrent (550390, us-ord) — 3 NodeBalancers = $30/mo
| NB id | IP | k8s Service | What | Ports | Health |
|---|---|---|---|---|---|
| 1952707 | 172.238.178.41 | arc/anyrent-api-service | AnyRent API | 80,443 | up |
| 1952704 | 172.236.105.253 | arc/anyrent-ui-service | AnyRent UI | 80,443 | up |
| 1957652 | 172.232.18.25 | arc/metabase | **Metabase BI tool** | 3000 | **ALL DOWN** |

> anyrent already has cert-manager running (ACME HTTP-01 solver ingresses exist for
> anyrentcloud.com / .io / sign. / esign. / www.), so an ingress path is half-built.

### Clusters / contexts
- `prod_cluster` (222618) — Linode us-east. kubeconfig: `openprophet-saas/k8s/prod_cluster-kubeconfig.yaml`, `~/.kube/prod_cluster-kubeconfig.yaml`
- `anyrent` (550390) — Linode us-ord. kubeconfig: Freelens `88d8e9ee-0dc0-4da5-b54d-b6cbb9889989` (ctx `lke550390-ctx`)
- `default` — **local k3s** at 10.0.0.101:6443 (LAN, NOT billed by Linode). Ignore for cost.
- Stale Freelens entries `lke550124`, `lke550171` — those clusters no longer exist.

### Object Storage buckets ↔ projects
`cartogopher-public-assets` (CartoGopher), `ecommerce` (my-ecommerce), `freeecomapi`
(dummyproductapi), `arc` (anyrent), `cathedral-notifications` + `history-of-freeland`
(misc/legacy). All tiny; flat $5/mo minimum regardless.

> bobcurtain electric + openprophet-saas have no dedicated LoadBalancer — likely not
> currently deployed to these clusters (openprophet's repo just holds the prod_cluster
> kubeconfig for ops access), or served as static sites elsewhere.

See `NODEBALANCERS.md` for the consolidation plan and `PLAN.md` for the full no-action roadmap.

---

## Confirmed deployment topology (live, via kubectl)

### prod_cluster (222618, us-east) — 3 nodes
Namespaces: `arcbeta`(empty), `carshowdb`, `cartogopher`, `cert-manager`, `dummy-api`,
`ecommerce`, `tailscale`, `unbelievable`.

- **`cartogopher` ns is already a mini shared-host behind ONE LoadBalancer.** The
  `cartogopher-loadbalancer` (143.42.178.227) points at a pod labelled `app: nginx-ingress`,
  which is actually a **hand-rolled `nginx:alpine` reverse proxy** driven by the `nginx-config`
  ConfigMap. It routes by hostname/path to ClusterIP services:
  - `cartogopher.com` → cartogopher-api / cartogopher-landing
  - `openprophet.io` → **openprophet-api** (:3738) + **openprophet-datastream** (:2222, `/data/*`)
  - also in-ns: postgres, redis, loki-proxy
  → **openprophet-saas IS deployed** here as ClusterIP (no dedicated LB — already efficient).
- `dummy-api`, `ecommerce`, `unbelievable` each have their **own** `type=LoadBalancer` service
  (= 3 separate NodeBalancers) instead of routing through the existing proxy. **This is the waste.**
- **`carshowdb`**: `carshowdb-api` is **crash-looping (750+ restarts)** — root cause: its
  `DATABASE_URL` points at a dead **Supabase** pooler (tenant/user no longer exists). Its
  `cloudflared` (Cloudflare Tunnel) + `tailscale` sidecars are healthy — **it's exposed via
  Cloudflare Tunnel, not a NodeBalancer.** Image `stackmaster/carshowdb-api:prod-<sha>` (Docker Hub).
  Fix = update Supabase creds or repoint to self-hosted PG; scale to 0 meanwhile. See `MIGRATION.md`.
- **`arcbeta`**: completely **empty** namespace — leftover from an old anyrent beta. Safe to delete.
  No anyrent/arc workloads actually run on prod_cluster.

### anyrent (550390, us-ord) — 4 nodes
Namespaces: `arc` (the real AnyRent app), `cert-manager`, `keda`, `monitoring`, `default`.
- `arc/anyrent-api`, `arc/anyrent-ui`, `arc/metabase` each have their **own** LoadBalancer (3 NBs).
- **No ingress controller exists** here (cert-manager is present; ACME HTTP-01 solver ingresses
  for anyrentcloud.com/.io are pending — the ingress path is half-built).
- `keda` (autoscaling) + `monitoring` (prometheus/grafana/loki) present.

### default context = local k3s (10.0.0.101) — NOT billed.

## Local repo ↔ deployment map
| Deployment | Namespace / cluster | Local repo / manifest |
|---|---|---|
| cartogopher + nginx proxy | cartogopher / prod | `CartoGopher-SaaS/k8s/` (ingress.yaml, ingress-https.yaml, nginx config) |
| openprophet | cartogopher / prod | `openprophet-saas/` (ClusterIP, routed by cartogopher nginx) |
| anyrent-api | arc / anyrent | `AgenticAnyRentWorkers/AnyRentAPI/.github/service-production.yml` |
| anyrent-ui | arc / anyrent | `AgenticAnyRentWorkers/AnyRentUI/.github/service-production.yml` |
| metabase | arc / anyrent | `AgenticAnyRentWorkers/AnyRentInfrastructure/k8s/metabase.yaml` |
| dummy-api | dummy-api / prod | **no local manifest** — live only in-cluster |
| ecommerce | ecommerce / prod | **no local manifest** — live only in-cluster |
| unbelievable | unbelievable / prod | **no local manifest** — live only in-cluster |
| carshowdb | carshowdb / prod | **no local manifest** — live only in-cluster (broken) |

## GoUS Nanode — KEEP
Per owner: GoUS (g6-nanode-1, $5/mo, us-east) hosts the **my-ecommerce database** and is used
for other things too. Not a deletion candidate. Documented for completeness; revisit only if
its workloads move into a cluster later.
