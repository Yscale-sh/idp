# Flux end-to-end test

`run.sh` creates a temporary [k3d](https://k3d.io) cluster, installs Flux and KEDA
(including the HTTP add-on), serves this repository to Flux from an in-cluster Git
source, and reconciles the rendered umbrella (`clusters/dev/platform.yaml`, the
HelmRelease that installs `charts/cluster`, which fans out one HelmRelease per app plus
its Postgres and each enabled module). A cleanup trap removes the cluster after the run.

The test covers the render-to-reconcile path that unit and golden tests cannot exercise. It also
checks whether Flux source-controller accepts an in-cluster `git://` source.

## Flux `git://` compatibility

Argo CD's repo-server accepted the previous in-cluster `git://` daemon pattern. Flux documents
`http(s)://` and `ssh://` for a `GitRepository`, so this test checks whether source-controller also
accepts `git://`.

`run.sh` answers it: it stands up the same kind of in-cluster git daemon
(`buildpack-deps:bookworm-scm` running `git daemon` on `:9418`, populated by
`kubectl cp` of a bare clone), applies a Flux `GitRepository`
(`source.toolkit.fluxcd.io/v1`) at `git://…/platformctl.git`, waits, reads
`status.conditions`, and prints the verdict:

```text
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

1. Detect `k3d`. If it is not installed, print `brew install k3d` and exit successfully with a
   skip message.
2. Create a uniquely named k3d cluster with an isolated kubeconfig.
3. Build the CLI (`go build -o idpctl ./cmd/idpctl`) and render desired state with
   `idpctl render --env dev --file
   examples/carshowdb/deploy.yaml --image ealen/echo-server:0.9.2` plus `idpctl
   infra render --env dev`, so `clusters/dev/platform.yaml` is current. Also assert the
   rendered application, Postgres, and umbrella manifests contain no `type: LoadBalancer`.
4. Install Flux controllers. Prefer the Flux CLI (`flux install --components
   source-controller,kustomize-controller,helm-controller`) if present; otherwise
   `helm install flux-operator oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator`
   and apply a minimal `FluxInstance` (`fluxcd.controlplane.io/v1`). Wait until
   source-controller + helm-controller are Available.
5. Install KEDA (`kedacore/keda`), the HTTP add-on (`kedacore/keda-add-ons-http` `0.14.1`), and
   Prometheus Operator CRDs. `carshowdb` requires `HTTPScaledObject`, and the
   rendered app chart ships a `ServiceMonitor` (a long-lived dev cluster may get
   that CRD from kube-prometheus-stack; an ephemeral k3d cluster does not, and without it the
   carshowdb HelmRelease install fails and Flux remediation tears it down).
6. Serve this repository from a pod running both a `git://` daemon (`:9418`) and a smart-HTTP endpoint
   (`nginx` + `fcgiwrap` + `git-http-backend`, `:80`) off the same `kubectl cp`'d bare
   clone.
7. Apply a `GitRepository` named `flux-system` in `flux-system` at
   `git://…/platformctl.git`, wait, and read `status.conditions[Ready]`. Print
   `FLUX_GIT_PROTOCOL: accepted` or `FLUX_GIT_PROTOCOL: rejected: <message>`.
8. Reconcile the umbrella:
   - If `git://` is accepted, apply `clusters/dev/platform.yaml` (the umbrella
     HelmRelease) against that `git://` source and let Flux's helm-controller
     reconcile the inner releases (carshowdb, carshowdb-postgres, keda,
     keda-http-add-on).
   - If `git://` is rejected, point the same `GitRepository` at the smart-HTTP endpoint and
     reconcile the same
     umbrella over `http://`. If even that source cannot go Ready, fall back to
     installing the inner charts directly via `helm` (umbrella `spec.values`
     extracted with `python3`). The script prints the selected path (`platform path:
     …`).
9. Wait for the Postgres StatefulSet and carshowdb HelmRelease, assert that the
   `HTTPScaledObject` exists and the Service is ClusterIP, then run a `Job` in
   `carshowdb-dev-api` (image `postgres:16-alpine`, `DATABASE_URL` from the
   `carshowdb-runtime` Secret via `secretKeyRef`) that runs `pg_isready` plus `psql -c
   "select 'CONNECTED ok='||1"`. The command must print `CONNECTED ok=1`.
10. Remove the cluster and temporary directory through a `trap` that runs on success, failure, or
    Ctrl-C.

## Isolation and cleanup

- `set -euo pipefail` and a cleanup `trap` on `EXIT INT TERM`, so the cluster and tmp
  dir are never leaked, even on failure.
- An isolated `KUBECONFIG` and uniquely named cluster limit commands to the test cluster.
- If `k3d` is absent, the test reports a skip and exits zero.

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
