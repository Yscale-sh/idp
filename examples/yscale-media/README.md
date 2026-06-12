# yscale-media — a fork of `dim`: the optionally-distributed sharded media server

`examples/dim` is the reference three-component media server (api + scanner + ui
sharing one Postgres/Redis, an iGPU, an NFS library, a LAN UI). **yscale-media is
what `dim` becomes in production** — the same product, plus a fourth component:
an **optionally-distributed sharded transcoder**. Same platform contract, no new
machinery — the scale-out is just another `deploy.yaml`.

This is the real app idp runs, kept here as the worked example because it
exercises essentially the whole contract.

## The components

| Component | `deploy.yaml` | What it shows |
|---|---|---|
| **api** | `yscale-api.deploy.yaml` | the web tier (REST/WS) — **provisions** the shared Postgres + Redis, `build.submodules` (private vendored Rust transcoder), `probes.type: tcp` (no `/healthz`), `sizing.xlarge` + `extraLimits: gpu.intel.com/i915` (hardware transcode), three volume kinds (read-only NFS-via-FS-Cache PVC, NFS metadata, emptyDir scratch) |
| **scanner** | `yscale-scanner.deploy.yaml` | a **portless worker** (`runtime.port: 0`) — **shares** the api's stores (`provision: false`), `sizing.autosize` (VPA right-sizes within the `large` envelope), a **provisioned** scratch PVC (just `name + size + mountPath`) |
| **ui** | `yscale-ui.deploy.yaml` | nginx SPA — the **LAN entrypoint** (`expose.lan` → MetalLB) with `expose.lan.host: yscale.lan` (ExternalDNS publishes a real name, not a bare IP), and `connectsTo` the api's in-cluster Service with `scheme: none` (a bare `host:port` for the nginx `upstream`) |
| **transcode** | `yscale-transcode.deploy.yaml` | **the sharded scale-out** — a GPU worker pool that drains a Redis chunk queue so one stream is encoded across many pods. `replicas: 0` (the GPU-spend gate: costs nothing at rest), `extraLimits: gpu.intel.com/i915` (shared-mode iGPU), shares the api's stores |

## The "optionally distributed" part

The api is the **coordinator**: it chunk-plans each stream, enqueues work onto
Redis, and assembles the S3/MinIO-backed sharded manifest. With the transcode
pool at `replicas: 0`, the api still transcodes in-process (the simple `dim`
behaviour) — so the burst is purely **additive and opt-in**. Turn it on by
scaling the pool (or let KEDA do it on queue depth):

```sh
kubectl -n yscale-media-<env>-transcode scale deploy/<name> --replicas=3
```

Now one 4K stream is sharded across three iGPU workers. Scale back to 0 and
you're a single-box media server again. That's the whole idea: **the same
deploy.yaml contract spans a hobby media server and a distributed transcode
farm — you opt into the scale-out one component at a time.**

## Render

```sh
idpctl render --env dev --file yscale-api.deploy.yaml      --image ghcr.io/jakenesler/yscale-media-api:<tag>
idpctl render --env dev --file yscale-scanner.deploy.yaml  --image ghcr.io/jakenesler/yscale-media-api:<tag>   # same image
idpctl render --env dev --file yscale-ui.deploy.yaml       --image ghcr.io/jakenesler/yscale-media-ui:<tag>
idpctl render --env dev --file yscale-transcode.deploy.yaml --image ghcr.io/jakenesler/yscale-media-api:<tag>  # same image
```

The two `YSCALE_*_KEY` values here are `REPLACE_ME` placeholders — in a real
instance supply them via a Secret/externalSecret. IPs and the NFS server are
this author's homelab; swap them for yours.
