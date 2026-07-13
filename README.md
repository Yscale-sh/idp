# Jakes Developer Platform

> Turn one small `deploy.yaml` into repeatable Kubernetes deployments managed by Flux.

Jakes Developer Platform (JDP) is an opinionated application delivery layer for teams that already
operate Kubernetes. It validates an app contract, renders the required Helm and Flux resources,
wires optional platform services, and commits desired state for Flux to reconcile.

**Assumption:** you already have a working Kubernetes cluster and administrative access for the
initial bootstrap. JDP does not create nodes, install Kubernetes, configure the control plane, or
choose your cloud, network, storage, or DNS provider.

## What JDP owns

- The `deploy.yaml` application contract and schema.
- Validation, policy checks, planning, rendering, removal, and promotion through `idpctl`.
- One Flux umbrella `HelmRelease` per environment.
- Per-app Deployments, ClusterIP Services, probes, resource limits, autoscaling, secret references,
  optional stores, routes, and observability wiring.
- Optional in-cluster build and ship automation.
- A read-only catalog and `doctor` checks for declared platform capabilities.

## What you own

- A Kubernetes cluster, access control, upgrades, node lifecycle, and network policy.
- A default StorageClass when you enable PVC-backed stores or volumes.
- A LoadBalancer, ingress, or tunnel implementation when applications need external traffic.
- DNS zones and provider accounts used by optional public-routing integrations.
- An image registry and its pull or push credentials.
- Secret backends, workload identity, and every credential value supplied to the platform.
- Backup, recovery, and availability policies appropriate for your workloads.

## Required dependencies

| Dependency | Why it is required |
|---|---|
| Existing Kubernetes cluster | JDP deploys applications to Kubernetes; it does not provision the cluster. |
| `kubectl` access | Required for bootstrap and cluster-facing commands such as `doctor` and `build`. |
| Git repository | Stores environment configuration and rendered desired state. |
| Flux Operator and Flux controllers | Reconcile the repository into the cluster. |
| Image registry | Stores application and platform images reachable by cluster nodes. |
| Go toolchain | Required only when building `idpctl` from source. The module pins its supported Go version. |
| Default StorageClass | Required only for PVC-backed databases, caches, or application volumes. |

## Optional integrations

Enable only what your environment provides.

| Integration | Needed when |
|---|---|
| External Secrets Operator | Secrets should be synchronized from a SecretStore or ClusterSecretStore. |
| AWS SSM Parameter Store | `secrets.backend: ssm` is selected. Prefer workload identity over static AWS keys. |
| KEDA and KEDA HTTP add-on | Applications declare event autoscaling or scale-to-zero. |
| MetalLB or another LoadBalancer implementation | LAN or direct LoadBalancer exposure is enabled. |
| Cloudflare DNS, Tunnel, or Access | Public routes use Cloudflare-managed DNS, tunnels, or access policy. |
| CloudNativePG | PostgreSQL is provisioned through the CNPG module. |
| Redis | Applications declare an in-cluster cache. |
| Loki or another compatible endpoint | Platform-managed log shipping is enabled. |
| Tailscale | A workload explicitly needs tailnet egress. |
| GitHub and `idp-shipper` | Push-to-deploy, in-cluster builds, or automated platform commits are enabled. |

## Credentials and keys

Local `validate`, `plan`, `render`, `catalog`, and scaffold commands do not require cloud
credentials. Cluster and provider credentials are needed only by the features you enable.

| Key or secret | Owner | Purpose |
|---|---|---|
| `CLOUDFLARE_API_TOKEN` or `CF_API_TOKEN` | Operator | Cloudflare DNS, Tunnel, and Access API operations. Scope it to the required account and zones. |
| `CLOUDFLARE_ACCOUNT_ID` or `CF_ACCOUNT_ID` | Operator | Selects the Cloudflare account for tunnel operations. |
| `CLOUDFLARE_ZONE_ID` or `CF_ZONE_ID` | Operator | Selects a DNS zone when it cannot be derived from configuration. |
| `GITHUB_TOKEN` or the shipper `gitToken` Secret | Operator | Reads application repositories and, when enabled, pushes rendered platform commits. |
| Registry pull Secret | Operator | Docker `config.json` or `dockerconfigjson` used by workloads to pull private images. |
| Registry push Secret | Operator | Used only by the optional in-cluster image builder. |
| AWS authentication | Operator | Lets External Secrets read SSM. Workload identity is preferred; static keys are a fallback. |
| `CF_ACCESS_CLIENT_ID` and `CF_ACCESS_CLIENT_SECRET` | Service owner | Authenticate `connectsTo.type: serviceToken` calls through Cloudflare Access. |
| `TUNNEL_TOKEN` | Platform automation | Connector token retrieved and reconciled by tunnel automation. Do not commit it. |
| `DATABASE_URL` and `PRIMARY_DATABASE_URL` | Store module or secret backend | PostgreSQL connection values injected into an app. Declare both for a `primary` database. |
| `REDIS_URL` | Store module or secret backend | Redis connection value injected into an app. |
| Application keys | Application owner | Declare key names in `secrets[]`; store values in the selected backend, never in Git. |

