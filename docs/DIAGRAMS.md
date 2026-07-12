# Diagrams: Yscale IDP visual guide

Mermaid diagrams for the reference platform. These are plain GitHub-renderable `flowchart` diagrams.

## 1. The platform at a glance

The per-app `deploy.yaml` is the app contract; it declares what the app needs, not Kubernetes objects. `idpctl render` upserts that contract into `clusters/<env>/platform.yaml`, the one umbrella `HelmRelease` for the environment. Flux is the only writer: it reconciles the umbrella, `charts/cluster` fans it out into a per-app `HelmRelease`, and that release creates the app namespace. The reference shape keeps app services ClusterIP by default, with exposure, data stores, and secrets supplied by environment seams.

```mermaid
flowchart LR
  Contract["deploy.yaml app contract"] --> Render["idpctl render"]
  Render --> Umbrella["clusters/env/platform.yaml umbrella HelmRelease"]
  Umbrella --> Flux["Flux Operator reconciles Git"]
  Flux --> ClusterChart["charts/cluster umbrella"]
  ClusterChart --> AppHR["per-app HelmRelease"]
  AppHR --> Namespace["app namespace"]
  Namespace --> Workload["Deployment and ClusterIP Service"]
  Flux -. "only writer" .-> Namespace
```

## 2. Fork model / bringup

The platform is meant to be forked, not installed as a black-box service. Fork the repo, edit `idp.yaml`, edit `environments/<env>/cluster.yaml`, and edit `clusters/<env>/flux-instance.yaml` so both the env config and Flux sync point at your fork. Then install the Flux Operator and apply the `FluxInstance`; from that point, Flux reconciles the platform state from your fork. App delivery stays the same: a `deploy.yaml` render changes the umbrella, and a Git commit is the deploy.

```mermaid
flowchart LR
  Fork["fork idp repo"] --> Identity["edit idp.yaml"]
  Identity --> Env["edit environments/env/cluster.yaml"]
  Env --> FluxFile["edit clusters/env/flux-instance.yaml"]
  FluxFile --> Operator["install Flux Operator"]
  Operator --> Instance["apply FluxInstance"]
  Instance --> Sync["Flux syncs clusters/env from your fork"]
  Sync --> Platform["platform reconciles"]
```

## 3. Dev -> prod promotion

Production promotion is digest-forward. `idpctl promote <app> prod --from dev` reads the image digest already running in dev's umbrella and re-renders the same artifact under prod policy, prod namespaces, and the prod secrets backend. It never rebuilds the artifact; prod rejects mutable tags with `allowMutableTags: false` and the hard `:latest` guard. Commit the resulting `clusters/prod/platform.yaml` on the `prod` branch so prod Flux applies it; rollback is `git revert` of that commit.

```mermaid
flowchart LR
  DevUmbrella["clusters/dev/platform.yaml running digest"] --> Promote["idpctl promote app prod --from dev"]
  Promote --> ReadDigest["read digest from dev umbrella"]
  ReadDigest --> ProdRender["re-render with prod policy secrets backend namespaces"]
  ProdRender --> Policy["reject latest allowMutableTags false"]
  Policy --> ProdUmbrella["clusters/prod/platform.yaml"]
  ProdUmbrella --> Commit["commit on prod branch"]
  Commit --> ProdFlux["prod Flux syncs refs/heads/prod"]
  ProdFlux --> ProdHR["prod per-app HelmRelease"]
  Commit -. "rollback: git revert" .-> Rollback["previous prod commit restored"]
```

## 4. One route, two fulfillments (the seams)

A `deploy.yaml` route with `public: true` means "expose this to users"; the target environment decides how. In dev, the reference env has no tunnel path and fulfills the route through the `lanExpose` seam: the chart renders a SEPARATE `<app>-lan` MetalLB `LoadBalancer` Service that selects the pods directly, while the app's own ClusterIP Service stays unchanged for in-cluster traffic (`charts/app/templates/lan-service.yaml` — the one sanctioned LoadBalancer). In prod, the env declares the tunnel seam and disables `lanExpose`, so Cloudflare Tunnel is the ingress path and apps stay ClusterIP, never a `LoadBalancer`.

```mermaid
flowchart TB
  Route["same deploy.yaml route: public true"] --> Policy["render policy checks env seams"]
  Policy --> Dev["dev: lanExpose seam, no tunnel"]
  Policy --> Prod["prod: tunnel seam, lanExpose false"]
  Dev --> LanSvc["separate app-lan Service: MetalLB LoadBalancer, selects pods directly"]
  LanSvc --> DevPods["dev app pods"]
  DevService["app ClusterIP Service: unchanged, in-cluster traffic"] --> DevPods
  Prod --> Tunnel["Cloudflare Tunnel"]
  Tunnel --> Cloudflared["cloudflared sidecar"]
  Cloudflared --> ProdService["prod ClusterIP Service"]
  ProdService --> ProdPods["prod app pods"]
```

## 5. Secrets flow

`deploy.yaml` declares secret names only; secret values do not belong in Git. The renderer emits an `ExternalSecret` that references the target environment's store: dev uses the `platform-local` Kubernetes-provider store, while prod uses the `platform-ssm` AWS SSM store. External-Secrets Operator reads that store and materializes a Kubernetes `Secret` in the app namespace. The app chart then injects the secret data as environment variables.

```mermaid
flowchart LR
  Contract["deploy.yaml secrets names only"] --> Render["idpctl render"]
  EnvStore["environment cluster.yaml storeRef"] --> ExternalSecret["ExternalSecret"]
  Render --> ExternalSecret
  ExternalSecret --> ESO["External-Secrets Operator"]
  DevStore["dev platform-local Kubernetes provider"] --> ESO
  ProdStore["prod platform-ssm AWS SSM"] --> ESO
  ESO --> K8sSecret["Kubernetes Secret"]
  K8sSecret --> Pod["pod env vars"]
  Git["Git"] -. "names and refs only" .-> Contract
```

## 6. Module system

`modules/registry.yaml` is the catalog: each module records its source, chart, version, default namespace, and purpose. `environments/<env>/cluster.yaml` is the enable matrix and per-env override file. `idpctl infra render` sets the enabled modules into the same umbrella `HelmRelease` used for apps, then `charts/cluster` templates module `HelmRelease` entries and any needed `HelmRepository` objects. Flux installs the enabled infra into its namespace, such as KEDA, External-Secrets Operator, CloudNativePG, image-builder, or idp-shipper.

```mermaid
flowchart LR
  Registry["modules/registry.yaml catalog source chart version"] --> InfraRender["idpctl infra render"]
  Matrix["environments/env/cluster.yaml enable matrix"] --> InfraRender
  InfraRender --> Umbrella["clusters/env/platform.yaml same umbrella HelmRelease"]
  Umbrella --> Flux["Flux reconciles charts/cluster"]
  Flux --> Repos["HelmRepository for chartRepo modules"]
  Flux --> ModuleHR["module HelmReleases"]
  ModuleHR --> Keda["keda namespace"]
  ModuleHR --> Secrets["external-secrets namespace"]
  ModuleHR --> CNPG["cnpg namespace"]
  ModuleHR --> Builders["image-builder namespace"]
```
