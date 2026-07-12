#!/usr/bin/env bash
# Idempotently create or update the per-app tunnel-token Secret in namespace
# platform-local. The app's dev ExternalSecret (ESO Kubernetes provider,
# remoteNamespace platform-local) resolves TUNNEL_TOKEN from it: the backing
# Secret is NAMED after the app with a TUNNEL_TOKEN data key.
#
# Run once per app after `idpctl tunnel up -e dev --token-out <file>`:
#   scripts/setup-dev-tunnel-secret.sh <app> <token-file>
#   rm <token-file>        # delete the token file after stashing
#   idpctl render -e dev --file deploy.yaml --image <tag>
#   # idpctl tunnel up ... --verify-access confirms the 302 redirect to CF Access
#
# The token file is written by --token-out (mode 0600); delete it after this
# script runs so it never sits on disk longer than necessary.
#
# PREREQUISITE: kubectl context must point at the dev cluster and have
# create/update permissions in namespace platform-local.
set -euo pipefail

APP="${1:?usage: setup-dev-tunnel-secret.sh <app> <token-file>}"
TOKEN_FILE="${2:?usage: setup-dev-tunnel-secret.sh <app> <token-file>}"
NS="${TUNNEL_SECRET_NS:-platform-local}"
CTX="${KUBE_CONTEXT:-}"

[ -f "$TOKEN_FILE" ] || { echo "!! token file not found: ${TOKEN_FILE}"; exit 1; }

CTX_FLAG=""
if [ -n "$CTX" ]; then
  CTX_FLAG="--context ${CTX}"
fi

echo "==> Applying Secret ${NS}/${APP} (key: TUNNEL_TOKEN) ..."
# shellcheck disable=SC2086
kubectl $CTX_FLAG -n "$NS" create secret generic "$APP" \
  --from-file=TUNNEL_TOKEN="$TOKEN_FILE" \
  --dry-run=client -o yaml | kubectl $CTX_FLAG apply -f -

echo "==> Done. The dev ExternalSecret for '${APP}' will resolve TUNNEL_TOKEN from ${NS}/${APP}."
echo "    Delete the token file now:  rm ${TOKEN_FILE}"
