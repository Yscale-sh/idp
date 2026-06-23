# Environment / secrets inventory: all apps: RESEARCH (names only, no values)

Pulled from the live deployments on both clusters (2026-06-01). **Only variable names/keys** were
read, never values. This is the list to provision in **SSM → external-secrets**.

This inventory is the input to a contract, not a hand-maintained config. In the platform model a
developer declares a small app contract in `deploy.yaml` (the "shopping list": `app`, `runtime`,
`routes`, `db`, `cache`, `storage`, `connectsTo`, `env`, `secrets`, ...). The platform derives the
env it injects from that contract: Tier-A platform vars, the data-store URLs (`DATABASE_URL`,
`REDIS_URL`), the storage creds, and the per-app `ExternalSecret`. The goal of this snapshot is to
fold every hand-set var below into that derivation so apps stop hard-coding what the platform owns.

## How env is delivered in the platform model
1. **Chart base env**: the `app-chart` injects a common set into *every* app (Tier A) from platform config.
2. **`ExternalSecret` per app**: pulls `/<app>/prod/*` from SSM into a k8s Secret the pod `envFrom`s.
3. **Shared `ExternalSecret`s**: `/shared/<group>/*` (Stripe, SendGrid, storage, Google OAuth) referenced by the apps that need them, so a key is defined once.

> Backend is per env. dev uses a local secrets backend (placeholder values so an app boots
> operator-free); prod uses the SSM external-secrets backend. The keys are the same; the source differs.

---

