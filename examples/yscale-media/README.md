# yscale-media multi-component example

`yscale-media` is a self-hosted media server with four workloads in one app manifest: API, scanner,
UI, and an optional distributed transcoder. The workloads share Postgres, Redis, an iGPU, and an
NFS library where declared. It began as a fork of the open-source
[Dim](https://github.com/Dusk-Labs/dim) media server.

The example covers multi-component inheritance, shared stores, worker workloads, GPU limits,
volumes, LAN exposure, and service connections.

## One file, four components

The top level of `yscale-media.deploy.yaml` is the shared base (app identity,
the API image, `build.submodules` for the vendored Rust transcoder, the app-level
Postgres + Redis); each entry under `components:` carries only its deltas. The
platform `Expand()`s it into one isolated HelmRelease per component, identical to
four separate files, with shared fields declared once.

| Component | What it shows |
|---|---|
| `api` | REST/WebSocket tier that inherits the base image and stores, provisions the stores, uses TCP probes, requests the `xlarge` profile and Intel iGPU, and mounts NFS and scratch volumes. |
| `scanner` | Portless worker (`port: 0`) that shares the API stores, uses VPA through `sizing.autosize`, and gets a scratch PVC. |
| `transcode` | GPU worker pool that drains a Redis chunk queue. `replicas: 0` keeps it disabled at rest; `extraLimits: gpu.intel.com/i915` requests an iGPU. |
| `ui` | nginx SPA with its own image and build context. It opts out of stores, exposes the LAN route through MetalLB, and connects to the API Service with `scheme: none`. |

The base declares the app identity, API image, `vendor/yscale-transcode` submodule build, and
Postgres/Redis stores. The API provisions the stores, workers share them, and the UI opts out.

## Optional distributed transcoding

The API coordinates each stream: it plans chunks, enqueues work in
Redis, and assembles the S3/MinIO-backed sharded manifest. With the transcode
pool at `replicas: 0`, the API still transcodes in-process. Enable the worker pool by
scaling the pool (or let KEDA do it on queue depth):

```sh
kubectl -n yscale-media-<env>-transcode scale deploy/<name> --replicas=3
```

One 4K stream can then be sharded across three iGPU workers. Scale the pool back to zero to return
to in-process transcoding.

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

The `YSCALE_*_KEY` values here are `REPLACE_ME` placeholders. In a real instance,
supply them via a Secret/externalSecret. The IPs and the NFS server are example
placeholders (RFC 5737 documentation addresses); swap them for your own LAN
address and storage export.
