# Jakes Developer Platform

> A Linux-like application layer for Kubernetes.

Bring a running Kubernetes cluster. Declare apps in `deploy.yaml`. Commit the rendered state. Flux
keeps the cluster running what Git says should be installed.

JDP does not create Kubernetes clusters. It starts where Kubernetes distributions stop: installing,
configuring, connecting, updating, promoting, and removing applications consistently.

## The model

| Linux concept | JDP concept |
|---|---|
| Machine and kernel | Your existing Kubernetes cluster |
| Distribution userland | JDP charts, policy, modules, and CLI |
| Package or service manifest | `deploy.yaml` |
| Installed package database | `clusters/<env>/platform.yaml` |
| Package repository | Your Git repository and image registry |
| Service manager and reconciler | Flux |
| System services | Optional modules such as databases, cache, autoscaling, and observability |
| Application process | Kubernetes Deployment created from the app manifest |

The unit of deployment is an **app**, not a pile of Kubernetes YAML. An app declares what it needs;
the platform supplies the standard Kubernetes implementation.

## What you need

### Required

| Dependency | Purpose |
|---|---|
| Existing Kubernetes cluster | Runs JDP and its apps. You manage nodes, networking, upgrades, and the control plane. |
| Cluster-admin bootstrap access | Installs Flux and the initial platform resources. |
| `kubectl` | Bootstrap and cluster diagnostics. |
| Git repository | Stores the installed app set and environment configuration. |
| Flux Operator and Flux controllers | Reconcile Git into Kubernetes. |
| Image registry | Stores images reachable by cluster nodes. |
| Go | Needed only to build `idpctl` from source. |

A default StorageClass is required only when an app or module requests persistent storage.

### Optional system services

| Service | Enable it when |
|---|---|
| External Secrets Operator | Secret values come from a SecretStore or ClusterSecretStore. |
| AWS SSM | An environment uses `secrets.backend: ssm`. |
| KEDA | Apps use event autoscaling or scale-to-zero. |
| MetalLB or another LoadBalancer controller | Apps need direct or LAN LoadBalancer addresses. |
| Cloudflare DNS, Tunnel, or Access | Apps use Cloudflare-managed public routes or access policy. |
| CloudNativePG | The platform should provision PostgreSQL through CNPG. |
| Redis | Apps declare an in-cluster cache. |
| Loki | Apps ship logs through the platform logging contract. |
| Tailscale | An app explicitly needs tailnet egress. |
| `idp-shipper` and image builder | Git pushes should build images and update the installed app set automatically. |

## Install JDP on an existing cluster

### 1. Fork and configure the platform repository

Create a repository with GitHub's **Use this template** button. Do not point
Flux at it yet: the checked-in registry and Git URLs intentionally use
`example.invalid`, and identity-dependent commands reject those placeholders.
That prevents a new installation from pushing to or reconciling Yscale's
resources by accident. See [`docs/TEMPLATE.md`](docs/TEMPLATE.md) for the
one-time adoption and safety checklist.

```bash
git clone https://github.com/YOUR_ORG/idp.git
cd idp

# Build the CLI.
make build

# Set your registry and repository in idp.yaml, then create an environment.
./idpctl init --env dev
```

Edit these files:

- `idp.yaml`: your image registry prefix and platform repository URL.
- `environments/dev/cluster.yaml`: secret backend, route zones, modules, resource limits, and Flux branch.
- `clusters/dev/flux-instance.yaml`: the repository and path Flux should reconcile.

Before installing Flux, confirm no placeholder remains:

```bash
rg -n 'example\.invalid|your-org|replace-me' idp.yaml environments clusters
```

### 2. Install Flux

```bash
helm install flux-operator \
  oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator \
  --namespace flux-system \
  --create-namespace

kubectl apply -f clusters/dev/flux-instance.yaml
```

This is the bootstrap exception. After Flux is running, Git is the writer for platform and app
state.

### 3. Install enabled system services

```bash
./idpctl infra render --env dev
git add clusters/dev/platform.yaml
git commit -m "platform: configure dev services"
git push
```

Flux reads the commit and installs the enabled modules.

## Install an app

### 1. Create the app manifest

```bash
./idpctl new app --name myapp --dir ./myapp
```

A minimal `deploy.yaml`:

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
  - name: primary
    type: postgres
secrets:
  - DATABASE_URL
  - PRIMARY_DATABASE_URL
env:
  LOG_LEVEL: info
```

The image value is the repository only. The deploy command supplies the tag or digest.

### 2. Validate and inspect it

```bash
./idpctl validate --file myapp/deploy.yaml
./idpctl plan \
  --env dev \
  --file myapp/deploy.yaml \
  --image ghcr.io/your-org/myapp:abc1234
```

`validate` checks the app manifest. `plan` applies environment policy and prints the desired state
without changing Git or Kubernetes.

### 3. Install it

```bash
./idpctl render \
  --env dev \
  --file myapp/deploy.yaml \
  --image ghcr.io/your-org/myapp:abc1234

