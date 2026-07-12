# Environment variable contract

Application configuration is divided by ownership and sensitivity. The platform renders only
declarations and references; credential values never belong in Git.

## Tiers

| Tier | Owner | Examples | Storage |
|---|---|---|---|
| Platform | IDP | `APP_ENV`, `APP_NAME`, `LOKI_URL` | Derived during render |
| Store | IDP | `DATABASE_URL`, `PRIMARY_DATABASE_URL`, `REDIS_URL` | External Secrets or local development store |
| Application secret | App team | `JWT_SECRET`, provider API keys | Declared in `deploy.yaml` `secrets[]`; value held by the configured backend |
| Application config | App team | `LOG_LEVEL`, feature flags, public base URLs | Committed in `deploy.yaml` `env{}` |

## Secret paths

Per-app keys resolve below `/apps/<app>/<env>/<KEY>`. Shared credentials may resolve below
`/shared/<group>/<KEY>` when several apps intentionally use one account. A fork chooses and
documents its own groups; this reference repository contains no live inventory.

A PostgreSQL store named `primary` exposes both `DATABASE_URL` and
`PRIMARY_DATABASE_URL`. Declare both keys in `secrets[]` for an SSM-backed environment.
Redis follows the same pattern through `REDIS_URL`.

## Rules

- Never commit secret values, `.env` files, rendered Secrets, tunnel tokens, or provider exports.
- Keep non-secret URLs in `env{}` only when they are portable configuration, not private topology.
- Prefer one canonical variable per capability; translate legacy variable names inside the app.
- Use `idpctl validate` and `idpctl plan` before rendering.
- Treat `clusters/<env>/platform.yaml` as generated instance state. Operational forks may commit it;
  the public reference repository ignores it.

See [SECRETS.md](SECRETS.md) for the trust model and [ENVIRONMENTS.md](ENVIRONMENTS.md) for
backend selection.