## The application contract

```yaml
app: myapp
runtime:
  image: ghcr.io/your-org/myapp
  port: 8080
routes:
  - host: api
    public: true
probes:
  path: /health
sizing:
  profile: minimal
  replicas: 2
db:
  - { name: primary, type: postgres }
secrets:
  - DATABASE_URL
  - PRIMARY_DATABASE_URL
env:
  LOG_LEVEL: info
```

The image repository is tagless. A deployment supplies the immutable tag or digest. JDP derives
the namespace, workload, ClusterIP Service, probes, limits, store wiring, and secret references.

## Local render-only quickstart

This path exercises the contract without contacting a cluster or cloud provider.

```bash
git clone https://github.com/yscale-sh/idp.git
cd idp
make build
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl plan --env dev --file examples/carshowdb/deploy.yaml \
  --image ghcr.io/your-org/carshowdb-api:dev-example
./idpctl render --env dev --file examples/carshowdb/deploy.yaml \
  --image ghcr.io/your-org/carshowdb-api:dev-example
```

## Bootstrap into an existing cluster

1. Fork this repository.
2. Edit `idp.yaml` with your registry prefix and fork URL.
3. Run `idpctl init --env <env>` and configure `environments/<env>/cluster.yaml`.
4. Install the Flux Operator and controllers in your cluster.
5. Point `clusters/<env>/flux-instance.yaml` at your fork and branch.
6. Configure only the SecretStores, modules, storage, routes, and provider integrations you use.
7. Apply the Flux bootstrap resource once. Flux is the only ongoing writer.
8. Render an application, commit `clusters/<env>/platform.yaml`, and push the environment branch.

Do not use `kubectl apply` or `helm upgrade` for application deployments. Commit desired state and
let Flux reconcile it.

## Common commands

```bash
idpctl init --env dev
idpctl validate --file deploy.yaml
idpctl plan --env dev --file deploy.yaml --image <image:tag>
idpctl render --env dev --file deploy.yaml --image <image:tag>
idpctl promote <app> prod --from dev --file deploy.yaml
idpctl remove --env dev --app <app>
idpctl infra render --env dev
idpctl catalog --all --out-dir public
idpctl doctor --env dev
idpctl dns sync --env prod
idpctl tunnel up --env prod --file deploy.yaml
```

## Environment and promotion model

Environment behavior is data in `environments/<env>/cluster.yaml`: Flux branch, route zones,
secret backend, enabled modules, resource bounds, and supported seams. Promotion reads the image
already selected in a source environment and renders the same artifact for the target environment.
It never rebuilds the image.

Production rejects mutable tags. Rollback is a Git revert of the desired-state commit.

## Repository layout

| Path | Purpose |
|---|---|
| `cmd/idpctl`, `internal/` | CLI and render, policy, catalog, provider, and cluster logic. |
| `cmd/idp-shipper` | Optional in-cluster build and delivery automation. |
| `schemas/deploy.schema.json` | Machine-readable application contract. |
| `charts/app` | Shared application chart. |
| `charts/cluster` | Environment umbrella chart. |
| `charts/infra`, `modules/` | Optional platform modules and their registry. |
| `environments/` | Reference environment inputs. |
| `clusters/` | Flux bootstrap skeletons; `platform.yaml` is generated per operational fork. |
| `examples/` | Generic application contracts. |
| `docs/` | Architecture, environments, secrets, CLI, and operating guides. |

## Security rules

- Never commit credentials, provider exports, `.env` files, kubeconfigs, or rendered Secrets.
- Keep generated operational `clusters/*/platform.yaml` in the deployment fork, not this reference repository.
- Prefer workload identity and narrowly scoped tokens.
- Treat Git write credentials and registry push credentials as privileged.
- Review the rendered plan before committing changes to an environment branch.

See [SECURITY.md](SECURITY.md), [docs/SECRETS.md](docs/SECRETS.md), and
[docs/USAGE.md](docs/USAGE.md).

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) and [CONVENTIONS.md](CONVENTIONS.md). Pull requests must not
contain live endpoints, private topology, credentials, or rendered operational state.

## License

Apache-2.0. See [LICENSE](LICENSE).

---

Created by [Jake Nesler](https://www.github.com/jakenesler).
