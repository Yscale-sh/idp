# NodeBalancer consolidation plan ($70/mo → $20/mo) — NO ACTION YET

## The problem
7 NodeBalancers @ $10/mo = **$70/mo**. Each exists because an app is a
`Service type=LoadBalancer`, and LKE provisions one NodeBalancer per such service.
Fix: route every app through **one shared ingress per cluster** behind a single NodeBalancer.

## Key insight from the live clusters
- **prod_cluster already has the pattern half-built.** The `cartogopher-loadbalancer` ($10/mo)
  fronts a hand-rolled `nginx:alpine` reverse proxy that already multiplexes cartogopher +
  openprophet by hostname. The other 3 prod apps (dummy-api, ecommerce, unbelievable) just
  didn't get added to it — they each spun up their own NodeBalancer instead.
- **anyrent has no ingress at all** — all 3 services are raw LoadBalancers. cert-manager is
  already installed, so TLS via ingress is ready to wire.

---

## Quick win: kill the Metabase LB — save $10/mo (lowest risk)
- NB **1957652** ($10/mo) fronts `arc/metabase` on :3000 and **every backend is DOWN** — paying
  for a dead LB. Metabase is internal BI; it should not be public (also a security exposure).
- Plan: `arc/metabase` Service → `ClusterIP`; reach via `kubectl port-forward -n arc svc/metabase 3000`
  or behind the future ingress with auth. Then delete NB 1957652. Reversible, no DNS change.

---

## prod_cluster: 4 → 1  (save $30/mo)
The shared proxy already exists (cartogopher-loadbalancer). Two viable routes:

**Option A (minimal, fastest):** extend the existing `nginx-config` ConfigMap to add
`server_name` blocks for the dummy-api / ecommerce / unbelievable hostnames, `proxy_pass`-ing
to their existing ClusterIP backends (each app already has pods + a Service; just flip the
Service from LoadBalancer to ClusterIP). Then repoint DNS to 143.42.178.227 and delete the 3
extra NBs (1887204, 883011, 1012868).
- Pro: no new components. Con: hand-edited nginx config doesn't scale (this is why we want the IDP).

**Option B (recommended, sets up the IDP):** install real **ingress-nginx**, migrate the
cartogopher/openprophet routes (and the 3 apps) to `Ingress` resources with cert-manager TLS,
retire the hand-rolled nginx. One NodeBalancer for the controller; delete the other 3.
- See `IDP.md` — this is the foundation of the internal developer platform.

Either way the end state is **1 NodeBalancer on prod_cluster**.

## anyrent: 3 → 1  (save $20/mo)
1. Install ingress-nginx (1 NodeBalancer). cert-manager already present.
2. `anyrent-api`, `anyrent-ui` → `ClusterIP` + `Ingress` (hosts: anyrentcloud.com, www, api.…).
   Finishes the half-done ACME cert issuance automatically.
3. `metabase` → `ClusterIP` (internal only).
4. Repoint DNS to the new ingress NB IP; delete NBs 1952707, 1952704, 1957652.

---

## Savings
| Action | Risk | $/mo |
|---|---|---|
| Metabase LB → ClusterIP + delete NB | low | 10 |
| prod_cluster 4→1 (3 NBs deleted) | med (DNS cutover ×3) | 30 |
| anyrent 3→1 (incl. metabase) | med (DNS cutover ×2) | 20 |
| **NodeBalancer line** | | **$70 → $20 = save $50/mo** |

## Execution guardrails (when you say go)
1. Per app: create the ingress route → curl-verify it serves the site via the new IP → only
   then flip DNS → wait out TTL → only then delete the old NodeBalancer. Reversible until delete.
2. Lower DNS TTLs to 300s a day before cutover.
3. Never delete a NodeBalancer whose app you haven't confirmed serving through the ingress.
4. Capture each live Service spec first (`kubectl get svc … -o yaml`) since dummy-api/ecommerce/
   unbelievable have no local manifests.
