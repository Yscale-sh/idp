# yscale-media: a fork of `dim`, the optionally-distributed sharded media server

`examples/dim` is the reference three-component media server (api + scanner + ui
sharing one Postgres/Redis, an iGPU, an NFS library, a LAN UI). **yscale-media is
what `dim` becomes in production**: the same product, plus a fourth component, an
**optionally-distributed sharded transcoder**, authored as a single
multi-component shopping list (`yscale-media.deploy.yaml`).

This is the real app idp runs, kept here as the worked example because it
exercises essentially the whole contract. The per-app shopping list is the
product: a developer declares a small app contract and the platform derives all
of the Kubernetes (namespaces, Deployments, ClusterIP Services, secret refs, env
vars, probes, limits, autoscaling) from it. Nobody writes a Deployment, Service,
or LoadBalancer here.

## One file, four components

The top level of `yscale-media.deploy.yaml` is the shared **base** (app identity,
the api image, `build.submodules` for the vendored Rust transcoder, the app-level
Postgres + Redis); each entry under `components:` carries only its deltas. The
platform `Expand()`s it into one isolated HelmRelease per component, identical to
four separate files, with the boilerplate declared **once**.

| Component | What it shows |
|---|---|
| **api** | the web tier (REST/WS) that inherits the base image + stores and **provisions** them (first user), `probes.type: tcp` (no `/healthz`), `sizing.profile: xlarge` + `extraLimits: gpu.intel.com/i915` (hardware transcode), three volume kinds (read-only NFS-via-FS-Cache PVC, NFS metadata, emptyDir scratch) |
| **scanner** | a **portless worker** (`port: 0`) sharing the base image. It **auto-shares** the api's stores (no `provision: false` needed), uses `sizing.autosize` (VPA), and gets a provisioned scratch PVC (just `name + size + mountPath`) |
| **transcode** | **the sharded scale-out**, a GPU worker pool that drains a Redis chunk queue so one stream is encoded across many pods. `replicas: 0` (the GPU-spend gate: costs nothing at rest), `extraLimits: gpu.intel.com/i915`, shares the api's stores |
| **ui** | nginx SPA that overrides `runtime` (its own image + port 80) and `build.context: ui`, opts out of the app stores (`db: []` / `cache: []`), is the **LAN entrypoint** (`expose.lan` → MetalLB, `host: yscale.lan` → external-dns), and `connectsTo` the api's Service with `scheme: none` |

What the base DRYs out: the `app` identity, the api image, the `vendor/yscale-transcode`
submodule build, and the Postgres/Redis, each declared **once** instead of four
times. Stores provision once (api owns them; the workers auto-share; the ui opts
out), so there are no hand-written `provision: false` flags.

## The "optionally distributed" part

The api is the **coordinator**: it chunk-plans each stream, enqueues work onto
Redis, and assembles the S3/MinIO-backed sharded manifest. With the transcode
pool at `replicas: 0`, the api still transcodes in-process (the single-box `dim`
behavior), so the burst is purely **additive and opt-in**. Turn it on by
scaling the pool (or let KEDA do it on queue depth):

```sh
kubectl -n yscale-media-<env>-transcode scale deploy/<name> --replicas=3
```

Now one 4K stream is sharded across three iGPU workers. Scale back to 0 and
you're a single-box media server again. That is the whole idea: **the same
deploy.yaml contract spans a hobby media server and a distributed transcode
farm, and you opt into the scale-out one component at a time.**

## Render

Deploying is a git commit, and Flux is the only writer. `idpctl render` upserts
all four components into the env umbrella (`clusters/<env>/platform.yaml`); you
commit that file on the env's `flux.branch` and Flux reconciles it. CI passes the
shared commit tag, and each component renders at `<its runtime.image>:<tag>`:

```sh
idpctl render --env dev --file yscale-media.deploy.yaml --image <tag>
```

In dev this is what the in-cluster idp-shipper does automatically: it watches the
app's branch, builds only the images whose inputs changed, renders each component
into `clusters/dev/platform.yaml`, and pushes the commit for Flux to reconcile.
The developer only ever touches the deploy.yaml.

The two `YSCALE_*_KEY` values here are `REPLACE_ME` placeholders. In a real
instance, supply them via a Secret/externalSecret. The IPs and the NFS server are
this author's homelab; swap them for yours.
