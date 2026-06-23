# test/e2e: ephemeral end-to-end test (Flux + umbrella platform)

`run.sh` spins up a **throwaway** [k3d](https://k3d.io) cluster, installs **Flux**
and **KEDA** (plus the HTTP add-on), serves *this* repo to Flux from an in-cluster git
source, reconciles the **real rendered umbrella** (`clusters/dev/platform.yaml`, the
HelmRelease that installs `charts/cluster`, which fans out one HelmRelease per app plus
its Postgres and each enabled module), proves the workloads, and **always tears the
cluster down**.

The platform's contract is the per-app `deploy.yaml` shopping list: a developer declares
a small app contract and the platform derives every Kubernetes object from it. This test
exercises that derivation end to end. It proves the **post-ArgoCD Flux platform** actually
reconciles on a real cluster, which the layer unit/golden tests cannot cover, and answers
one homelab-cutover question that has no documented answer.

## THE HEADLINE: does Flux's source-controller accept a `git://` daemon?

ArgoCD's repo-server happily cloned our in-cluster `git://` daemon (see
`scripts/homelab-bringup.sh`). Flux's docs only list `http(s)://` and `ssh://` for a
`GitRepository`. Before the homelab cutover we need to know empirically whether
Flux's **source-controller** will use a `git://` source.

`run.sh` answers it: it stands up the **same** in-cluster git daemon the homelab uses
(`buildpack-deps:bookworm-scm` running `git daemon` on `:9418`, populated by
`kubectl cp` of a bare clone), applies a Flux `GitRepository`
(`source.toolkit.fluxcd.io/v1`) at `git://…/platformctl.git`, waits, reads
`status.conditions`, and prints the verdict as the headline line:

```
FLUX_GIT_PROTOCOL: accepted
# or
FLUX_GIT_PROTOCOL: rejected: <message from source-controller>
```

## Run it

```bash
make e2e
# or
./test/e2e/run.sh
```

## What it does

1. **Detect k3d.** If `k3d` is not installed, print `brew install k3d` and **exit 0
   with a SKIP**, so a machine/CI without k3d does not hard-fail.
2. **Create** a uniquely named k3d cluster using an **isolated kubeconfig**, so it can
   never touch a real cluster/context.
3. **Build the CLI** (`go build -o idpctl ./cmd/idpctl`) and **render the
   real desired state**: `idpctl render --env dev --file
   examples/carshowdb/deploy.yaml --image ealen/echo-server:0.9.2` plus `idpctl
   infra render --env dev`, so `clusters/dev/platform.yaml` is current. Also assert the
   rendered app/postgres/umbrella manifests contain **0** `type: LoadBalancer`.
4. **Install Flux controllers.** Prefer the Flux CLI (`flux install --components
   source-controller,kustomize-controller,helm-controller`) if present; otherwise
   `helm install flux-operator oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator`
   and apply a minimal **FluxInstance** (`fluxcd.controlplane.io/v1`). Wait until
   source-controller + helm-controller are Available.
5. **Install KEDA** (`kedacore/keda`) **and the HTTP add-on** (`kedacore/keda-add-ons-http`
   `0.14.1`) so the `HTTPScaledObject` CRD exists, because carshowdb is **scale-to-zero**.
   Also install the **Prometheus Operator CRDs** (`prometheus-operator-crds`), because the
   rendered app chart ships a `ServiceMonitor` (the homelab gets that CRD from
   kube-prometheus-stack; an ephemeral k3d cluster does not, and without it the
   carshowdb HelmRelease install fails and Flux remediation tears it down).
6. **Serve this repo in-cluster** from a single pod that runs **both** a `git://`
   daemon (`:9418`, the thing under test) and a Flux-accepted **smart-HTTP** endpoint
   (`nginx` + `fcgiwrap` + `git-http-backend`, `:80`) off the same `kubectl cp`'d bare
   clone.
7. **THE KEY TEST:** apply a `GitRepository` named `flux-system` in `flux-system` at
   `git://…/platformctl.git`, wait, and read `status.conditions[Ready]`. Print
   `FLUX_GIT_PROTOCOL: accepted` or `FLUX_GIT_PROTOCOL: rejected: <message>`.
8. **Reconcile the umbrella:**
   - **If `git://` accepted**, apply `clusters/dev/platform.yaml` (the umbrella
     HelmRelease) against that `git://` source and let Flux's helm-controller
     reconcile the inner releases (carshowdb, carshowdb-postgres, keda,
     keda-http-add-on).
   - **If `git://` rejected**, re-point the **same** `GitRepository` at the
     smart-HTTP endpoint (which Flux **does** accept) and reconcile the **same**
     umbrella over `http://`. If even that source cannot go Ready, fall back to
     installing the inner charts directly via `helm` (umbrella `spec.values`
     extracted with `python3`). **The path that ran is printed** (`platform path:
     …`).
9. **PROVE the platform:** wait for the Postgres StatefulSet Ready and the
   carshowdb HelmRelease to reconcile; assert the carshowdb **HTTPScaledObject**
   (scale-to-zero) exists and the Service is **ClusterIP**; then run a `Job` in
   `carshowdb-dev-api` (image `postgres:16-alpine`, `DATABASE_URL` from the
   `carshowdb-runtime` Secret via `secretKeyRef`) that runs `pg_isready` plus `psql -c
   "select 'CONNECTED ok='||1"` and **must print `CONNECTED ok=1`**, the dead-Supabase
   fix, proven live cross-namespace.
10. **Tear down** the cluster and tmp via a `trap` that runs on success, failure, or
    Ctrl-C.

## Safety / robustness

- `set -euo pipefail` and a cleanup `trap` on `EXIT INT TERM`, so the cluster and tmp
  dir are never leaked, even on failure.
- Isolated `KUBECONFIG` (a temp file) and a uniquely-named cluster, so the test only
  ever talks to the cluster it just created. **It never touches a live/remote
  cluster** (k3d only, it creates its own throwaway cluster).
- `k3d` absent gives a graceful **SKIP (exit 0)**, not a failure.

## Knobs (env vars)

| Var | Default | Meaning |
|---|---|---|
| `E2E_KEEP` | `0` | `1` keeps the cluster up after the run (for debugging). |
| `E2E_TIMEOUT` | `240` | Per-wait/rollout timeout, in seconds. |
| `APP_IMAGE` | `ealen/echo-server:0.9.2` | Image deployed for `carshowdb` (the real image is private). The DB-connectivity proof uses `postgres` directly, so app business logic isn't needed to prove the platform wiring. |

## Prerequisites

`k3d`, `kubectl`, `helm`, `go`, and `git` on `PATH` (the smart-HTTP fallback also uses
`python3`). Only `k3d` is checked for the SKIP path; when k3d is present, the others
are hard errors. A running Docker daemon is required for k3d to create the cluster.