git add clusters/dev/platform.yaml
git commit -m "app: install myapp abc1234"
git push
```

Flux reconciles the app. JDP derives the namespace, Deployment, ClusterIP Service, probes, resource
limits, secret references, store wiring, routes, and optional autoscaling.

Do not deploy apps with `kubectl apply` or `helm upgrade`. Change the app manifest or environment,
render it, commit it, and let Flux reconcile it.

## App manifest fields

| Field | Meaning |
|---|---|
| `app`, `component` | Product and optional component identity. |
| `runtime` | Image repository and listening port. Port `0` creates a worker without a Service. |
| `routes` | Hostnames, exposure, and access policy. |
| `probes` | HTTP, TCP, or disabled health checks. |
| `sizing` | Resource profile, replica count, and autoscaling. |
| `db`, `cache` | PostgreSQL and Redis requirements. |
| `secrets` | Secret key names the app expects. Values remain outside Git. |
| `env` | Non-secret application configuration committed to Git. |
| `volumes` | PVC, NFS, Secret, and temporary mounts. |
| `connectsTo` | Addresses and credentials for app-to-app connections. |
| `build` | Image build context, Dockerfile, and submodule requirements. |
| `components` | Several workloads described by one app manifest. |

## Multi-component apps

One app manifest can install an API, UI, worker, and router as one product:

```yaml
app: suite
runtime:
  image: ghcr.io/your-org/suite-api
  port: 8080
components:
  - component: api
    db: [{ name: primary, type: postgres }]
  - component: worker
    port: 0
  - component: ui
    runtime: { image: ghcr.io/your-org/suite-ui, port: 80 }
  - component: router
    runtime: { image: ghcr.io/your-org/suite-router, port: 80 }
    routes: [{ host: suite, public: true }]
```

The router is the single public front component. Sibling components communicate through generated
in-cluster service addresses.

## Secrets and keys

Local `validate`, `plan`, `render`, and `catalog` commands require no provider credentials.
Credentials are needed only for enabled services.

| Key or Secret | Used by |
|---|---|
| `CLOUDFLARE_API_TOKEN` or `CF_API_TOKEN` | Cloudflare DNS, Tunnel, and Access operations. |
| `CLOUDFLARE_ACCOUNT_ID` or `CF_ACCOUNT_ID` | Cloudflare tunnel account selection. |
| `CLOUDFLARE_ZONE_ID` or `CF_ZONE_ID` | Cloudflare DNS zone selection. |
| `GITHUB_TOKEN` or shipper `gitToken` | Reading app repositories and pushing the platform repository. |
| Registry pull `dockerconfigjson` | Pulling private app images. |
| Registry push `dockerconfigjson` | Optional in-cluster image builds. |
| AWS workload identity or credentials | Reading SSM through External Secrets. |
| `CF_ACCESS_CLIENT_ID`, `CF_ACCESS_CLIENT_SECRET` | Service-to-service calls protected by Cloudflare Access. |
| `TUNNEL_TOKEN` | Generated and reconciled by tunnel automation. Never commit it. |
| `DATABASE_URL`, `PRIMARY_DATABASE_URL` | Supplied by the selected PostgreSQL implementation or secret backend. |
| `REDIS_URL` | Supplied by the selected Redis implementation or secret backend. |

App-specific credentials are declared by name in `secrets[]`. Store their values in the configured
secret backend under `/apps/<app>/<env>/<KEY>` or an explicitly shared group.

## Update, promote, and remove apps

```bash
# Update an installed app.
idpctl render --env dev --file deploy.yaml --image ghcr.io/your-org/myapp:def5678

# Install the same artifact in another environment.
idpctl promote myapp prod --from dev --file deploy.yaml

# Uninstall an app or one component.
idpctl remove --env dev --app myapp
idpctl remove --env dev --app suite --component worker
```

Promotion is digest-forward: it installs the artifact selected in the source environment without
rebuilding it. Production rejects mutable tags. Rollback is a Git revert.

## Inspect the system

```bash
# Installed apps from committed desired state.
idpctl catalog --env dev
idpctl catalog --all --out-dir public

# Check whether the cluster provides the capabilities declared by the environment.
idpctl doctor --env dev

# Reconcile optional Cloudflare resources.
idpctl dns sync --env prod
idpctl tunnel up --env prod --file deploy.yaml
```

## Files on disk

| Path | Role |
|---|---|
| `idp.yaml` | Platform identity and registry. |
| `environments/<env>/cluster.yaml` | System profile for an environment. |
| `clusters/<env>/flux-instance.yaml` | Flux bootstrap for that environment. |
| `clusters/<env>/platform.yaml` | Generated installed app and module set. |
| `deploy.yaml` | One app manifest. |
| `charts/app` | Standard implementation for apps. |
| `charts/cluster` | Expands the installed app set into isolated HelmReleases. |
| `charts/infra`, `modules/registry.yaml` | Optional system services. |
| `cmd/idpctl`, `internal/` | CLI and platform implementation. |
| `cmd/idp-shipper` | Optional automatic build and install loop. |
| `schemas/deploy.schema.json` | App manifest schema. |

## Safety rules

- Git contains desired state and secret names, never secret values.
- Use immutable image tags or digests outside development.
- Give provider tokens only the permissions required by enabled services.
- Keep registry push and Git write credentials away from application containers.
- Review `idpctl plan` before changing an environment.
- Roll back by reverting the Git commit.

## Contributing

Issues and pull requests are welcome. Changes must preserve the app manifest contract and must not
include credentials, private endpoints, personal infrastructure, or generated operational state.

## License

Apache-2.0. See [LICENSE](LICENSE).

---

Created by [Jake Nesler](https://www.github.com/jakenesler).
