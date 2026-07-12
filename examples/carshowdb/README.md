# carshowdb: the minimal single-service contract

One API service (`component: api`), one public route, one platform-provisioned database. The
simplest complete example on the platform.

| Feature | Demonstrates |
|---|---|
| `db: [{name: primary, type: postgres}]` | Platform provisions dev Postgres and wires `DATABASE_URL` — no connection string in the repo. Replaces a hand-wired external URL (`db: primary` is the minimal form). |
| `probes: {path: /health}` | Custom probe path; the default `/healthz` returns 404 for this image. Without it, liveness kills the pod into CrashLoopBackOff. |
| `autoscale: {enabled: false}` | `scaleToZero` is incompatible with the cloudflared sidecar model: at zero replicas the tunnel connector dies and the tunnel has no origin to forward to. Fixed `replicas: 2` keeps the tunnel alive and provides HA. |

```bash
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl render --env dev --file examples/carshowdb/deploy.yaml --image ghcr.io/yscale-sh/carshowdb-api:dev-<sha>
```
