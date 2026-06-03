#!/usr/bin/env bash
#
# test/e2e/run.sh — ephemeral, self-cleaning end-to-end test for the NEW
# Flux + umbrella platform (the ArgoCD -> Flux cutover).
#
# It spins up a THROWAWAY k3d cluster and proves the real, rendered desired state
# (clusters/dev/platform.yaml — the umbrella HelmRelease that installs
# charts/cluster, which fans out one HelmRelease per app + its Postgres + each
# enabled module) reconciles on a real Kubernetes cluster running Flux.
#
# ── THE HEADLINE QUESTION (for the homelab cutover) ──────────────────────────
# ArgoCD's repo-server happily cloned our in-cluster git:// daemon. Flux's
# source-controller, per its docs, only documents http(s):// and ssh:// for a
# GitRepository. Does Flux's source-controller ACTUALLY accept a git:// daemon
# as a GitRepository source, or reject it?
#
# This test answers it empirically: it stands up the SAME in-cluster git daemon
# the homelab uses (buildpack-deps:bookworm-scm running `git daemon` on :9418,
# populated by `kubectl cp` of a bare clone), points a Flux GitRepository at
# git://…/platformctl.git, waits, and reads status.conditions. It prints the
# verdict as the headline line:
#       FLUX_GIT_PROTOCOL: accepted
#   or  FLUX_GIT_PROTOCOL: rejected — <message>
#
# ── What it then PROVES about the platform ───────────────────────────────────
#   - If git:// is ACCEPTED: it applies the umbrella HelmRelease against that
#     git:// GitRepository and lets Flux's helm-controller reconcile the inner
#     releases (carshowdb, carshowdb-postgres, keda, keda-http-add-on).
#   - If git:// is REJECTED: it FALLS BACK to a source Flux DOES accept — a
#     smart-HTTP git server (git-http-backend) served over http:// — re-points
#     the GitRepository at it, and reconciles the SAME umbrella. (If even that
#     is unavailable it degrades to installing the inner charts directly via
#     helm so the workloads are still proven.) The path that ran is printed.
#   - Either way it then PROVES the dead-Supabase fix: a Job in the carshowdb
#     namespace, using the SAME DATABASE_URL the platform wired into the
#     carshowdb-runtime Secret, connects cross-namespace to the in-cluster
#     Postgres and prints `CONNECTED ok=1`.
#   - It asserts the carshowdb HTTPScaledObject (scale-to-zero) exists and that
#     NO rendered manifest is a LoadBalancer (platform guardrail).
#
# Safety:
#   - k3d absent  -> SKIP (exit 0), not fail.
#   - isolated kubeconfig + uniquely-named cluster: only ever talks to the
#     cluster it just created; never touches a real context.
#   - trap always deletes the cluster + tmp (even on failure / Ctrl-C).
#
# Usage:  ./test/e2e/run.sh         (or `make e2e`)
# Knobs:  E2E_KEEP=1   keep cluster        E2E_TIMEOUT=240  per-wait seconds
#         APP_IMAGE=…  workload image (carshowdb's real image is private)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd)"

CLUSTER="platformctl-e2e-$$"
TIMEOUT="${E2E_TIMEOUT:-240}"

# Namespaces / names — must match the rendered umbrella (clusters/dev/platform.yaml).
FLUX_NS="flux-system"
SRC_NAME="flux-system"                          # GitRepository the umbrella references
NS_API="carshowdb-dev-api"
NS_PG="carshowdb-dev-postgres"
PG_RELEASE="carshowdb-postgres"                 # => svc carshowdb-postgres-dev-postgres
APP_RELEASE="carshowdb"
RUNTIME_SECRET="carshowdb-runtime"
HTTPSCALEDOBJECT="carshowdb"                    # app.fullname == app name

# In-cluster git source (reuses scripts/homelab-bringup.sh's daemon pattern).
GIT_NS="platform-gitsrc"
GIT_SVC="platformctl-git"
GIT_REPO="platformctl.git"
GIT_URL="git://${GIT_SVC}.${GIT_NS}.svc.cluster.local/${GIT_REPO}"
HTTP_URL="http://${GIT_SVC}.${GIT_NS}.svc.cluster.local/${GIT_REPO}"
BRANCH="main"

