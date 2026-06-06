# Secrets & provisioning — the multi-tenant trust model

> **Status: design decision, not yet implemented.** Captured 2026-06-06. The
> single-store model below is what exists in spirit today (dev uses a local
> backend; prod *specs* `backend: ssm`); the per-tenant model is the target to
> implement when prod / multi-tenant is actually on the table. This doc is the
> decision so it doesn't get lost. Naming conventions live in
> [`CONVENTIONS.md`](CONVENTIONS.md) §5; the Crossplane hook is in
> [`DEPLOY_GO_CLI.md`](DEPLOY_GO_CLI.md).

## The core move: the platform mints secrets, the tenant never holds a key

A developer's `deploy.yaml` declares *needs* — `db: postgres`, `cache: redis`,
`storage: {type: s3}`, secrets at `/apps/<app>/<env>`. The platform's job is not
just to *read* those secrets, it is to **provision the backing resource and mint
its credential**:

```
deploy.yaml (shopping list)
  → platform PROVISIONS the resource   (DB / bucket / IAM identity / "account")
  → platform CAPTURES the generated creds
  → platform WRITES them to /apps/<app>/<env>/*   (SSM)
  → the app READS them at runtime via its OWN scoped SecretStore
```

"Bring your own secret" → **"the platform mints your secret."** A tenant is given
a provisioned, pre-wired app; they never see, paste, or rotate a raw credential.
Same principle as the rest of the platform: the developer writes a shopping list,
the platform derives the implementation — here, the *credential* is part of the
implementation.

## Why the single shared store is a non-starter for multi-tenant

Today's shape is **one `platform-ssm` ClusterSecretStore** backed by **one AWS
identity** that external-secrets uses to pull every app's root
(`/apps/<app>/<env>/*`) and the shared groups (`/shared/*`). That means **one
credential can read every tenant's secrets**. Even when it isn't literally root,
it's "one key reads everything" — unacceptable the moment we host apps for others.

## Two-tier trust (the thing to lock in before prod)

Split the privilege into two identities with opposite shapes:

| Tier | Who | Privilege | Trust |
|---|---|---|---|
| **Provisioner** | platform control plane / CI | **write**: create DB/bucket/IAM, `ssm:PutParameter` to `/apps/<app>/<env>/*` | privileged, **never handed to a tenant**, runs in one audited place |
| **Per-app reader** | the app at runtime (its SecretStore) | **read-only**, scoped to **only** `/apps/<that-app>/<env>/*` | least-privilege; App A physically cannot read App B |

The provisioner is the one place privilege concentrates — and that's fine, because
it's *the platform itself*, not a tenant. The reader tier is where "least-privilege
by construction" lives: a **namespaced `SecretStore` per app** (not one shared
`ClusterSecretStore`), each backed by an identity scoped to that app's path.

## Derive the grant from the shopping list

The elegant part: `jdpctl render` should also **emit the per-app least-privilege
IAM policy**, derived from the same `deploy.yaml` it already reads:

- `secrets` (always) → `ssm:GetParameter*` on `/apps/<app>/<env>/*` (read-only)
- `storage: {name: uploads, type: s3}` → `s3:GetObject`/`PutObject` on **that
  bucket's prefix only**
- `db` / `cache` → connection creds at the app's SSM path (written by the
  provisioner); no DB-admin grant to the app

No wildcards, no cross-app paths. The grant **auto-updates** as the app's declared
needs change — least-privilege by construction, not by hand.

## Auth: prefer role assumption over static keys

- **Preferred:** the app's ServiceAccount **assumes a scoped IAM role** —
  IRSA / Pod-Identity on EKS, or an OIDC provider on the k3s cluster. No
  long-lived key on the app side at all.
- **Fallback:** static per-app access keys (written by the provisioner, rotated on
  a schedule). Acceptable as a bridge; not the destination.

## The missing piece today: the provisioning step

jdp currently has no "create real resource → capture creds → write SSM" loop —
`dev-postgres` is just an in-cluster HelmRelease with a *placeholder* password
(dev backend). Real provisioning (managed Postgres, S3 buckets, IAM) needs a step
between `render` and "secret exists". **Crossplane is the natural home**:
[`DEPLOY_GO_CLI.md`](DEPLOY_GO_CLI.md) already flags *"Crossplane compatibility,
not day-one Crossplane."* A `storage: s3` claim → provision bucket + scoped IAM +
write the connection secret is exactly a Crossplane composite. That's the box the
design left open; this is what fills it.

## Coexistence with platform-infra secrets

Two conventions, two trust scopes — keep them distinct:

- **Tenant-app secrets** → `/apps/<app>/<env>/*`, **per-app** namespaced SecretStore
  (the model above). For workloads the platform deploys for developers/tenants.
- **Platform-infra secrets** → e.g. `/homelab/*` (or `/platform/*`), **one scoped
  reader**. For the platform's *own* plumbing (CI runner PATs, image-builder
  registry creds, the ARC controller key) — not multi-tenant, so a single
  least-privilege reader is correct, not per-app isolation.

## State / next steps

- ✅ A scoped homelab reader IAM user `eso-homelab` exists
  (`ssm:Get*` on `/homelab/*`, read-only) — for the **platform-infra** tier.
- ⏳ External-Secrets Operator is **not yet installed** on the homelab cluster
  (the `external-secrets` module is in the registry, disabled).
- ⏳ Per-app provisioner + reader identities, per-app `SecretStore`, and the
  `jdpctl render` IAM-policy derivation are **unimplemented** — do them when prod /
  multi-tenant is live.

### Lock in before wiring prod
1. Per-app `SecretStore` (namespaced), **not** one shared `ClusterSecretStore`.
2. `jdpctl render` emits a per-app least-privilege IAM policy (applied via
   Terraform/IAM, or an OIDC trust) alongside the app's desired state.
3. The provisioner identity is the only privileged one; per-app readers are
   read-only on their own path. No identity is ever cluster-wide admin.