## Tier A: Platform-injected into EVERY app (not per-app secrets)
The chart sets these; apps stop hard-coding them. (Seen across cartogopher/anyrent/carshowdb today.)
This list is authoritative against `internal/render/env.go` (`TierAEnv`): the renderer injects exactly
these, and `internal/render/values.go` marks them reserved so an app's `env{}` cannot shadow them.
| Var | Source |
|---|---|
| `ENVIRONMENT` | chart (the env name, e.g. `prod`) |
| `LOKI_URL` | platform Loki endpoint |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | platform OTEL collector (injected only when the env provides one) |
| `CONSOLE_LOGGING` | chart |
| `DEPLOY_TIME` | CI (commit/build stamp; left blank for Flux/CI to fill) |
| `PORT` | from `deploy.yaml` (`runtime.port`) |
| ~~`IMAGE_NAME`~~ | **removed**: Helm sets the image directly (today's apps inject it as a secret) |

## Tier B: Sidecar / infra env (platform-managed, NOT in per-app SSM)
| Group | Vars | Disposition |
|---|---|---|
| **Cloudflare Tunnel** | `TUNNEL_TOKEN` | platform-owned, per-app token provisioned at `<appRoot>/TUNNEL_TOKEN` in SSM when a public route is onboarded; pinned into the runtime Secret. carshowdb already uses it |
| **Tailscale egress** | `TAILSCALE_AUTH_KEY/HOSTNAME`, `TS_AUTHKEY/TS_HOSTNAME/TS_KUBE_SECRET/TS_USERSPACE/TS_ACCEPT_DNS` | injected **only when `tailscaleEgress: true`** (e.g. ecommerce→GoUS DB), and only on a non-local env. `TS_AUTHKEY` comes from the shared SSM key `/shared/tailscale/auth-key`. Most apps **drop these** once in-cluster; they were only there to reach the cluster/DB over Tailscale |

## Tier C: Shared secret groups (define once in `/shared/*`, reference from many)
| Group (SSM path) | Keys (names vary per app today, standardize) | Used by |
|---|---|---|
| `/shared/stripe` | `STRIPE_PUBLIC_KEY`(=`STRIPE_PUBLISHABLE_KEY`), `STRIPE_SECRET_KEY`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_*_PRICE_ID/PRICE_*`, `STRIPE_ACCOUNT_ID`, `STRIPE_DESTINATION_ACCOUNT`, `*_TEST` variants | ecommerce, cartogopher, openprophet, anyrent-api/ui, signing, carshowdb |
| `/shared/sendgrid` | `SENDGRID_API_KEY`, `SENDGRID_FROM_EMAIL`, `SENDGRID_FROM_NAME` | nearly all |
| `/shared/storage` | object storage creds (**see naming cleanup below**) | cartogopher, anyrent, carshowdb, ecommerce |
| `/shared/google-oauth` | `GOOGLE_CLIENT_ID`, `GOOGLE_CLIENT_SECRET`, `GOOGLE_REDIRECT_URL` | anyrent-api (end-user "Sign in with Google") |

> **Storage-key naming is inconsistent today and should be unified** (one S3-compatible convention,
> pointed at R2): cartogopher uses `AWS_ACCESS_KEY_ID/…/AWS_S3_BUCKET`; anyrent uses **both** `AWS_*`
> **and** `R2_ENDPOINT/R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY/R2_BUCKET`; carshowdb uses
> `S3_ACCESS_KEY/S3_SECRET_KEY/S3_ENDPOINT/S3_BUCKET/S3_REGION/S3_PUBLIC_BASE_URL/S3_USE_PATH_STYLE`;
> ecommerce uses `LINODE_ACCESS_KEY/LINODE_SECRET_KEY`. Pick one (R2, S3-compatible) in the platform.
> The contract already derives this: a `storage[]` entry named `<NAME>` yields
> `<NAME>_{BUCKET,ENDPOINT,ACCESS_KEY_ID,SECRET_ACCESS_KEY}` (`internal/render/env.go`, `StorageEnvKeys`).

## Tier D: Data stores (the contract yields `DATABASE_URL`/`REDIS_URL`; Mongo is the exception)
The first `db[]` entry yields `DATABASE_URL`, the first `cache[]` entry yields `REDIS_URL` (plus a
prefixed `<NAME>_DATABASE_URL` / `<NAME>_REDIS_URL` form for additional stores; see
`DataStoreEnvKeys`). In dev the platform provisions the store in-cluster (dev-postgres / dev-redis
modules) and supplies the URL; in prod `statefulStores:false`, so the same URL keys come from the
SSM secrets backend and the app declares them in `secrets[]`. Either way the app reads one var.
| Engine | Vars | Apps |
|---|---|---|
| Postgres | `DATABASE_URL` *or* `POSTGRES_DB/HOST/PORT/USER/PASSWORD/SSLMODE` | cartogopher, openprophet-api, anyrent-api/ui, signing, ecommerce(→GoUS today) |
| Redis | `REDIS_URL`/`REDIS_ADDR`/`REDIS_ENDPOINT`, `REDIS_PASSWORD` | cartogopher, openprophet, anyrent, redis |
| **MongoDB** | `MONGO_USERNAME`, `MONGO_PASSWORD` | **unbelievable** (not Postgres; the self-hosted-PG plan doesn't cover it) |

---

## Per-app env lists (full, from live pods)

**dummy-api**: `IMAGE_NAME`(drop), `TAILSCALE_HOSTNAME`, `TAILSCALE_AUTH_KEY`(drop), `LOKI_URL`(A), `ENVIRONMENT`(A). Trivial; almost no app secrets.

**ecommerce** (`/ecommerce/prod`): DB(Postgres@GoUS): `POSTGRES_DB/HOST/PORT/USER/PASSWORD`; storage: `LINODE_ACCESS_KEY/LINODE_SECRET_KEY`; Stripe (`STRIPE_PUBLIC/SECRET_KEY[_TEST]`, `STRIPE_WEBHOOK_SECRET`, `STRIPE_DESTINATION_ACCOUNT`); `SENDGRID_API_KEY`; `SHIPENGINE_API_KEY`, `SHIPENGINE_CARRIER_IDS`; `SESSION_KEY`; `SUCCESS_URL`,`CANCEL_URL`,`CORS_ALLOWED_ORIGINS`; +A. Keeps Tailscale (DB egress).

**unbelievable** (`/unbelievable/prod`): `MONGO_USERNAME`, `MONGO_PASSWORD`, `SENDGRID_API_KEY`. (Mongo.)

**carshowdb** (`/carshowdb/prod`, one `carshowdb-api-secrets`): `DATABASE_URL`(**broken Supabase, repoint**), `JWT_SECRET`, `CORS_ORIGIN`, `PUBLIC_WEB_URL`, `GEMINI_API_KEY`,`GEMINI_MODEL`, `S3_ACCESS_KEY/S3_SECRET_KEY/S3_ENDPOINT/S3_BUCKET/S3_REGION/S3_PUBLIC_BASE_URL/S3_USE_PATH_STYLE`, `SENDGRID_API_KEY/FROM_EMAIL/FROM_NAME`, `STRIPE_PUBLISHABLE_KEY/SECRET_KEY/WEBHOOK_SECRET`, `STRIPE_PRICE_*`, `LOKI_URL`(A),`ENVIRONMENT`(A); sidecars `TUNNEL_TOKEN`,`TS_*`.

**cartogopher** (`/cartogopher/prod`): `DATABASE_URL`,`REDIS_URL`; Stripe (`STRIPE_PUBLIC/SECRET_KEY`,`STRIPE_WEBHOOK_SECRET`,`STRIPE_MONTHLY_PRICE_ID`,`STRIPE_ANNUAL_PRICE_ID`); `SENDGRID_API_KEY`; `DOMAIN`; AWS S3 (`AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY/AWS_REGION/AWS_S3_BUCKET`); `SYNC_ON_STARTUP`; +A (`LOKI_URL`,`CONSOLE_LOGGING`,`ENVIRONMENT`,`OTEL_EXPORTER_OTLP_ENDPOINT`,`DEPLOY_TIME`).

**openprophet-api** (`/openprophet/prod`): `PORT`,`DATABASE_URL`,`REDIS_URL`; Stripe (`STRIPE_SECRET_KEY`,`STRIPE_WEBHOOK_SECRET`,`STRIPE_SUB_PRICE_ID`,`STRIPE_SUB_PRICE_MONTHLY_ID`); `SENDGRID_API_KEY`; `DOMAIN`; `SLACK_WEBHOOK_URL`; `POSTHOG_PROJECT_TOKEN`,`POSTHOG_HOST`; `LOKI_URL`(A).
**openprophet-datastream**: `REDIS_URL`, `OPS_ADMIN_TOKEN`, `FRED_API_KEY`.

**anyrent-api** (`/anyrent/prod`): Postgres (`POSTGRES_DB/HOST/PORT/USER/PASSWORD/SSLMODE`); Stripe (`STRIPE_PUBLIC/SECRET_KEY`,`STRIPE_WEBHOOK_SECRET`,`STRIPE_ACCOUNT_ID`); `SENDGRID_API_KEY`; AWS S3 + R2 (`AWS_*` + `R2_ENDPOINT/R2_ACCESS_KEY_ID/R2_SECRET_ACCESS_KEY/R2_BUCKET`); Redis (`REDIS_ADDR/REDIS_PASSWORD/REDIS_ENDPOINT`); LLM (`LLM_API_KEY/LLM_MODEL/LLM_ADDRESS`,`GEMINI_API_KEY`); Google OAuth (`GOOGLE_CLIENT_ID/SECRET/REDIRECT_URL`); Documenso (`DOCUMENSO_API_KEY/URL`); auth/config (`SESSION_SECRET`,`SECURE_COOKIES`,`TRUSTED_AUTH_PROXIES`,`INTERNAL_API_TOKEN`,`ALLOW_UNSIGNED_WEBHOOKS`,`ENABLE_DEBUG_ENDPOINTS`,`CORS_ALLOWED_PATTERN`,`APP_BASE_URL`,`UI_BASE_URL`,`ANYRENT_ACCOUNT_ID`,`STRIPE_ACCOUNT_ID`,`DAYS_TO_BILL`,`NOTIFICATIONS`,`SUCCESS_URL`,`CANCEL_URL`); +A.

**anyrent-ui** (`/anyrent/prod`): overlaps api; adds `API_BASE_URL`/`API_ENDPOINT`/`UI_ENDPOINT`, `SIGNING_APP_URL`, `COOKIE_DOMAIN`, Stripe `*_TEST`.

**anyrent-signing** (Node, `/anyrent/prod` or `/signing/prod`): `NODE_ENV`,`PORT`,`DATABASE_URL`, Stripe (`STRIPE_SECRET_KEY/WEBHOOK_SECRET`), `SENDGRID_API_KEY`, `SESSION_SECRET`, `DOCUMENSO_API_KEY/URL/WEBHOOK_SECRET`, `FRONTEND_URL`, `LOKI_URL`(A).

**documenso** (3rd-party, `/documenso/prod`): `NEXTAUTH_URL/SECRET`, `NEXT_PUBLIC_WEBAPP_URL`, `NEXT_PRIVATE_ENCRYPTION_KEY`/`_SECONDARY_KEY`, `NEXT_PRIVATE_DATABASE_URL`/`_DIRECT_DATABASE_URL`, SMTP (`NEXT_PRIVATE_SMTP_HOST/PORT/USERNAME/PASSWORD/FROM_*/TRANSPORT`), signing (`NEXT_PRIVATE_SIGNING_PASSPHRASE`/`_LOCAL_FILE_PATH`), upload→R2 (`NEXT_PRIVATE_UPLOAD_ENDPOINT/ACCESS_KEY_ID/SECRET_ACCESS_KEY/BUCKET/REGION/FORCE_PATH_STYLE`), `NEXT_PUBLIC_DISABLE_SIGNUP`.

**metabase**: stock image; env = its DB connection (`MB_DB_*`) only. Internal, behind CF Access.

---

## Identity vars → Cloudflare Access mapping
- `INTERNAL_API_TOKEN`, `OPS_ADMIN_TOKEN` (service-to-service / admin): candidates to replace with
  **Cloudflare Access service tokens** (minted by the scaffolder → SSM).
- `GOOGLE_CLIENT_*` (end-user login): **stays per-app** (external-provider OAuth; the platform stores and syncs it, it can't mint it).
- `SESSION_SECRET`, `SECURE_COOKIES`, `COOKIE_DOMAIN`, `TRUSTED_AUTH_PROXIES`, `ALLOW_UNSIGNED_WEBHOOKS`,
  `ENABLE_DEBUG_ENDPOINTS`: app session/security config. Keep them, but `TRUSTED_AUTH_PROXIES` should
  include the Cloudflare/cloudflared hop, and `ENABLE_DEBUG_ENDPOINTS` must be `false` in prod.

## Cleanups this surfaced
- **Unify object-storage env naming** (`AWS_*`/`R2_*`/`S3_*`/`LINODE_*` → one R2 convention).
- **unbelievable runs MongoDB**: the self-hosted-Postgres plan doesn't cover it. Decide: self-host Mongo, migrate it to Postgres, or keep external.
- **ecommerce DB = GoUS over Tailscale**: its `tailscaleEgress: true` stays until that DB moves in-cluster.
- Drop `IMAGE_NAME` indirection everywhere (Helm sets the image).
- Most `TAILSCALE_*`/`TS_*` go away once apps deploy in-cluster (kept only for real egress like ecommerce→GoUS).