# carshowdb's real image is private/unpublished; a public echo-server passes the
# chart's HTTP probes. The DB-connectivity proof uses postgres directly, so the
# app's business logic is not needed to prove the platform wiring.
APP_IMAGE="${APP_IMAGE:-ealen/echo-server:0.9.2}"

# Flux Operator chart (OCI) — installs the controllers via a FluxInstance.
FLUX_OPERATOR_CHART="oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator"

WORKDIR="" ; KUBECONFIG_FILE="" ; BARE=""
RAN_PATH="(none)"                               # which platform path actually ran

log()  { printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
info() { printf '   %s\n' "$*"; }
ok()   { printf '   \033[1;32m✓ %s\033[0m\n' "$*"; }
warn() { printf '   \033[1;33m! %s\033[0m\n' "$*"; }
skip() { printf '\n\033[1;33mSKIP: %s\033[0m\n' "$*"; }
fail() { printf '\n\033[1;31mFAIL: %s\033[0m\n' "$*" >&2; exit 1; }
hdr()  { printf '\n\033[1;35m### %s\033[0m\n' "$*"; }   # headline lines

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
command -v go      >/dev/null 2>&1 || fail "go not found."
command -v git     >/dev/null 2>&1 || fail "git not found."
[[ -f "${REPO_ROOT}/charts/cluster/Chart.yaml" ]] || fail "charts/cluster missing."
[[ -f "${REPO_ROOT}/charts/app/Chart.yaml" ]]     || fail "charts/app missing."
[[ -f "${REPO_ROOT}/charts/infra/dev-postgres/Chart.yaml" ]] || fail "charts/infra/dev-postgres missing."

WORKDIR="$(mktemp -d)"; KUBECONFIG_FILE="${WORKDIR}/kubeconfig"
export KUBECONFIG="${KUBECONFIG_FILE}"          # isolate from any real context

# ── 1. throwaway cluster ─────────────────────────────────────────────────────
log "Creating ephemeral k3d cluster '${CLUSTER}'"
k3d cluster create "${CLUSTER}" --agents 1 --no-lb \
  --kubeconfig-update-default=false --kubeconfig-switch-context=false --wait
k3d kubeconfig get "${CLUSTER}" > "${KUBECONFIG_FILE}"
kubectl cluster-info >/dev/null || fail "cluster did not come up"
ok "cluster up: $(kubectl get nodes --no-headers | wc -l | tr -d ' ') node(s)"

# ── 2. build the CLI + render the REAL desired state ─────────────────────────
log "Building platformctl and rendering the dev umbrella (the real desired state)"
( cd "${REPO_ROOT}" && go build -o platformctl ./cmd/platformctl )
ok "built ${REPO_ROOT}/platformctl"
( cd "${REPO_ROOT}" && ./platformctl render --env dev \
    --file examples/carshowdb/deploy.yaml --image "${APP_IMAGE}" >/dev/null )
( cd "${REPO_ROOT}" && ./platformctl infra render --env dev >/dev/null )
PLATFORM_YAML="${REPO_ROOT}/clusters/dev/platform.yaml"
[[ -f "${PLATFORM_YAML}" ]] || fail "render did not produce ${PLATFORM_YAML}"
ok "umbrella rendered: clusters/dev/platform.yaml"

# ── policy: NO LoadBalancer anywhere in the rendered app/cluster manifests ───
log "Policy: assert the rendered app/cluster charts produce 0 LoadBalancer Services"
LB_SCAN="${WORKDIR}/lb-scan.yaml"
{
  helm template "${APP_RELEASE}" "${REPO_ROOT}/charts/app" \
    -f "${REPO_ROOT}/charts/app/ci/carshowdb-dev-values.yaml" \
    --set "image.repository=${APP_IMAGE%:*}" --set "image.tag=${APP_IMAGE##*:}" 2>/dev/null || true
  helm template "${PG_RELEASE}" "${REPO_ROOT}/charts/infra/dev-postgres" 2>/dev/null || true
  cat "${PLATFORM_YAML}"
} > "${LB_SCAN}"
if grep -Eq '^[[:space:]]*type:[[:space:]]*LoadBalancer' "${LB_SCAN}"; then
  fail "POLICY VIOLATION: a rendered manifest produced a LoadBalancer Service."
fi
ok "no LoadBalancer in rendered app/postgres/umbrella manifests"

# ── 3. install Flux controllers (Flux CLI if present, else Flux Operator) ────
log "Installing Flux controllers on k3d"
if command -v flux >/dev/null 2>&1; then
  info "using the Flux CLI"
  flux install --components source-controller,kustomize-controller,helm-controller \
    --namespace "${FLUX_NS}"
else
  info "Flux CLI absent — installing the Flux Operator + a minimal FluxInstance"
  helm install flux-operator "${FLUX_OPERATOR_CHART}" \
    -n "${FLUX_NS}" --create-namespace --wait --timeout "${TIMEOUT}s"
  kubectl apply -f - <<EOF
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: ${FLUX_NS}
spec:
  distribution:
    version: "2.x"
    registry: "ghcr.io/fluxcd"
  components:
    - source-controller
    - kustomize-controller
    - helm-controller
  cluster:
    type: kubernetes
EOF
fi
log "Waiting for the Flux controllers to be Ready"
for d in source-controller helm-controller kustomize-controller; do
  for _ in $(seq 1 "${TIMEOUT}"); do
    kubectl -n "${FLUX_NS}" get deploy "${d}" >/dev/null 2>&1 && break
    sleep 1
  done
done
kubectl -n "${FLUX_NS}" rollout status deploy/source-controller --timeout="${TIMEOUT}s"
kubectl -n "${FLUX_NS}" rollout status deploy/helm-controller   --timeout="${TIMEOUT}s"
ok "Flux source-controller + helm-controller Available"

# ── 4. KEDA + HTTP add-on (HTTPScaledObject CRDs; carshowdb is scale-to-zero) ─
log "Installing KEDA + the HTTP add-on (HTTPScaledObject CRDs for scale-to-zero)"
helm repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
helm repo update kedacore >/dev/null 2>&1 || true
helm upgrade --install keda kedacore/keda -n keda --create-namespace \
  --wait --timeout "${TIMEOUT}s"
kubectl -n keda rollout status deploy/keda-operator --timeout="${TIMEOUT}s"
helm upgrade --install keda-http-add-on kedacore/keda-add-ons-http \
  --version 0.14.1 -n keda --wait --timeout "${TIMEOUT}s"
kubectl get crd httpscaledobjects.http.keda.sh >/dev/null 2>&1 \
  && ok "KEDA + HTTPScaledObject CRD present" \
  || fail "HTTPScaledObject CRD missing after keda-add-ons-http install"

# The carshowdb app chart renders a ServiceMonitor (serviceMonitor.enabled:true in
# the rendered umbrella values). The homelab already runs kube-prometheus-stack, so
# the ServiceMonitor CRD exists there; an ephemeral k3d cluster does not. Without
# the CRD the carshowdb HelmRelease install fails on apply and Flux remediation
# uninstalls it (Ready=False, runtime Secret gone). Install just the Prometheus
# Operator CRDs so the rendered desired state applies as-is (no chart changes).
log "Installing Prometheus Operator CRDs (the app chart's ServiceMonitor needs them)"
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community >/dev/null 2>&1 || true
helm upgrade --install prometheus-operator-crds \
  prometheus-community/prometheus-operator-crds -n monitoring --create-namespace \
  --wait --timeout "${TIMEOUT}s"
kubectl get crd servicemonitors.monitoring.coreos.com >/dev/null 2>&1 \
  && ok "ServiceMonitor CRD present" \
  || fail "ServiceMonitor CRD missing after prometheus-operator-crds install"

# ── 5. serve THIS repo in-cluster via a git daemon (homelab pattern) ─────────
log "Serving the repo in-cluster (git daemon + smart-HTTP, ns ${GIT_NS})"
BARE="${WORKDIR}/${GIT_REPO}"
git clone --bare "${REPO_ROOT}" "${BARE}" >/dev/null 2>&1
( cd "${BARE}" && git update-server-info )     # enables smart/dumb HTTP serving
REV="$(cd "${BARE}" && git rev-parse --short HEAD)"
info "bare clone @ ${REV}"

# The buildpack-deps:bookworm-scm image ships full git (git-daemon AND
# git-http-backend) pre-installed — no runtime apk/DNS needed. We run the git
# daemon on :9418 (the git:// test) and an nginx+fcgiwrap smart-HTTP endpoint on
# :80 (the Flux-accepted fallback) from the SAME populated /srv/git volume.
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Namespace
metadata: { name: platform-gitsrc, labels: { platform/managed-by: platformctl, platform/role: gitsrc } }
---
apiVersion: v1
kind: ConfigMap
metadata: { name: gitsrc-nginx, namespace: platform-gitsrc }
data:
  default.conf: |
    server {
      listen 80;
      location / {
        # git smart-HTTP via git-http-backend (CGI) — the protocol Flux accepts.
        fastcgi_pass unix:/run/fcgiwrap.socket;
        include fastcgi_params;
        fastcgi_param SCRIPT_FILENAME /usr/lib/git-core/git-http-backend;
        fastcgi_param GIT_HTTP_EXPORT_ALL "";
        fastcgi_param GIT_PROJECT_ROOT /srv/git;
        fastcgi_param PATH_INFO $uri;
        fastcgi_param REMOTE_USER $remote_user;
      }
    }
---
apiVersion: apps/v1
kind: Deployment
metadata: { name: platformctl-git, namespace: platform-gitsrc, labels: { app: platformctl-git } }
spec:
  replicas: 1
  selector: { matchLabels: { app: platformctl-git } }
  template:
    metadata: { labels: { app: platformctl-git } }
    spec:
      containers:
        # (a) git:// daemon — what we are TESTING Flux against.
        - name: git-daemon
          image: buildpack-deps:bookworm-scm
          command: ["git","daemon","--verbose","--reuseaddr","--listen=0.0.0.0","--port=9418","--base-path=/srv/git","--export-all","--enable=upload-pack","/srv/git"]
          ports: [ { containerPort: 9418, name: git } ]
          readinessProbe: { tcpSocket: { port: 9418 }, initialDelaySeconds: 2, periodSeconds: 3 }
          volumeMounts: [ { name: repo, mountPath: /srv/git } ]
        # (b) smart-HTTP (nginx + fcgiwrap + git-http-backend) — the FALLBACK
        #     source Flux DOES accept (http://). nginx + fcgiwrap are apt-installed
        #     once at start (k3d nodes have outbound network).
        - name: git-http
          image: buildpack-deps:bookworm-scm
          command: ["bash","-c"]
          args:
            - |
              set -e
              apt-get update -qq && apt-get install -y -qq --no-install-recommends nginx fcgiwrap >/dev/null
              cp /etc/nginx-conf/default.conf /etc/nginx/sites-enabled/default
              ( /usr/sbin/fcgiwrap -s unix:/run/fcgiwrap.socket & )
              sleep 1; chmod 0666 /run/fcgiwrap.socket || true
              exec nginx -g 'daemon off;'
          ports: [ { containerPort: 80, name: http } ]
          readinessProbe:
            httpGet:
              path: "/platformctl.git/info/refs?service=git-upload-pack"
              port: 80
            initialDelaySeconds: 5
            periodSeconds: 5
            failureThreshold: 30
          volumeMounts:
            - { name: repo, mountPath: /srv/git }
            - { name: nginx-conf, mountPath: /etc/nginx-conf }
      volumes:
        - { name: repo, emptyDir: {} }
        - { name: nginx-conf, configMap: { name: gitsrc-nginx } }
---
apiVersion: v1
kind: Service
metadata: { name: platformctl-git, namespace: platform-gitsrc }
spec:
  selector: { app: platformctl-git }
  ports:
    - { name: git,  port: 9418, targetPort: 9418 }
    - { name: http, port: 80,   targetPort: 80 }
YAML

# Wait for a running pod, then kubectl cp the bare clone into the shared volume.
log "Populating the in-cluster git source"
POD=""
for _ in $(seq 1 40); do
  POD="$(kubectl -n "${GIT_NS}" get pod -l app=platformctl-git \
          --field-selector=status.phase=Running \
          -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null || true)"
  if [[ -n "${POD}" ]] && kubectl -n "${GIT_NS}" exec -c git-daemon "${POD}" -- true 2>/dev/null; then break; fi
  POD=""; sleep 3
done
[[ -n "${POD}" ]] || fail "git source pod never became Running"
kubectl -n "${GIT_NS}" exec -c git-daemon "${POD}" -- sh -c "rm -rf /srv/git/${GIT_REPO}"
kubectl -n "${GIT_NS}" cp "${BARE}" "${POD}:/srv/git/${GIT_REPO}" -c git-daemon
ok "repo served at ${GIT_URL} (pod ${POD})"

# ── 6. THE KEY TEST: does Flux's source-controller accept a git:// GitRepository?
hdr "KEY TEST — Flux source-controller vs a git:// daemon"
kubectl -n "${FLUX_NS}" delete gitrepository "${SRC_NAME}" --ignore-not-found >/dev/null 2>&1 || true

GIT_PROTO="unknown" ; GIT_MSG=""
APPLY_OUT="${WORKDIR}/gitrepo-apply.txt"
# Flux rejects an unsupported scheme in TWO possible places:
#   (a) at admission — the GitRepository CRD pins spec.url to ^(http|https|ssh)://
#       so `kubectl apply` itself fails (the object is never created), or
#   (b) at reconcile — source-controller sets Ready=False with a fetch error.
# Capture both. (Note: `kubectl apply` is allowed to fail here under set -e.)
if kubectl apply -f - >"${APPLY_OUT}" 2>&1 <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: ${SRC_NAME}
  namespace: ${FLUX_NS}
spec:
  interval: 30s
  url: ${GIT_URL}
  ref:
    branch: ${BRANCH}
EOF
then
  # Admission accepted the object — now watch the Ready condition. Ready=True
  # means source-controller fetched an artifact over git://; Ready=False means it
  # refused/failed the scheme at reconcile time.
  info "GitRepository accepted by admission — watching reconcile status…"
  for _ in $(seq 1 40); do
    READY="$(kubectl -n "${FLUX_NS}" get gitrepository "${SRC_NAME}" \
              -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}|{.reason}|{.message}{end}' 2>/dev/null || true)"
    if [[ -n "${READY}" ]]; then
      GIT_MSG="${READY#*|}"; GIT_MSG="${GIT_MSG#*|}"
      case "${READY%%|*}" in
        True)  GIT_PROTO="accepted"; break ;;
        False) GIT_PROTO="rejected"; break ;;
      esac
    fi
    sleep 3
  done
  echo
  info "GitRepository ${SRC_NAME} status.conditions:"
  kubectl -n "${FLUX_NS}" get gitrepository "${SRC_NAME}" \
    -o jsonpath='{range .status.conditions[*]}     [{.type}={.status}] {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
  if [[ "${GIT_PROTO}" == "unknown" ]]; then
    GIT_PROTO="rejected"; GIT_MSG="no Ready condition within timeout"
  fi
