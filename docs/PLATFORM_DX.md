# Platform & developer experience: how deploys work + identity — DISCUSSION / NO ACTION

Answers your three questions: (1) what do I do to deploy, (2) is there a platform repo that's the
deployment module, (3) commit code → a yaml → it just happens. Plus the OAuth/identity piece, which
is the highest-complexity part — discussed honestly at the end.

---

## TL;DR — what you actually do to deploy
- **Once per app (onboarding):** drop a `values.yaml` (the app contract) and point the app repo's CI
  at the platform's reusable workflow. ~10 minutes, scaffolded.
- **Every deploy after that:** `git push`. CI builds `stackmaster/<app>:prod-<sha>`, bumps the tag,
  and the platform deploys it. **You don't touch YAML again** unless you're changing config (a new
  env var, replica count, a new hostname).

That's the "I commit and it does it all in the backend" experience — and it's mostly assembled from
pieces you already run (Docker Hub `stackmaster/*`, the self-hosted in-cluster runner, SSM secrets).

---

## Yes — a `platform` repo IS the deployment module
One repo, owned by you, that every app depends on:
```
platform/
  charts/app-chart/             # THE shared Helm chart: Deployment + ClusterIP Service +
                                #   Tunnel route + HPA/KEDA + PDB + ServiceMonitor. Forbids LoadBalancer.
  .github/workflows/deploy.yml  # reusable workflow apps call: test → build → push → helm upgrade
  bootstrap/                    # one-time cluster setup: cert-manager issuers, cloudflared,
                                #   namespaces+quotas, KEDA, monitoring, CloudNativePG
  apps/<app>/values.yaml        # per-app contract (only if you go GitOps; else lives in app repo)
  tunnel/routes.yaml            # hostname → service map for cloudflared
  scripts/new-app               # scaffolder: generates values.yaml, DNS, secrets, OAuth client
```
App repos stay thin: their code + a Dockerfile + a tiny `values.yaml` + a 3-line CI that calls
`platform/.github/workflows/deploy.yml`. All the k8s complexity lives in the chart, once.

---

## How "commit → deploy" runs — DECIDED: push-based via self-hosted GitHub Actions runner

Keep the existing **self-hosted in-cluster runner** (`stackmaster/anyrent-k8s-runner`). No Flux/Argo,
no new component.

```
git push → GitHub Actions (self-hosted runner, in-cluster)
  → test → build & push stackmaster/<app>:prod-<sha>
  → helm upgrade --install <app> platform/app-chart -f values.yaml
```

Why this fits:
- **Reuses what you run today** — it's your `CI-CD-Go-Production.yml` generalized into one reusable workflow.
- **No cluster creds leave the cluster:** the runner deploys via its in-cluster ServiceAccount (RBAC-scoped
  per namespace), not an exported kubeconfig.
