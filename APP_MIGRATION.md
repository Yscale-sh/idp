# Per-app migration: what each app must change for the platform — RESEARCH / NO ACTION

Researched from each repo's CI/CD + live cluster state (2026-06-01). Target platform recap:
**one k3s cluster · shared Helm `app-chart` (ClusterIP only) · Cloudflare Tunnel ingress (no
NodeBalancers) · push-deploy via the self-hosted in-cluster runner (`helm upgrade`) ·
`stackmaster/<app>:prod-<sha>` · SSM→external-secrets · Cloudflare Access for internal tools.**

## Two deploy patterns exist today — and one of your own apps is already the target
| Pattern | Apps | Distance from target |
|---|---|---|
| **"Gin template" (old)** | dummy-api, my-ecommerce, unbelievable | **furthest** — most change |
| **AnyRent (mid)** | anyrent-api, anyrent-ui | medium |
| **nginx-proxy (prod ns)** | cartogopher, openprophet | medium (ingress rewrite) |
| **carshowdb (already ~target)** | carshowdb | **closest** — copy this |

**carshowdb is the reference.** Its `prod_api.yml` already does: `workflow_dispatch` with a
`"type PRODUCTION"` confirmation + main-branch guard, runs Go tests, pulls secrets from **AWS SSM
(`/carshowdb/prod/*`)**, and exposes via a **Cloudflare Tunnel `cloudflared` sidecar — no Service,
no LoadBalancer**. That's the model; the gin apps should converge to it.

---

## The "gin template" — dummy-api, my-ecommerce, unbelievable (identical `.github/`)
Confirmed identical layout: `service.yml` (NodePort) + `service-loadbalancer.yml` (LoadBalancer),
`k8s/{certificate,cluster-issuer}.yml`, `deployment.yml`, `workflows/{CI-CD,prod}.yml`, `Dockerfile`.

**Current `prod.yml` (manual `workflow_dispatch`):**
- Runs on **GitHub-hosted `ubuntu-latest`**, reaches the cluster over **Tailscale + a base64
  `LINODE_KUBECONFIG`** with `--insecure-skip-tls-verify` on every call.
- Build: `docker buildx ... -t stackmaster/<repo>-gin:latest --push` → **unpinned `:latest`**.
- Deploy: imperative script — `kubectl create secret` for regcred, a **manual TLS secret**
  (`TLS_CRT`/`TLS_KEY` GitHub secrets — the cert-manager files in the repo are **unused**),
  `logging-and-tailscale`, `image_name`; then `kubectl apply service-loadbalancer.yml` (the
  NodeBalancer) and delete+apply `deployment.yml`.
- Pod: **Tailscale sidecar** (`dnsConfig → 100.100.100.100`), Loki logging, **replicas: 1**.
- Secrets: **GitHub Actions secrets** (not SSM).

**Required changes (uniform across all three — fix once, apply 3×):**
1. **Tag:** `:latest` → `prod-<sha>` (immutable, traceable).
2. **Drop `service-loadbalancer.yml`** → ClusterIP via `app-chart` → **deletes its NodeBalancer** (−$10/mo each).
3. **Ingress:** add a **Cloudflare Tunnel route** per hostname; **delete the manual `TLS_CRT/TLS_KEY`**
   (TLS now terminates at Cloudflare edge); remove the unused `certificate.yml`/`cluster-issuer.yml`.
4. **Deploy:** replace the imperative `kubectl apply` script with `helm upgrade --install <app>
   app-chart -f values.yaml` on the **self-hosted in-cluster runner** → drop `LINODE_KUBECONFIG`
   and all `--insecure-skip-tls-verify` (deploys via in-cluster ServiceAccount RBAC).
5. **Secrets:** GitHub secrets → **SSM → external-secrets**; remove the `image_name` secret
   indirection (Helm sets the image directly).
6. **regcred:** becomes a one-time platform-level `imagePullSecret`, not per-deploy.
7. **HA:** `replicas: 1` → `2` + PDB + (optional) HPA via the chart.
8. **Tailscale sidecar:** keep **only where it's an egress dependency** — see my-ecommerce; drop it
   for apps that only used Tailscale to *reach the cluster at deploy time* (no longer needed).
9. Write a `values.yaml` per app.

**my-ecommerce extras:** has `postgres-secrets.yml` + `tailscale-rbac.yml` and `services/`
(checkout, email, shipengine) — it talks to **Postgres on the GoUS Nanode over Tailscale**
(confirms GoUS = the ecommerce DB). So: **keep its Tailscale egress** (until/unless the DB moves
in-cluster), and migrate the Postgres/ShipEngine/email creds to external-secrets.

---