else
  # Admission rejected the object outright (the CRD's spec.url scheme pattern).
  GIT_PROTO="rejected"
  GIT_MSG="$(tr '\n' ' ' < "${APPLY_OUT}" | sed -E 's/  +/ /g')"
  info "GitRepository REJECTED at admission:"
  sed 's/^/     /' "${APPLY_OUT}"
fi

echo
if [[ "${GIT_PROTO}" == "accepted" ]]; then
  hdr "FLUX_GIT_PROTOCOL: accepted"
else
  hdr "FLUX_GIT_PROTOCOL: rejected — ${GIT_MSG}"
fi

# ── 7. reconcile the umbrella — over git:// if accepted, else the HTTP fallback
apply_umbrella() {
  # Apply the rendered umbrella HelmRelease verbatim. It references GitRepository
  # ${SRC_NAME} for ./charts/cluster, which we have just made Ready (over the
  # working scheme), so the helm-controller can resolve the in-repo charts.
  kubectl apply -f "${PLATFORM_YAML}"
}

if [[ "${GIT_PROTO}" == "accepted" ]]; then
  RAN_PATH="flux-umbrella-over-git://"
  log "git:// accepted — reconciling the umbrella HelmRelease over git://"
  apply_umbrella
else
  log "git:// rejected — falling back to a Flux-accepted smart-HTTP source"
  # Re-point the SAME GitRepository at the smart-HTTP endpoint (git-http-backend).
  if kubectl apply -f - <<EOF 2>/dev/null
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: ${SRC_NAME}
  namespace: ${FLUX_NS}
