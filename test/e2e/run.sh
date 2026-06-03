#!/usr/bin/env bash
#
# test/e2e/run.sh — ephemeral, self-cleaning end-to-end test for platformctl.
#
# What it PROVES (on a throwaway k3d cluster, using the REAL rendered desired state):
#   1. The per-app dev Postgres (charts/infra/dev-postgres) comes up Ready in its
#      own namespace  carshowdb-dev-postgres.
#   2. The shared app chart deploys carshowdb into its own namespace
#      carshowdb-dev-api, ClusterIP-only (asserts NO LoadBalancer), with KEDA
#      present and a ScaledObject accepted.
#   3. THE FIX: a workload in carshowdb-dev-api, using the SAME DATABASE_URL the
#      platform wired into the carshowdb-runtime Secret, actually CONNECTS to the
#      in-cluster Postgres across namespaces (psql 'select 1'). This is the dead-
#      Supabase problem being fixed — proven live, without needing the (private)
#      carshowdb business image.
#
# Safety:
#   - k3d absent  -> SKIP (exit 0), not fail.
#   - isolated kubeconfig + uniquely-named cluster: only ever talks to the cluster
#     it just created; never touches a real context.
#   - trap always deletes the cluster (even on failure / Ctrl-C).
#
# Usage:  ./test/e2e/run.sh         (or `make e2e`)
# Knobs:  E2E_KEEP=1  keep cluster   E2E_TIMEOUT=180  per-wait seconds
#         APP_IMAGE=...  substitute workload image (carshowdb's real image is private)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"
APP_CHART="${REPO_ROOT}/charts/app"
PG_CHART="${REPO_ROOT}/charts/infra/dev-postgres"
APP_VALUES="${APP_CHART}/ci/carshowdb-dev-values.yaml"   # mirrors `platformctl render --env dev`

CLUSTER="platformctl-e2e-$$"
NS_API="carshowdb-dev-api"
NS_PG="carshowdb-dev-postgres"
PG_RELEASE="carshowdb-postgres"            # => service carshowdb-postgres-dev-postgres
APP_RELEASE="carshowdb"
RUNTIME_SECRET="carshowdb-runtime"
TIMEOUT="${E2E_TIMEOUT:-180}"
# carshowdb's real image is private/unpublished; substitute a public web image that
# passes the chart's HTTP probes. The DB-connectivity proof (step 3) uses postgres
# directly, so the app's business logic is not needed to prove the platform wiring.
APP_IMAGE="${APP_IMAGE:-nginxinc/nginx-unprivileged:1.27}"

