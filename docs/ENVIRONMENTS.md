# Environments and promotion

An environment is a policy and capability document, not a cloud account or cluster type. Yscale IDP assumes
the target Kubernetes cluster already exists.

## Environment contract

`environments/<env>/cluster.yaml` declares:

- Flux repository, branch, namespace, and source name.
- Secret backend and SecretStore references.
- Approved route zones.
- Resource bounds.
- Enabled modules and per-environment values.
- Supported seams such as stores, autoscaling, direct exposure, and tunnels.
- The accepted promotion source.

A single cluster may host multiple environments, or each environment may use its own cluster. That
choice belongs to the operator. Use distinct Flux sources, namespaces, and release names whenever
environments share a cluster.

## Reference shapes

### Development

A development environment commonly allows mutable tags, local secret stores, in-cluster development
databases, and a direct LoadBalancer implementation. These are examples, not required defaults.

### Production

A production environment should require immutable artifacts, use a durable secret backend, define
strict route zones and resource bounds, and declare only capabilities present in the cluster.

### Additional environments

Names such as `stage`, `qa`, or `eu-prod` have no built-in semantics. Copy the schema, choose a Flux
branch, declare capabilities, and set an explicit promotion source.

## Promotion

```bash
idpctl promote <app> <target> --from <source> --file deploy.yaml
```

Promotion:

1. Reads the image or digest selected in the source environment.
2. Refuses an unknown workload or invalid promotion source.
3. Applies target environment policy.
4. Renders the same artifact into the target environment's desired state.
5. Prints the Flux branch that must receive the commit.

Promotion never rebuilds an artifact. Production rejects mutable tags. Rollback is a Git revert of
the target desired-state commit.

## Cluster bootstrap boundary

Before Yscale IDP can reconcile an environment, the operator must provide:

- A reachable Kubernetes API and administrative bootstrap access.
- Flux Operator and the required Flux controllers.
- Registry access for platform and application images.
- Any StorageClass, LoadBalancer, DNS, SecretStore, autoscaling, or observability implementation
  declared by the environment.

Yscale IDP can install modules described in the module registry, but it does not provision the Kubernetes
cluster, nodes, provider network, or control plane.

## Validation and runtime checks

`idpctl validate` and `plan` check the declared contract locally. `idpctl doctor --env <env>` reads the
user's cluster and verifies that declared seams are present. A declaration is not proof of runtime
availability; use both policy validation and runtime checks.