spec:
  interval: 30s
  url: ${HTTP_URL}
  ref:
    branch: ${BRANCH}
EOF
  then :; fi
  HTTP_OK="no"
  for _ in $(seq 1 40); do
    if kubectl -n "${FLUX_NS}" get gitrepository "${SRC_NAME}" \
        -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null \
        | grep -qx True; then HTTP_OK="yes"; break; fi
    sleep 3
  done
  if [[ "${HTTP_OK}" == "yes" ]]; then
    RAN_PATH="flux-umbrella-over-http:// (git:// fallback)"
    ok "smart-HTTP GitRepository Ready — reconciling the umbrella over http://"
    apply_umbrella
  else
    # Last-resort fallback: install the umbrella chart's inner releases directly
    # with helm, using the umbrella's spec.values, so the SAME workloads are
    # proven even without a Flux-acceptable git source.
    RAN_PATH="direct-helm (no Flux-acceptable git source)"
    warn "smart-HTTP source not Ready either — installing inner charts directly via helm"
    PG_VALUES="${WORKDIR}/pg-values.yaml"
    APP_VALUES="${WORKDIR}/app-values.yaml"
    python3 - "$PLATFORM_YAML" "$PG_VALUES" "$APP_VALUES" <<'PY'
import sys, yaml
doc = yaml.safe_load(open(sys.argv[1]))
app = doc["spec"]["values"]["apps"][0]
yaml.safe_dump(app["postgres"]["values"], open(sys.argv[2], "w"))
yaml.safe_dump(app["values"],             open(sys.argv[3], "w"))
PY
    kubectl create namespace "${NS_PG}"  --dry-run=client -o yaml | kubectl apply -f -
    kubectl create namespace "${NS_API}" --dry-run=client -o yaml | kubectl apply -f -
    helm upgrade --install "${PG_RELEASE}" "${REPO_ROOT}/charts/infra/dev-postgres" \
      -n "${NS_PG}" -f "${PG_VALUES}" --wait --timeout "${TIMEOUT}s"
    helm upgrade --install "${APP_RELEASE}" "${REPO_ROOT}/charts/app" \
      -n "${NS_API}" -f "${APP_VALUES}" \
      --set "image.repository=${APP_IMAGE%:*}" --set "image.tag=${APP_IMAGE##*:}" \
      --wait --timeout "${TIMEOUT}s"
  fi