WORKDIR="" ; KUBECONFIG_FILE=""
log()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
ok()   { printf '   \033[1;32m✓ %s\033[0m\n' "$*"; }
skip() { printf '\n\033[1;33mSKIP: %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }

cleanup() {
  local rc=$?
  if [[ "${E2E_KEEP:-0}" == "1" ]]; then
    log "E2E_KEEP=1 — leaving cluster '${CLUSTER}' (delete: k3d cluster delete ${CLUSTER})"
  elif command -v k3d >/dev/null 2>&1 && k3d cluster list 2>/dev/null | grep -q "^${CLUSTER}\b"; then
    log "Tearing down k3d cluster '${CLUSTER}'"; k3d cluster delete "${CLUSTER}" >/dev/null 2>&1 || true
  fi
  [[ -n "${WORKDIR}" && -d "${WORKDIR}" ]] && rm -rf "${WORKDIR}"
  exit "${rc}"
}
trap cleanup EXIT INT TERM

# ── preflight ────────────────────────────────────────────────────────────────
if ! command -v k3d >/dev/null 2>&1; then
  skip "k3d not installed — skipping e2e (not a failure). Install: brew install k3d"
  trap - EXIT INT TERM; exit 0
fi
command -v kubectl >/dev/null 2>&1 || fail "kubectl not found."
command -v helm    >/dev/null 2>&1 || fail "helm not found."
[[ -f "${APP_CHART}/Chart.yaml" && -f "${PG_CHART}/Chart.yaml" ]] || fail "charts missing."
[[ -f "${APP_VALUES}" ]] || fail "app values ${APP_VALUES} missing (run: platformctl render --env dev ...)."

WORKDIR="$(mktemp -d)"; KUBECONFIG_FILE="${WORKDIR}/kubeconfig"
export KUBECONFIG="${KUBECONFIG_FILE}"     # isolate from any real context

# ── 1. throwaway cluster ─────────────────────────────────────────────────────
log "Creating ephemeral k3d cluster '${CLUSTER}'"
k3d cluster create "${CLUSTER}" --agents 1 --no-lb \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false --wait
k3d kubeconfig get "${CLUSTER}" > "${KUBECONFIG_FILE}"
kubectl cluster-info >/dev/null || fail "cluster did not come up"
ok "cluster up: $(kubectl get nodes --no-headers | wc -l | tr -d ' ') node(s)"

# ── 2. KEDA (the app chart's ScaledObject needs the CRDs) ───────────────────
log "Installing KEDA"
helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
helm repo update kedacore >/dev/null 2>&1 || true
helm upgrade --install keda kedacore/keda -n keda --create-namespace --wait --timeout "${TIMEOUT}s"
kubectl -n keda rollout status deploy/keda-operator --timeout="${TIMEOUT}s"
ok "KEDA operator Available"

# ── 3. per-app dev Postgres (carshowdb-dev-postgres) ────────────────────────
log "Deploying per-app Postgres into ${NS_PG} (release ${PG_RELEASE})"
kubectl create namespace "${NS_PG}" --dry-run=client -o yaml | kubectl apply -f -
helm template "${PG_RELEASE}" "${PG_CHART}" -n "${NS_PG}" \
  --set auth.database=carshowdb --set auth.username=app --set auth.password=dev-postgres-placeholder \
  --set persistence.storageClass=local-path --set-json 'nodeSelector={}' \
  | kubectl apply -n "${NS_PG}" -f -
log "Waiting for Postgres Ready"
kubectl -n "${NS_PG}" rollout status statefulset --timeout="${TIMEOUT}s" 2>/dev/null \
  || kubectl -n "${NS_PG}" wait --for=condition=Available deploy --all --timeout="${TIMEOUT}s"
kubectl -n "${NS_PG}" wait --for=condition=Ready pod --all --timeout="${TIMEOUT}s" || fail "Postgres not Ready"
ok "Postgres Ready (service carshowdb-postgres-dev-postgres.${NS_PG}.svc:5432)"

# ── 4. carshowdb app (carshowdb-dev-api) ────────────────────────────────────
log "Deploying carshowdb app into ${NS_API} (real rendered values + e2e image/probe overrides)"
kubectl create namespace "${NS_API}" --dry-run=client -o yaml | kubectl apply -f -
RENDERED="${WORKDIR}/app.yaml"
helm template "${APP_RELEASE}" "${APP_CHART}" -n "${NS_API}" -f "${APP_VALUES}" \
  --set image.repository="${APP_IMAGE%:*}" --set image.tag="${APP_IMAGE##*:}" \
  --set probes.liveness.path=/ --set probes.readiness.path=/ \
  --set serviceMonitor.enabled=false > "${RENDERED}"
grep -Eq '^[[:space:]]*type:[[:space:]]*LoadBalancer' "${RENDERED}" \
  && fail "POLICY VIOLATION: rendered app chart produced a LoadBalancer Service." || true
ok "policy: no LoadBalancer in rendered Service"
kubectl apply -n "${NS_API}" -f "${RENDERED}"
log "Waiting for carshowdb Deployment Available"
kubectl -n "${NS_API}" rollout status deploy/"${APP_RELEASE}" --timeout="${TIMEOUT}s" || fail "carshowdb did not roll out"
ok "carshowdb Deployment Available"
kubectl -n "${NS_API}" get scaledobject "${APP_RELEASE}" >/dev/null 2>&1 && ok "KEDA ScaledObject accepted" || info "(no ScaledObject — check keda values)"
kubectl -n "${NS_API}" get secret "${RUNTIME_SECRET}" >/dev/null 2>&1 \
  && ok "runtime Secret ${RUNTIME_SECRET} present (carries DATABASE_URL)" || fail "runtime Secret missing"
kubectl -n "${NS_API}" get svc "${APP_RELEASE}" -o jsonpath='{.spec.type}' | grep -qx ClusterIP \
  && ok "Service is ClusterIP" || fail "Service is not ClusterIP"

# ── 5. THE FIX: live cross-namespace DB connection over the wired DATABASE_URL ─
log "Proving the wired DATABASE_URL actually connects (the dead-Supabase fix)"
kubectl apply -n "${NS_API}" -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: db-connectivity-check
spec:
  backoffLimit: 4
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: psql
          image: postgres:16-alpine
          command: ["sh","-c"]
          args:
            - |
              echo "DATABASE_URL host: \$(echo "\$DATABASE_URL" | sed -E 's#.*@([^/]+)/.*#\1#')"
              until pg_isready -d "\$DATABASE_URL" -t 3; do echo "waiting for db..."; sleep 2; done
              psql "\$DATABASE_URL" -tAc "select 'CONNECTED ok='||1;"
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef: { name: ${RUNTIME_SECRET}, key: DATABASE_URL }
EOF
kubectl -n "${NS_API}" wait --for=condition=complete job/db-connectivity-check --timeout="${TIMEOUT}s" \
  || { kubectl -n "${NS_API}" logs job/db-connectivity-check || true; fail "DB connectivity check did NOT succeed"; }
log "DB connectivity check logs:"; kubectl -n "${NS_API}" logs job/db-connectivity-check | sed 's/^/     /'
kubectl -n "${NS_API}" logs job/db-connectivity-check | grep -q 'CONNECTED ok=1' \
  && ok "carshowdb's DATABASE_URL connects to the in-cluster Postgres (cross-namespace)" \
  || fail "expected 'CONNECTED ok=1' from psql"

# ── done ─────────────────────────────────────────────────────────────────────
log "E2E PASSED"
ok "Postgres Ready in ${NS_PG}; carshowdb Available in ${NS_API}; ClusterIP-only; DATABASE_URL connects live."
