# carshowdb single-service example

This example defines one API service, one public route, and one platform-provisioned database.

| Feature | Demonstrates |
|---|---|
| `db: [{name: primary, type: postgres}]` | Provisions development Postgres and injects `DATABASE_URL`; no connection string is stored in the repository. |
| `probes: {path: /health}` | Custom probe path; the default `/healthz` returns 404 for this image. Without it, liveness kills the pod into CrashLoopBackOff. |
| `autoscale: {enabled: false}` | `scaleToZero` is incompatible with the cloudflared sidecar model: at zero replicas the tunnel connector dies and the tunnel has no origin to forward to. Fixed `replicas: 2` keeps the tunnel alive and provides HA. |

```bash
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl render --env dev --file examples/carshowdb/deploy.yaml --image ghcr.io/yscale-sh/carshowdb-api:dev-<sha>
```