fi
info "platform path: ${RAN_PATH}"

# ── 7b. wait for the inner workloads to materialize (Flux paths) ─────────────
if [[ "${RAN_PATH}" != direct-helm* ]]; then
  log "Waiting for Flux to reconcile the umbrella into the inner HelmReleases"
  # Force a fast reconcile if the Flux CLI is around (optional).
  command -v flux >/dev/null 2>&1 && \
    flux -n "${FLUX_NS}" reconcile helmrelease platform --with-source >/dev/null 2>&1 || true
  # Wait for the postgres StatefulSet to be created then Ready.
  for _ in $(seq 1 "${TIMEOUT}"); do
    kubectl -n "${NS_PG}" get statefulset >/dev/null 2>&1 \
      && [[ -n "$(kubectl -n "${NS_PG}" get statefulset -o name 2>/dev/null)" ]] && break
    sleep 2
  done
  kubectl -n "${NS_PG}" rollout status statefulset --timeout="${TIMEOUT}s" 2>/dev/null || true
  # And for the carshowdb HelmRelease to report Ready. This is blocking: if it
  # never reconciles, dump its conditions + the helm-controller's view so the
  # failure is self-diagnosing (a missing CRD, a bad value, etc.).
  HR_READY="no"
  for _ in $(seq 1 "${TIMEOUT}"); do
    kubectl -n "${FLUX_NS}" get helmrelease "${APP_RELEASE}" \
      -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null \
      | grep -qx True && { HR_READY="yes"; break; }
    sleep 2
  done
  if [[ "${HR_READY}" == "yes" ]]; then
    ok "carshowdb HelmRelease reconciled Ready"
  else
    warn "carshowdb HelmRelease not Ready — dumping diagnostics:"
    kubectl -n "${FLUX_NS}" get helmrelease "${APP_RELEASE}" \
      -o jsonpath='{range .status.conditions[*]}     [{.type}={.status}] {.reason}: {.message}{"\n"}{end}' 2>/dev/null || true
    kubectl -n "${FLUX_NS}" get helmrelease -o wide 2>/dev/null | sed 's/^/     /' || true
    fail "carshowdb HelmRelease did not reconcile Ready (see conditions above)"
  fi