- **No tag-sortability requirement:** CI deploys the exact SHA it just built (a GitOps image-automation
  controller would have needed a sortable tag scheme; push-based doesn't).
- **Rollback:** `helm rollback <app>` or re-run a prior workflow.

> GitOps (Flux or Argo CD) stays an *option later* if you ever want git-as-truth + drift detection + a
> dashboard — it layers on top of the same chart without redoing it. Not needed now.

---

## The app contract — the only thing a dev writes
```yaml
name: cartogopher
namespace: cartogopher
image: stackmaster/cartogopher-api          # tag injected by CI as prod-<sha>
port: 3001
hostnames: [cartogopher.com, www.cartogopher.com]   # → cloudflared route + Cloudflare DNS
replicas: 2
autoscale: { enabled: true, min: 2, max: 5 }        # KEDA/HPA → can spill onto the yscale mesh
secrets: [DATABASE_URL, REDIS_URL]                  # synced from SSM via external-secrets
oauth: { humans: cloudflare-access, service-token: true }   # see identity section
monitor: true
```

---

## Identity / OAuth — "mint oauth creds for all services" (the hard part)
There are **three different things** people lump into "OAuth," and they have very different cost:

**1. Humans logging into internal tools** (Grafana, Metabase, Argo, admin UIs)
→ Put them behind **Cloudflare Access** (OIDC SSO, **free up to 50 users**), in front of the Tunnel
you're already using. One login source (Google/GitHub), central allow-list policies, **zero per-app
auth code**. This is low-complexity and high-value — and it fixes the "Metabase is publicly exposed"
problem from the NodeBalancer review for free.

**2. Service-to-service / machine identity** ("mint creds for all services")
→ **Cloudflare Access *service tokens*** are exactly this: Cloudflare mints a `client_id` +
`client_secret` per service; you attach it to an Access policy; the calling service sends them as
headers. The onboarding scaffolder mints the token via the **Cloudflare API**, writes it to **SSM**,
and `external-secrets` syncs it into the app's namespace. So "mint OAuth creds for all services"
becomes a step in `scripts/new-app` — no self-hosted identity provider needed.
→ *Heavier alternative* if you outgrow that: a self-hosted OIDC issuer (**Zitadel / Authentik /
Keycloak**) doing OAuth2 client-credentials with per-service RBAC. More power, materially more ops
(it becomes a critical stateful service you run). Only do this if you need fine-grained authz or
must own the IdP for compliance.

**3. External-provider OAuth** (end-users "Sign in with Google," Stripe Connect, etc.)
→ These OAuth apps must be registered **with each provider** — the platform *can't* mint them. What
the platform does: store the client_id/secret in SSM and sync via external-secrets, and the
scaffolder creates the placeholders so you don't forget one. This stays per-app by nature.

**Recommendation:** Cloudflare Access for #1 and #2 (it's free, central, automatable, and rides the
Tunnel you already have), SSM+external-secrets for #3. Revisit self-hosted OIDC only if you hit its
limits. This is the lowest-complexity way to "mint creds for all services."

---

## Complexity — the honest version
**Build effort (one-time):**
| Piece | Effort | Notes |
|---|---|---|
| Shared Helm `app-chart` | ~1–2 days | the core; get it right once |
| Reusable CI workflow | ~1 day | generalize your existing Go CI |
| Cloudflare Tunnel + route automation | ~1 day | + DNS via CF API |
| SSM → external-secrets | ~0.5 day | you already push to SSM |
| Cloudflare Access (humans + service tokens) | ~1 day | policies + scaffolder minting |
| `scripts/new-app` scaffolder | ~1 day | **this is what keeps onboarding to minutes** |
| CloudNativePG (separate workstream) | ~1 day | per POSTGRES_BACKUPS.md |
| Argo CD (optional) | +~1 day | only if you want pull-based GitOps |

**Ongoing complexity (be clear-eyed):**
- The "it just deploys" magic = **layers** (chart → CI → tunnel → secrets → identity). Each layer is
  simple; the *sum* needs a one-page runbook, because when it breaks you debug across layers. This is
  the real tax of any IDP — you trade per-app boilerplate for one shared system you must understand.
- **Onboarding a new app** is where complexity concentrates (image, values, DNS/route, secrets,
  OAuth client, monitoring). The scaffolder is non-negotiable — without it, onboarding is error-prone;
  with it, it's a single command. **Per-commit deploys stay trivial.**
- You now own: k3s control-plane health (snapshots), node/CSI, secret + OAuth-token rotation. That's
  the cost of leaving managed services — offset by ~$120–140/mo saved and full control.

**What keeps it sane:** one chart, one CI workflow, one tunnel, one identity source (Cloudflare
Access), one scaffolder. Resist per-app special cases — every exception is future debugging.

---

## Decisions — MADE
1. **Deploy model:** ✅ **push-based via the self-hosted GitHub Actions runner** (no Flux/Argo now).
2. **Identity:** ✅ **Cloudflare Access** (humans, OIDC SSO) **+ Access service tokens** (services),
   minted by the scaffolder via the Cloudflare API → SSM → external-secrets. No self-hosted OIDC.

Everything else (the `app-chart`, Cloudflare Tunnel, SSM→external-secrets, the `new-app` scaffolder)
is unchanged by these — it's the shared core regardless.
