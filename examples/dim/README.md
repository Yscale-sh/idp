# dim — a three-component product on the platform

[Dim](https://github.com/Dusk-Labs/dim) is a self-hosted media server. It's the platform's
"hard mode" example: one **product** (`app: dim`) made of three **components**, each its own
`deploy.yaml` (in real life, each living in its own repo), each rendering to an isolated
HelmRelease — while sharing stores, a GPU, and an NFS library.

| Component | File | Demonstrates |
|---|---|---|
| `api` | [`api.deploy.yaml`](api.deploy.yaml) | TCP probes (no `/healthz`), `sizing.extraLimits` for an Intel iGPU, NFS + emptyDir volumes, **provisions** the product's Postgres + Redis |
| `scanner` | [`scanner.deploy.yaml`](scanner.deploy.yaml) | `port: 0` worker (no Service/probes), **shares** the api's stores via `provision: false`, shared-RW NFS subpath |
| `ui` | [`ui.deploy.yaml`](ui.deploy.yaml) | `expose.lan` MetalLB LoadBalancer (the only sanctioned LB), `connectsTo` with `scheme: none` for an nginx upstream |

The names compose: components render as `dim-api`, `dim-scanner`, `dim-ui`, but secret roots and
store names stay app-level (`dim`) so siblings share them.

```bash
./idpctl validate --file examples/dim/api.deploy.yaml
./idpctl render --env dev --file examples/dim/api.deploy.yaml     --image ghcr.io/jakenesler/dim:dev-<sha>
./idpctl render --env dev --file examples/dim/scanner.deploy.yaml --image ghcr.io/jakenesler/dim:dev-<sha>
./idpctl render --env dev --file examples/dim/ui.deploy.yaml      --image ghcr.io/jakenesler/dim-ui:dev-<sha>
```

> The NFS `server`/`path` values and the MetalLB `ip` are placeholders — point them at your own
> NAS export and an address from your MetalLB pool.