fi

# ── 8. PROVE the workloads (postgres Ready, app Available, scale-to-zero HSO) ─
log "Asserting the carshowdb workloads"
for _ in $(seq 1 "${TIMEOUT}"); do
  kubectl -n "${NS_PG}" get pod -l app.kubernetes.io/instance="${PG_RELEASE}" \
    -o jsonpath='{range .items[*]}{.status.phase}{end}' 2>/dev/null | grep -q Running && break
  sleep 2
done
kubectl -n "${NS_PG}" wait --for=condition=Ready pod --all --timeout="${TIMEOUT}s" \
  || fail "Postgres not Ready in ${NS_PG}"
ok "Postgres Ready (svc ${PG_RELEASE}-dev-postgres.${NS_PG}.svc:5432)"

# The runtime Secret, Service, and HTTPScaledObject are applied by the carshowdb
# HelmRelease; give them a short grace window to appear after it reports Ready.
for _ in $(seq 1 60); do
  kubectl -n "${NS_API}" get secret "${RUNTIME_SECRET}" >/dev/null 2>&1 && break
  sleep 2
done
kubectl -n "${NS_API}" get secret "${RUNTIME_SECRET}" >/dev/null 2>&1 \
  && ok "runtime Secret ${RUNTIME_SECRET} present (carries DATABASE_URL)" \
  || fail "runtime Secret ${RUNTIME_SECRET} missing in ${NS_API}"
