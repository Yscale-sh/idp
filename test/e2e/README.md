# test/e2e — ephemeral end-to-end test

`run.sh` spins up a **throwaway** [k3d](https://k3d.io) cluster, deploys the
shared app chart (as `carshowdb`) plus the `dev-postgres` infra chart, installs
KEDA, asserts everything becomes healthy, and **always tears the cluster down**.

It proves the charts actually deploy and reconcile on a real Kubernetes cluster —
the layer unit/golden tests can't cover.

## Run it

```bash
make e2e
# or
./test/e2e/run.sh
```

## What it does

1. **Detect k3d.** If `k3d` is not installed, it prints `brew install k3d` and
   **exits 0 with a SKIP** — a machine/CI without k3d does not hard-fail.
2. **Create** a uniquely named k3d cluster using an **isolated kubeconfig**, so it
   can never touch a real cluster/context.
3. **Install KEDA** via Helm (the app chart's autoscaling objects depend on it).
4. **Deploy `dev-postgres`** (`helm template … | kubectl apply`) and wait until its
   pod is **Ready** — this is the data store `carshowdb`'s `db: primary` needs.
5. **Deploy `carshowdb`** from `charts/app`, assert the rendered Service is **not**
   a LoadBalancer (policy guardrail), apply it, and wait until the Deployment is
   **Available**.
6. **Tear down** the cluster via a `trap` that runs on success, failure, or Ctrl-C.

## Safety / robustness

- `set -euo pipefail` and a cleanup `trap` on `EXIT INT TERM` — the cluster is
  never leaked, even on failure.
- Isolated `KUBECONFIG` (a temp file) — the test only ever talks to the cluster it
  just created. It never touches a live cluster.
- **Graceful skips** while sibling work is in flight: if the app chart
  (`charts/app/Chart.yaml`) or the `dev-postgres` chart isn't on disk yet, the
  relevant step SKIPs instead of failing.

## Knobs (env vars)

| Var | Default | Meaning |
|---|---|---|
| `E2E_KEEP` | `0` | `1` keeps the cluster up after the run (for debugging). |
| `E2E_TIMEOUT` | `180` | Per-rollout wait, in seconds. |
| `IMAGE` | a public nginx image | Image deployed for `carshowdb`. The real carshowdb image may be private; the e2e validates that the **chart** renders and rolls out, not app business logic, so it deploys a public image on the same port. |
| `APP_PORT` | `8080` | Container/service port for the app. |

## Prerequisites

`k3d`, `kubectl`, and `helm` on `PATH`. Only `k3d` is checked for the SKIP path;
when k3d is present, missing `kubectl`/`helm` are hard errors.
