# Secrets and provisioning

Yscale IDP stores secret declarations in Git and secret values outside Git. The selected environment
backend materializes values into application namespaces.

## Core rules

- `deploy.yaml` lists key names in `secrets[]`; it never contains credential values.
- Rendered state contains Secret references or ExternalSecret objects, not provider exports.
- Per-app values use `/apps/<app>/<env>/<KEY>` by convention.
- Deliberately shared values may use `/shared/<group>/<KEY>`.
- Operators choose and configure the SecretStore or ClusterSecretStore.
- Prefer workload identity or role assumption over static provider keys.

## Backends

A local environment may use a Kubernetes-provider SecretStore. A remote backend such as AWS SSM may
be used when External Secrets and the required workload identity are configured. Backend names and
provider accounts are environment configuration, never platform defaults.

## Store credentials

A PostgreSQL store named `primary` exposes both `DATABASE_URL` and `PRIMARY_DATABASE_URL`. A Redis
store exposes `REDIS_URL`. Depending on the environment, an enabled module may generate these values
or the configured backend may supply them. Applications declare the expected keys either way.

## Provider credentials

Cloudflare, GitHub, registry, object-storage, and tailnet credentials are required only by their
optional integrations. Scope each token to the smallest useful account, repository, zone, or action.
Do not reuse platform write credentials as application credentials.

`TUNNEL_TOKEN` is retrieved and reconciled by tunnel automation when enabled. Access service tokens
use `CF_ACCESS_CLIENT_ID` and `CF_ACCESS_CLIENT_SECRET`. Neither belongs in Git.

## Isolation

A shared platform SecretStore is operationally simple but broadens impact. Stronger isolation uses
per-app stores, scoped roles, or a provisioner that derives grants from `deploy.yaml`. The platform
contract supports either model; operators choose the trust boundary appropriate to their cluster.

## Rotation and recovery

Rotate values in the backend, let External Secrets reconcile them, and restart workloads when the
application cannot reload credentials. Back up backend configuration and recovery procedures
separately from application desired state.

A leaked credential must be revoked at its provider. Removing it from Git is not sufficient.
