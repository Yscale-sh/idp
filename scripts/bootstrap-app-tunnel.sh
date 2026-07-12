#!/usr/bin/env bash
#
# bootstrap-app-tunnel.sh — ONE-TIME Cloudflare Tunnel bootstrap for an app.
#
# `idpctl promote` re-asserts the tunnel ingress + DNS on every deploy, but the
# per-app TUNNEL_TOKEN the cloudflared sidecar actually runs with must be minted
# once and parked in SSM where the app's ExternalSecret reads it. This does that:
#
#   1. sources the shared Cloudflare creds from SSM
#      (/shared/cloudflare/admin-api-token + /shared/cloudflare/account-id)
#   2. `idpctl tunnel up` — ensures the named tunnel exists (idempotent), mints its
#      connector token, sets the ingress (host -> localhost:port) + the proxied DNS
#      CNAME (host -> <tunnelID>.cfargotunnel.com)
#   3. stores the minted token at SSM /apps/<app>/<env>/TUNNEL_TOKEN (SecureString)
#
# Idempotent: re-running adopts the existing tunnel and refreshes the stored token.
# After this, `idpctl promote` keeps ingress + DNS in sync on each deploy.
#
# Run it:   bash scripts/bootstrap-app-tunnel.sh examples/carshowdb/deploy.yaml prod
# Preview:  DRY_RUN=1 bash scripts/bootstrap-app-tunnel.sh examples/carshowdb/deploy.yaml prod
#
# Requires: aws cli (SSM read on /shared/cloudflare/*, SSM write on /apps/*),
#           idpctl on PATH or ./idpctl, and the deploy.yaml.

set -euo pipefail

FILE="${1:?usage: $0 <deploy.yaml> [env] [app-name-override]}"
ENV="${2:-prod}"

# idpctl: prefer PATH, fall back to the repo-root binary.
IDPCTL="${IDPCTL:-idpctl}"
command -v "$IDPCTL" >/dev/null 2>&1 || IDPCTL="./idpctl"
command -v "$IDPCTL" >/dev/null 2>&1 || [ -x "$IDPCTL" ] || { echo "idpctl not found (set IDPCTL=/path/to/idpctl)"; exit 1; }

# App name: from deploy.yaml `app:` unless overridden as arg 3.
APP="${3:-$(awk '/^app:[[:space:]]/{print $2; exit}' "$FILE")}"
[ -n "$APP" ] || { echo "could not read app name from $FILE (pass it as arg 3)"; exit 1; }

SSM_CF_TOKEN="${SSM_CF_TOKEN:-/shared/cloudflare/admin-api-token}"
SSM_CF_ACCOUNT="${SSM_CF_ACCOUNT:-/shared/cloudflare/account-id}"
PARAM="/apps/$APP/$ENV/TUNNEL_TOKEN"

echo "==> bootstrapping Cloudflare Tunnel for ${APP}/${ENV} (from ${FILE})"

# 1. Shared Cloudflare creds from SSM -> env (idpctl tunnel reads these).
# Assign separately from export so a failed `aws` trips `set -e` (SC2155).
CLOUDFLARE_API_TOKEN="$(aws ssm get-parameter --name "$SSM_CF_TOKEN" --with-decryption --query Parameter.Value --output text)"
CLOUDFLARE_ACCOUNT_ID="$(aws ssm get-parameter --name "$SSM_CF_ACCOUNT" --query Parameter.Value --output text)"
export CLOUDFLARE_API_TOKEN CLOUDFLARE_ACCOUNT_ID
[ -n "${CLOUDFLARE_API_TOKEN:-}" ] && [ "$CLOUDFLARE_API_TOKEN" != "None" ] || { echo "missing $SSM_CF_TOKEN in SSM"; exit 1; }
[ -n "${CLOUDFLARE_ACCOUNT_ID:-}" ] && [ "$CLOUDFLARE_ACCOUNT_ID" != "None" ] || { echo "missing $SSM_CF_ACCOUNT in SSM"; exit 1; }

if [ "${DRY_RUN:-0}" = "1" ]; then
  echo "==> DRY RUN: previewing tunnel + DNS (no SSM write)"
  "$IDPCTL" tunnel up --file "$FILE" --env "$ENV" --dry-run
  echo "==> would store the minted token at SSM ${PARAM}"
  exit 0
fi

# 2. Ensure tunnel + token + ingress + DNS; write the token to a temp file.
TOKEN_FILE="$(mktemp)"
trap 'rm -f "$TOKEN_FILE"' EXIT
"$IDPCTL" tunnel up --file "$FILE" --env "$ENV" --token-out "$TOKEN_FILE"
[ -s "$TOKEN_FILE" ] || { echo "tunnel up produced no token"; exit 1; }

# 3. Park the token where the app's ExternalSecret reads it.
aws ssm put-parameter --name "$PARAM" --type SecureString --overwrite \
  --value "file://$TOKEN_FILE" >/dev/null
echo "==> stored TUNNEL_TOKEN at SSM ${PARAM}"
echo "    ingress + DNS were set by 'tunnel up'; promote re-asserts them on each deploy."
