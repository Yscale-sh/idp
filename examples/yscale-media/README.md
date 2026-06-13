# yscale-media — a fork of `dim`: the optionally-distributed sharded media server

`examples/dim` is the reference three-component media server (api + scanner + ui
sharing one Postgres/Redis, an iGPU, an NFS library, a LAN UI). **yscale-media is
what `dim` becomes in production** — the same product, plus a fourth component:
an **optionally-distributed sharded transcoder** — authored as a single
multi-component shopping list (`yscale-media.deploy.yaml`).

This is the real app idp runs, kept here as the worked example because it
exercises essentially the whole contract.

## One file, four components

The top level of `yscale-media.deploy.yaml` is the shared **base** (app identity,
the api image, `build.submodules` for the vendored Rust transcoder, the app-level
Postgres + Redis); each entry under `components:` carries only its deltas. The
platform `Expand()`s it into one HelmRelease per component — identical to four
separate files, with the boilerplate declared **once**.

| Component | What it shows |
|---|---|
| **api** | the web tier (REST/WS) — inherits the base image + stores and **provisions** them (first user), `probes.type: tcp` (no `/healthz`), `sizing.xlarge` + `extraLimits: gpu.intel.com/i915` (hardware transcode), three volume kinds (read-only NFS-via-FS-Cache PVC, NFS metadata, emptyDir scratch) |
| **scanner** | a **portless worker** (`port: 0`) sharing the base image — **auto-shares** the api's stores (no `provision: false` needed), `sizing.autosize` (VPA), a provisioned scratch PVC (just `name + size + mountPath`) |
| **transcode** | **the sharded scale-out** — a GPU worker pool that drains a Redis chunk queue so one stream is encoded across many pods. `replicas: 0` (the GPU-spend gate: costs nothing at rest), `extraLimits: gpu.intel.com/i915`, shares the api's stores |
| **ui** | nginx SPA — overrides `runtime` (its own image + port 80) and `build.context: ui`, opts out of the app stores (`db: []` / `cache: []`), the **LAN entrypoint** (`expose.lan` → MetalLB, `host: yscale.lan` → ExternalDNS), and `connectsTo` the api's Service with `scheme: none` |

What's DRY'd by the base: the `app` identity, the api image, the `vendor/yscale-transcode`
submodule build, and the Postgres/Redis — each declared **once** instead of four
times. Stores provision once (api owns them; the workers auto-share; the ui opts
out), so there are no hand-written `provision: false` flags.

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

One file renders all four components (CI passes the shared commit tag; each
component renders at `<its runtime.image>:<tag>`):

```sh
idpctl render --env dev --file yscale-media.deploy.yaml --image <tag>
```

The two `YSCALE_*_KEY` values here are `REPLACE_ME` placeholders — in a real
instance supply them via a Secret/externalSecret. IPs and the NFS server are
this author's homelab; swap them for yours.
