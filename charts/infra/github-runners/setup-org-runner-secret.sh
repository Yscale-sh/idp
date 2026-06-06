#!/usr/bin/env bash
# Create the per-org credentials Secret a github-runners pool authenticates with
# (githubAPICredentialsFrom -> Secret github-token-<org>). Holds a FINE-GRAINED PAT
# scoped to ONLY that one org — nothing is made public, and the token can't reach
# any other org.
#
# PREREQUISITE (one-time): create a fine-grained PAT at
#   https://github.com/settings/personal-access-tokens/new
#   - Resource owner:           the org (Yscale-sh / Cartogopher-ai)
#   - Repository access:        All repositories
#   - Repository permissions:     Actions: Read-only   (Metadata: Read-only is auto)
#   - Organization permissions:   Self-hosted runners: Read and write
#   (If the org requires approval for fine-grained PATs, approve it in the org's
#    Settings -> Personal access tokens before the token works.)
#
# Usage (token from $GITHUB_RUNNER_PAT, else prompted — never passed on argv):
#   ./setup-org-runner-secret.sh Yscale-sh      github-token-yscale
#   ./setup-org-runner-secret.sh Cartogopher-ai github-token-cartogopher
set -euo pipefail

ORG="${1:?usage: setup-org-runner-secret.sh <org-slug> <secret-name>}"
SECRET="${2:?usage: setup-org-runner-secret.sh <org-slug> <secret-name>}"
CTX="${KUBE_CONTEXT:-optiplex-pg}"
NS="${RUNNER_NAMESPACE:-github}"

TOKEN="${GITHUB_RUNNER_PAT:-}"
if [ -z "$TOKEN" ]; then
  read -rsp "Fine-grained PAT for ${ORG} (input hidden): " TOKEN; echo
fi
[ -n "$TOKEN" ] || { echo "!! no token provided"; exit 1; }

# Validate the PAT can mint an org runner registration token (proves org
# Self-hosted runners: write). Mints-and-discards — creates no runner. 201 = good.
echo "==> Validating the PAT against ${ORG} (org runner-registration permission) ..."
CODE="$(curl -sS -o /dev/null -w '%{http_code}' -X POST \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Accept: application/vnd.github+json" \
  -H "X-GitHub-Api-Version: 2022-11-28" \
  "https://api.github.com/orgs/${ORG}/actions/runners/registration-token")"
if [ "$CODE" != "201" ]; then
  echo "!! PAT check failed (HTTP ${CODE}). The token lacks org Self-hosted-runners:write"
  echo "   on ${ORG}, is unapproved, or is scoped to the wrong owner. Fix it and re-run."
  exit 1
fi
echo "    ok (org runner registration permitted)."

echo "==> Applying secret ${NS}/${SECRET} (key: github_token) ..."
kubectl --context "$CTX" -n "$NS" create secret generic "$SECRET" \
  --from-literal=github_token="$TOKEN" \
  --dry-run=client -o yaml | kubectl --context "$CTX" apply -f -

echo "==> Done. ${ORG} runners can now register. Watch them with:"
echo "    kubectl --context ${CTX} -n ${NS} get runnerdeployments,horizontalrunnerautoscalers,runners,pods"
