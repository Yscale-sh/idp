# Architecture

Jakes Developer Platform is an application delivery layer for an existing Kubernetes cluster. It
starts at the application contract and ends at reconciled Kubernetes resources. Cluster creation,
node management, networking, and provider infrastructure stay outside its boundary.

## Logical flow

```text
deploy.yaml
    |
    v
idpctl: load -> expand -> validate -> policy -> render
    |
    v
clusters/<env>/platform.yaml
    |
    v
Git branch -> Flux -> umbrella HelmRelease
    |
    v
per-app HelmRelease -> Deployment, Service, probes, limits, secrets, optional stores
```

Flux is the only ongoing writer to the cluster. `idpctl` writes desired state to Git; it does not
perform application releases with direct Helm or kubectl mutations.

## Components

### Application contract

`deploy.yaml` declares the workload, routes, stores, secrets, sizing, probes, volumes, and
connections. Multi-component products share a base contract and render isolated workloads.

### CLI

`idpctl` validates contracts, applies environment policy, renders desired state, promotes immutable
artifacts, removes workloads, renders modules, builds the catalog, and checks declared seams.

### Environment configuration

`environments/<env>/cluster.yaml` describes capabilities available to applications: Flux source,
secret backend, route zones, modules, resource bounds, promotion source, and seam declarations.
Environment names are labels, not infrastructure implementations.

### Umbrella chart

Each environment has one umbrella HelmRelease. The cluster chart expands its values into isolated
HelmReleases for applications, stores, and enabled modules. A failed application does not need to
block reconciliation of unrelated workloads.

### Optional automation

`idp-shipper` can poll application repositories, build images, render updated state, and push the
platform branch. It is optional; manual render and commit remains a complete delivery path.

## Trust boundaries

| Boundary | Trusted with |
|---|---|
| Platform Git repository | Desired state and non-secret configuration. |
| Flux controllers | Read access to desired state and write access to declared cluster resources. |
| Secret backend and External Secrets | Credential values for configured applications and modules. |
| Image registry | Application artifacts and optional pull or push credentials. |
| Provider integrations | Only the scoped DNS, tunnel, storage, or identity permissions enabled by the operator. |
| Application namespace | The secrets and service identity required by that workload. |

Credentials never belong in rendered Git state. Charts render Secret references or ExternalSecret
objects; a configured backend supplies values at reconciliation time.

## Seam contract

Applications request capabilities. Environments declare whether they provide them.

- `statefulStores`: databases, caches, and PVC-backed application volumes.
- `lanExpose`: a LoadBalancer implementation for direct or LAN routes.
- `publicRoutes` or `tunnel`: an external routing implementation.
- `autoscale`: KEDA or another supported autoscaling implementation.
- `observability`: endpoints and controllers required by enabled logging or metrics.

Validation fails closed when an app requests a capability the target environment does not declare.
`idpctl doctor` checks the runtime side of those declarations against the user's cluster.

## Storage and availability

JDP does not prescribe a storage provider or availability topology. Operators choose StorageClasses,
replica counts, backup systems, disruption policy, and recovery objectives. The reference modules
are optional implementations, not cluster prerequisites.

## Promotion

Promotion is digest-forward. JDP reads the artifact selected in the source environment, applies the
target environment's policy and configuration, and renders the same artifact into the target branch.
Mutable production tags are rejected. Rollback is a Git revert.