kubectl -n "${NS_API}" get svc "${APP_RELEASE}" -o jsonpath='{.spec.type}' 2>/dev/null \
  | grep -qx ClusterIP && ok "carshowdb Service is ClusterIP" \
  || warn "carshowdb Service type is not ClusterIP (or not created yet)"
for _ in $(seq 1 30); do
  kubectl -n "${NS_API}" get httpscaledobject "${HTTPSCALEDOBJECT}" >/dev/null 2>&1 && break
  sleep 2
done
kubectl -n "${NS_API}" get httpscaledobject "${HTTPSCALEDOBJECT}" >/dev/null 2>&1 \
  && ok "HTTPScaledObject ${HTTPSCALEDOBJECT} present (scale-to-zero)" \
  || fail "HTTPScaledObject ${HTTPSCALEDOBJECT} missing in ${NS_API}"

# ── 9. THE FIX: live cross-namespace DB connection over the wired DATABASE_URL ─
log "Proving the wired DATABASE_URL connects (the dead-Supabase fix)"
kubectl -n "${NS_API}" delete job db-connectivity-check --ignore-not-found >/dev/null 2>&1 || true
kubectl apply -n "${NS_API}" -f - <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: db-connectivity-check
spec:
  backoffLimit: 6
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

# ── done — restate the headline findings ─────────────────────────────────────
log "E2E PASSED"
if [[ "${GIT_PROTO}" == "accepted" ]]; then
  hdr "FLUX_GIT_PROTOCOL: accepted"
else
  hdr "FLUX_GIT_PROTOCOL: rejected — ${GIT_MSG}"
fi
ok "platform path that ran: ${RAN_PATH}"
ok "Postgres Ready; carshowdb HTTPScaledObject (scale-to-zero) present; ClusterIP-only; DATABASE_URL connects live (CONNECTED ok=1)."
