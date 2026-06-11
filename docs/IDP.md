# Internal Developer Platform (IDP) — design & plan (NO ACTION YET)

> **UPDATE:** research found you already use **Cloudflare Tunnel** (carshowdb's `cloudflared`
> sidecar exposes its API with no NodeBalancer) and a full build/deploy model (Docker Hub
> `stackmaster/*`, self-hosted k8s runner, SSM secrets, pg_dump→R2 backups). The full,
> research-backed version of this plan — single-cluster consolidation, Cloudflare Tunnel as the
> default ingress (replaces ingress-nginx, $0 vs $20/mo), and self-hosted Postgres — is now
> captured in **`../ARCHITECTURE.md`**, which supersedes the ingress-nginx approach sketched
> below. Read that first. (This doc is kept as design history.)

## Why
Today every new app becomes a `Service type=LoadBalancer` → a new **$10/mo NodeBalancer**, plus
hand-edited nginx config, manual DNS, manual TLS. That's how you got to 7 NodeBalancers and a
hand-rolled `nginx:alpine` proxy on prod. An IDP makes the cheap, correct path the *default*:

> **Onboarding a new app = a `values.yaml` + a git push. It gets a ClusterIP, an Ingress route,
> auto TLS, auto DNS, dashboards, and autoscaling — and adds $0 in NodeBalancers.**

This generalizes the pattern you already built by hand in the `cartogopher` namespace.

## What you already have (build on it, don't reinvent)
| Capability | prod_cluster | anyrent | Use |
|---|---|---|---|
| cert-manager | ✅ (616d) | ✅ (154d) | automatic TLS |
| shared proxy | hand-rolled nginx | ❌ none | replace with ingress-nginx |
| autoscaling | ❌ | ✅ KEDA | optional per-app scaling |
| observability | loki-proxy | ✅ prometheus+grafana+loki | auto-wire dashboards/logs |
| Tailscale | ✅ | (via infra) | private/admin access |
| CI deploy | GitHub Actions apply `.github/service*.yml` | same | switch to helm chart |

---

## Architecture

```
                       Internet
                          │  (A record per host, managed by external-dns)
                  ┌───────┴────────┐
                  │  ingress-nginx │   ← exactly ONE NodeBalancer per cluster ($10/mo)
                  └───────┬────────┘
        ┌──────────┬──────┼───────┬───────────┐
     app-a       app-b  app-c   app-d  ...   (each: ClusterIP Svc + Ingress, host-routed)
        │            cert-manager issues TLS per host automatically
   namespace-per-app, deployed from one shared Helm chart
```

### Components to add
1. **ingress-nginx** (one per cluster) — the single front door. Replaces the hand-rolled
   `nginx:alpine` proxy on prod; brand-new on anyrent.
2. **cert-manager ClusterIssuer** (Let's Encrypt prod + staging) — already installed; just add
   the issuers and annotate Ingresses. Auto-TLS for every host, no manual certs.
3. **external-dns** with the **Linode DNS provider** — watches Ingress `hosts` and creates/updates
   Linode DNS records automatically. Kills the manual "repoint DNS" toil during cutovers and for
   every future app. (Needs a Linode token with `domains:read_write`.)
4. **A shared app Helm chart** (`platform/app-chart`) — the heart of the IDP. One chart, per-app
   `values.yaml`. Renders: Deployment, ClusterIP Service, Ingress (+ TLS annotation),
   optional HPA/KEDA `ScaledObject`, optional `ServiceMonitor`, PodDisruptionBudget, resource
   requests/limits. No app ever defines a LoadBalancer again.
5. **Secrets**: standardize on **external-secrets** (or sealed-secrets) instead of the current
   ad-hoc `sync-*-secrets-to-k8s.sh` scripts. Source from AWS SSM (already used for documenso).
6. **Reusable GitHub Actions workflow** — `helm upgrade --install <app> platform/app-chart -f values.yaml`.
   Replaces the per-repo `.github/service*.yml` LoadBalancer manifests.

### Per-app `values.yaml` (the entire contract)
```yaml
name: dummy-api
namespace: dummy-api
image: registry/dummy-api:TAG
port: 8080
host: api.dummyproducts.example.com   # external-dns + cert-manager act on this
replicas: 2
resources: { requests: {cpu: 50m, mem: 128Mi}, limits: {cpu: 500m, mem: 512Mi} }
autoscale: { enabled: true, min: 1, max: 4 }   # KEDA/HPA, optional
monitor: true                                   # emit ServiceMonitor
```

---

## Rollout plan (no action yet)

**Phase A — anyrent first (greenfield ingress, has KEDA+monitoring already).**
1. Install ingress-nginx (1 NB) + ClusterIssuer + external-dns.
2. Author `platform/app-chart`; express anyrent-api / anyrent-ui as values files.
3. Cut api/ui over to ingress; metabase → ClusterIP (internal). Delete the 3 old NBs. (= Phase 2 anyrent)

**Phase B — prod_cluster (migrate off the hand-rolled nginx).**
1. Install ingress-nginx alongside the existing nginx proxy (new NB, temporary overlap).
2. Recreate cartogopher.com + openprophet.io routes as Ingress resources (port the
   `nginx-config` server blocks → Ingress paths). Verify.
3. Onboard dummy-api / ecommerce / unbelievable / carshowdb as values files → Ingress.
4. Move the cartogopher LB's role to ingress-nginx, retire the `nginx:alpine` deployment,
   delete the 3 extra app NBs. End state: 1 NB on prod.

**Phase C — self-service & governance.**
1. A `template-app` repo (cookiecutter): copy → fill `values.yaml` → push → live. Document in a README.
2. Cost guardrails baked in:
   - chart **forbids** `type: LoadBalancer` (lint/policy) — everything goes through ingress.
   - StorageClass with a sane reclaim policy + a **Released-PV janitor** so volumes don't leak
     (that's what created the 6 orphaned $1/mo volumes).
   - burst-instance reaper + concurrency cap for the agentic GPU workers.
   - Linode billing alert threshold.
3. Optional: adopt Argo CD for GitOps if you want declarative, audited deploys across both clusters.

---

## What the IDP buys you
- New project cost: **$0 extra NodeBalancers** (vs $10/mo each today).
- TLS + DNS: automatic, no manual cutovers.
- Consistent observability, autoscaling, resource limits, PDBs — for free per app.
- A single place to enforce cost hygiene, so the bill doesn't silently creep back up.

> This is the "make it like that, but real" version of the cartogopher nginx pattern — and it's
> the prerequisite that makes a later "one cluster" consolidation tractable.
