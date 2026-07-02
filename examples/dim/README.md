# dim: a three-component product on the platform

[Dim](https://github.com/Dusk-Labs/dim) is a self-hosted media server. It is the platform's
"hard mode" example. One **product** (`app: dim`) made of three **components**, each its own
`deploy.yaml` shopping list (in real life, each living in its own repo). Each list declares a
small app contract, and the platform derives all of the Kubernetes from it: namespace,
Deployment, ClusterIP Service, secret refs, env vars, probes, limits. Each component renders to
its own isolated HelmRelease while the three share stores, a GPU, and an NFS library.

| Component | File | Demonstrates |
|---|---|---|
| `api` | [`api.deploy.yaml`](api.deploy.yaml) | TCP probes (no `/healthz`), `sizing.extraLimits` for an Intel iGPU, NFS plus emptyDir volumes, **provisions** the product's Postgres and Redis |
| `scanner` | [`scanner.deploy.yaml`](scanner.deploy.yaml) | `port: 0` worker (no Service/probes), **shares** the api's stores via `provision: false`, shared-RW NFS subpath |
| `ui` | [`ui.deploy.yaml`](ui.deploy.yaml) | `expose.lan` MetalLB LoadBalancer (the only sanctioned LB), `connectsTo` with `scheme: none` for an nginx upstream |

The names compose: components render as `dim-api`, `dim-scanner`, `dim-ui`, but secret roots and
store names stay app-level (`dim`) so siblings share them.

```bash
./idpctl validate --file examples/dim/api.deploy.yaml
./idpctl render --env dev --file examples/dim/api.deploy.yaml     --image ghcr.io/yscale-sh/dim:dev-<sha>
./idpctl render --env dev --file examples/dim/scanner.deploy.yaml --image ghcr.io/yscale-sh/dim:dev-<sha>
./idpctl render --env dev --file examples/dim/ui.deploy.yaml      --image ghcr.io/yscale-sh/dim-ui:dev-<sha>
```

Each `render` upserts that component into `clusters/dev/platform.yaml`, the env's umbrella
HelmRelease. The deploy is the git commit: push `platform.yaml` on the dev branch and Flux
reconciles it. Never `kubectl apply` or `helm upgrade`.

> The NFS `server`/`path` values and the MetalLB `ip` are placeholders. Point them at your own
> NAS export and an address from your MetalLB pool.
