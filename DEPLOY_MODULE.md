# The deployment module — submit a YAML, runner clones the module, it deploys — DESIGN / NO ACTION

What you described: a developer **submits a `deploy.yaml`**, that **kicks off an action**, and the
action uses a **shared module** (which the **self-hosted runner clones**) to do the deploy. Here's
the concrete shape, grounded in your stack (self-hosted in-cluster runner, `stackmaster/*` images,
Helm, Cloudflare Tunnel, SSM).

## Mental model
```
 app repo (code + deploy.yaml)                platform MODULE repo (versioned @v1)
   └─ git push / "Ship" action  ──────────►   ├─ app-chart/            (Helm chart)
                                               ├─ bin/deploy.sh         (validate → helm → tunnel)
        self-hosted runner (in-cluster):       ├─ .github/workflows/deploy.yml  (reusable, workflow_call)
        1. build & push stackmaster/<app>:tag  ├─ schema/app.schema.json (validates deploy.yaml)
        2. CLONE the module  ──────────────►   └─ bin/ensure-tunnel-route.sh
        3. deploy.sh: validate deploy.yaml,
           helm upgrade app-chart, ensure tunnel route + Access token
```
The **module is versioned** (`@v1` git tag). Apps pin to a version, so a bad module change can't
silently break everyone — you bump `@v1`→`@v2` deliberately.

---

## What the developer writes — `deploy.yaml` (the whole contract)
```yaml
app: dummy-api
port: 8080
hostnames: [api.dummyproducts.com]      # → tunnel route + DNS
replicas: 2
autoscale: { min: 2, max: 5 }            # KEDA/HPA
resources: { cpu: 100m, mem: 128Mi }
secrets:  { ssmPath: /dummy-api/prod }   # external-secrets syncs these in
access:   { humans: false, serviceToken: true }   # Cloudflare Access
tailscaleEgress: false                   # true only if it must reach GoUS DB etc (my-ecommerce)
```
That's it. No Deployment, Service, Ingress, TLS, or NodeBalancer — the chart renders all of it,
ClusterIP-only, exposed via the tunnel.

---

## Pattern A — values live in the app repo (default, for apps that build an image)
**App repo `.github/workflows/ship.yml` (thin — the only platform-specific file in the app):**
```yaml
name: Ship
on:
  workflow_dispatch:                # manual prod, with a PRODUCTION guard (copy carshowdb's)
    inputs: { confirm: { description: 'type PRODUCTION', required: true } }
jobs:
  ship:
    uses: JakeNesler/platform/.github/workflows/deploy.yml@v1   # ← the module's reusable workflow
    with: { app: dummy-api, values: deploy.yaml }
    secrets: inherit
```

**Module `platform/.github/workflows/deploy.yml` (reusable, runs on your runner):**
```yaml
on: { workflow_call: { inputs: {
  app:    { required: true,  type: string },
  values: { default: deploy.yaml, type: string } } } }
jobs:
  deploy:
    runs-on: [self-hosted, k8s, optimized]      # your in-cluster runner
    steps:
      - uses: actions/checkout@v4                # the APP repo (code + deploy.yaml + Dockerfile)
      - name: Build & push
        run: |
          TAG=prod-$(date +%s)-${GITHUB_SHA::8}
          IMG=stackmaster/${{ inputs.app }}:$TAG
          docker buildx build --platform linux/amd64 -t "$IMG" --push .
          echo "IMG=$IMG" >> "$GITHUB_ENV"
      - name: Clone the platform MODULE          # ← "clone the module in our runner"
        uses: actions/checkout@v4
        with: { repository: JakeNesler/platform, ref: v1, path: .platform,
                token: ${{ secrets.PLATFORM_REPO_TOKEN }} }   # PAT/App: read the private module
      - name: Deploy
        run: .platform/bin/deploy.sh --app "${{ inputs.app }}" --values "${{ inputs.values }}" --image "$IMG"
```

**Module `platform/bin/deploy.sh` (the logic):**
```bash
#!/usr/bin/env bash
set -euo pipefail
# 1. validate the submitted yaml against the schema (reject bad/unsafe configs early)
check-jsonschema --schemafile .platform/schema/app.schema.json "$VALUES"
# 2. deploy via the shared chart — ClusterIP only; chart FORBIDS type: LoadBalancer
helm upgrade --install "$APP" .platform/app-chart \
  -f "$VALUES" --set image="$IMG" \
  --namespace "$APP" --create-namespace --wait --timeout 5m
# 3. ensure ingress + identity (idempotent)
.platform/bin/ensure-tunnel-route.sh "$APP" "$VALUES"   # hostname→svc route + DNS via Cloudflare API
.platform/bin/ensure-access.sh       "$APP" "$VALUES"   # mint CF Access service token → SSM if requested
```
Deploys via the runner's **in-cluster ServiceAccount** (RBAC scoped to the app's namespace) — no
kubeconfig shipped, no `--insecure-skip-tls-verify` (unlike today's gin template).

---

## Pattern B — central submit (for stock images / no-build, e.g. metabase)
Put the YAML in the **module repo** and dispatch there — the "submit a yaml to the module" reading,
and it doubles as a **catalog of everything deployed**:
```
platform/apps/metabase.yaml        # image: metabase/metabase:latest, hostnames: [bi.internal...], access.humans: true
platform/.github/workflows/deploy-app.yml   # workflow_dispatch(app) → deploy.sh --app $app --values apps/$app.yaml
```
A PR into `platform/apps/` (reviewed) or a manual dispatch deploys it. No app repo / build needed.

> Both patterns call the **same `deploy.sh` + chart**. Pattern A = app-owned config next to code;
> Pattern B = central, reviewable, catalog-style. Use A for your Go apps, B for stock/no-build apps.

---

## Build vs resolve: how the module reaches the runner
- **Reusable workflow (`uses: …@v1`)** — GitHub resolves the workflow; you still `actions/checkout`
  the module to get the **chart + scripts** onto the runner (the explicit clone you described).
- Alternative: **bake the module into the runner image** (chart + scripts at `/opt/platform`) so no
  clone per deploy — faster, but you rebuild the runner to ship a module change. Start with clone
  (simpler, versioned per-ref); bake later if clone latency matters.
- The chart could also be pushed as a **Helm OCI artifact** and `helm upgrade oci://…/app-chart:1.2`
  instead of checked out — cleanest versioning, optional later.

## Guardrails baked into the module
- **Schema validation** rejects bad `deploy.yaml` before any cluster change.
- Chart **forbids `type: LoadBalancer`** → nobody can re-create a NodeBalancer.
- Runner SA RBAC is **per-namespace**, so an app's deploy can't touch another app.
- `PLATFORM_REPO_TOKEN` (a read-only deploy key / GitHub App) lets the runner clone the private module.
- Module is **versioned (`@v1`)** + the app pins it → controlled rollout of platform changes.

## Effort
The module is ~the `app-chart` (1–2 days) + `deploy.sh`/`ensure-*` scripts (~1 day) + the reusable
workflow (~0.5 day) + schema (~0.5 day). After that, onboarding an app = write `deploy.yaml` + add
the 6-line `ship.yml` → `git push`. That's the "submit a yaml and it deploys" experience, end to end.
