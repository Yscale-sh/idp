# Linode cost-reduction plan — NO ACTION YET (planning only)

Current bill **$235/mo**. This plan takes it toward **~$140/mo** with low/medium-risk steps,
and lays the groundwork (an internal developer platform) so it stays low as you add projects.
Nothing here has been executed. See `README.md` for the evidence and `IDP.md` for the platform design.

## Savings at a glance

| # | Action | Risk | Effort | $/mo saved | Running total |
|---|---|---|---|---|---|
| 1 | Delete 6 orphaned block volumes | low | 10 min | $6 | $229 |
| 2 | Metabase LB → ClusterIP, delete its NB | low | 20 min | $10 | $219 |
| 3 | Delete empty `arcbeta` ns + fix/scale-down crashlooping `carshowdb` | low | 20 min | $0 (hygiene) | $219 |
| 4 | prod_cluster: 3 app LBs → shared ingress (4→1 NB) | med | ½–1 day | $30 | $189 |
| 5 | anyrent: 2 app LBs → ingress (3→1 NB, incl. #2) | med | ½–1 day | $20 | $169* |
| 6 | Self-host or downsize managed Postgres | med | 1 day | up to $31 | ~$140 |
| 7 | GPU/ephemeral burst guardrails | low | ½ day | prevents spikes | ~$140 |
| 8 | (later) Consolidate to ONE cluster/region | high | multi-day | ~$40–75 | ~$70–100 |

\* Items 2 and 5 overlap on the Metabase NB — combined NodeBalancer line goes $70 → $20 (save $50).

---

## Phase 1 — Zero-risk cleanup (do first, no traffic impact)

### 1.1 Delete 6 orphaned block-storage volumes — save $6/mo
Verified `Released` / no-PV in both clusters (see README table). Delete these Linode volume IDs:
`10981975, 10981976, 15824233, 12974292, 12929601, 12933597`.
- Also delete their dangling `Released` PV objects in-cluster (cosmetic).
- Double-check nothing was re-bound right before deletion.

### 1.2 Empty `arcbeta` namespace — delete
Confirmed empty on prod_cluster. `kubectl delete ns arcbeta`. Pure hygiene, $0.

### 1.3 carshowdb — stop the crash-loop
`carshowdb-api` has 730+ restarts, 0/1 available, for days. No $ cost but wastes CPU/RAM on a
3-node cluster. Decide: fix the image/config, or `kubectl scale deploy/carshowdb-api --replicas=0`
until you work on it. When it's ready to ship, onboard it via the IDP (ingress route, no new LB).

---

## Phase 2 — NodeBalancer consolidation — save $50/mo  (see NODEBALANCERS.md)

The big lever. 7 NodeBalancers → 2 (one shared ingress per cluster).

- **Metabase quick win (anyrent):** `arc/metabase` → ClusterIP, delete dead NB 1957652. $10/mo, no DNS.
- **prod_cluster (4→1):** the cartogopher LB already fronts a shared nginx proxy. Route
  dummy-api / ecommerce / unbelievable through a shared ingress instead of their own LBs;
  delete NBs 1887204, 883011, 1012868. $30/mo.
- **anyrent (3→1):** stand up ingress-nginx (cert-manager already there), move api/ui/metabase
  behind it, delete NBs 1952707, 1952704, 1957652. $20/mo.

Cutover discipline: route → verify via new IP → flip DNS (low TTL) → delete old NB. Reversible
until the delete. dummy-api/ecommerce/unbelievable have no local manifests — capture their live
Service YAML before changing.

---

## Phase 3 — Managed Postgres decision — up to $31/mo

`anyrent-prod-db` (DBaaS, g6-standard-1, **single node = no HA**) is $32/mo — the largest single
line item. You're paying the managed premium mostly for automated backups/patching, not HA.

Options:
- **A — Keep as-is.** Simplest; worth it if you value hands-off backups/PITR. $0 saved.
- **B — Self-host Postgres in the anyrent cluster** (you already run `postgres` in the cartogopher
  ns on prod, so the pattern exists). A 10–20Gi PVC + CloudNativePG or a simple StatefulSet,
  with backups to the existing Object Storage. **Saves ~$31/mo**, adds ops responsibility
  (you own backups/upgrades). Note the remediator reports show this DB has had **high-disk-usage
  incidents** — fix disk sizing/retention as part of any migration.
- **C — Downsize** if the managed tier has a cheaper option for the actual working set.

Recommendation: **B** if you're comfortable owning backups (you clearly run Postgres already);
otherwise **A**. Decide explicitly — don't migrate a prod DB on autopilot.

---

## Phase 4 — GPU / ephemeral burst guardrails — prevent spikes (~$14/mo today)

The `ys-burst-*` / RTX4000 instances from the agentic workers are ~$14/mo and currently clean
(nothing leaked — none running now). But this is the line most likely to run away.
- Add a hard cap on concurrent burst instances + a max-age reaper (delete any `ys-burst-*` older
  than N hours) so a stuck job can't accumulate cost.
- A daily budget alert (Linode → Billing → set a notification threshold) for early warning.
- Tag burst instances consistently so they're easy to audit/sweep.

---

## Phase 5 (later, bigger) — One cluster + IDP + Cloudflare Tunnel → see MIGRATION.md

prod_cluster (us-east, 3 nodes) and anyrent (us-ord, 4 nodes) are separate clusters in separate
regions. **`MIGRATION.md`** is the full, research-backed plan to collapse them into one production
cluster, standardize deploys on an internal developer platform (the model AnyRent already uses),
and — the key upgrade to Phase 2 above — expose everything through **Cloudflare Tunnel** instead of
NodeBalancers. You already use Tunnel (carshowdb), it's **free**, and it takes the NodeBalancer line
to **~$0** (better than the $20 ingress-nginx end-state). Combined with self-hosting Postgres, the
target is **~$95–115/mo**. Big migration — do after Phase 1 cleanup; sequence is in MIGRATION.md.

---

## Net

| Milestone | Bill |
|---|---|
| Today | $235 |
| After Phase 1 (cleanup) | ~$219 |
| After Phase 2 (NB consolidation) | ~$169 |
| After Phase 3B (self-host Postgres) | ~$140 |
| After Phase 5 (single cluster) | ~$70–100 |

The durable win isn't any single deletion — it's the **IDP pattern** (one ingress per cluster,
every new app = a ClusterIP + an Ingress route, $0 extra NodeBalancers). See `IDP.md`.