## carshowdb — closest to target, but BROKEN
**Current (`prod_api.yml`, `prod_web.yml`, `backup.yml`, `uptime.yml`):** PRODUCTION-confirm +
main-guard, Go tests, **SSM `/carshowdb/prod/*`**, builds `carshowdb-api` (+ web, + scraper),
exposed via **cloudflared + tailscale sidecars, no Service**. Live pod: **0/1 CrashLoopBackOff**.
**Root cause (confirmed in logs):** `DATABASE_URL` points at a dead Supabase pooler
(`aws-1-us-east-2.pooler.supabase.com:6543` → `FATAL: tenant/user not found`).

**Required changes (least of any app):**
1. **Fix the DB** — update Supabase creds in `carshowdb-api-secrets`, **or** repoint to the new
   self-hosted Postgres. Unblocks the crashloop. (Also `PLAN.md §1.3`.)
2. Adopt `app-chart` `values.yaml`; optionally move the per-pod `cloudflared` **sidecar → the shared
   cluster `cloudflared` Deployment** (cleaner, but the sidecar already works).
3. Run the **scraper** (`Dockerfile.scraper`) as a `CronJob`/`Job` via the chart.
4. Already `prod-<sha>` ✓, SSM ✓, tests ✓, prod-guard ✓, Tunnel ✓ — **use this as the template.**

---

## anyrent-api / anyrent-ui — medium
**Current:** self-hosted runner `[self-hosted,k8s,optimized]`, `stackmaster/* :prod-<sha>`,
`type: LoadBalancer` Services (each = a NodeBalancer), cert-manager TLS (anyrentcloud.com/.io),
replicas: 1. *(CI internals from prior research — re-verify the exact workflow before editing.)*

**Required changes:**
1. Service **LoadBalancer → ClusterIP** → delete NBs 1952707 + 1952704 (−$20/mo).
2. **Ingress:** Cloudflare Tunnel for anyrentcloud.com / www / api / .io; **drop the cert-manager
   `Certificate`** (edge TLS). Finishes the half-built ACME setup by removing the need for it.
3. **Deploy step → `helm upgrade app-chart`** (it already uses the self-hosted runner + stackmaster
   + prod-sha, so this is the *smallest* CI delta of the LB apps).
4. `replicas: 1` → `2`, add **HPA (KEDA already installed)** + PDB.
5. Secrets already SSM-style → external-secrets.

---

## cartogopher + openprophet — medium (ingress rewrite, little app change)
**Current:** ONE LoadBalancer (`cartogopher-loadbalancer`) → a hand-rolled **`nginx:alpine` reverse
proxy** (`nginx-config` ConfigMap) routing `cartogopher.com` and `openprophet.io` (incl. `/data/*` →
openprophet-datastream) to **ClusterIP** backends. openprophet is already ClusterIP.

**Required changes:**
1. Replace the nginx proxy **and** its LoadBalancer with **Cloudflare Tunnel routes** (one per
   hostname/path) → delete NB 2136181 (−$10/mo). Port the `server_name`/`location` map → tunnel routes.
2. cartogopher-api / cartogopher-landing / openprophet-api / openprophet-datastream → `app-chart`
   values (already ClusterIP — easy).
3. Retire the `nginx-config` ConfigMap + `nginx-ingress` Deployment.
4. Decide on the in-namespace `postgres`/`redis` (move to `platform` ns or keep).

---

## metabase — trivial
**Current:** `type: LoadBalancer` :3000 (its NodeBalancer's backends are all down — paying for a dead LB).
**Required:** Service **LoadBalancer → ClusterIP** (delete NB 1957652, −$10/mo); expose only via
**Cloudflare Tunnel behind Cloudflare Access** (it's internal BI — should never be public);
`app-chart` values (stock image, no build).

---

## Effort ranking + NodeBalancers removed
| App | Effort | NB removed | Notes |
|---|---|---|---|
| metabase | trivial | 1957652 (−$10) | LB→ClusterIP + Access; it's dead anyway |
| carshowdb | low* | — (already Tunnel) | *low effort but **must fix DB**; it's the template |
| anyrent-api | low–med | 1952707 (−$10) | closest CI; swap deploy step |
| anyrent-ui | low–med | 1952704 (−$10) | same |
| cartogopher (+openprophet) | med | 2136181 (−$10) | nginx→Tunnel rewrite |
| dummy-api | med | 1887204 (−$10) | gin template overhaul |
| my-ecommerce | med | 883011 (−$10) | + keep Tailscale→GoUS DB, migrate PG/ShipEngine secrets |
| unbelievable | med | 1012868 (−$10) | gin template overhaul |

**Platform-level work done once (not per app):** the `cloudflared` Deployment + tunnel-route config,
external-secrets + SSM wiring, the `app-chart`, the reusable self-hosted-runner workflow, Cloudflare
Access (humans + service tokens), and a shared `regcred` imagePullSecret. Build the gin-template fix
**once** and the three gin apps fall in line together; carshowdb's `prod_api.yml` is the reference to
copy from.
