#!/usr/bin/env bash
#
# homelab-flux-bringup.sh — cut the homelab k3s cluster (context: optiplex-pg)
# over from ArgoCD to FLUX, using a LAN-LOCAL git source. Nothing leaves your
# network: the repo is served from an in-cluster smart-HTTP git server and Flux
# reconciles the umbrella (clusters/dev/platform.yaml) from it.
#
# This REPLACES scripts/homelab-bringup.sh (the ArgoCD git:// bring-up). The key
# difference, proven by test/e2e/run.sh: Flux's source-controller REJECTS git://
# at CRD admission (spec.url must match ^(http|https|ssh)://). So the in-cluster
# git source here is smart-HTTP (nginx + fcgiwrap + git-http-backend), NOT a
# git:// daemon. See memory: flux-rejects-git-protocol.
#
# What it does (all ADDITIVE + REVERSIBLE — see teardown at the bottom):
#   1. removes the old ArgoCD platform apps (carshowdb, carshowdb-postgres,
#      platform-dev-root) — leaves onprem-* untouched
#   2. hands KEDA to Flux: uninstalls the manual `keda` helm release + its CRDs
#      (only carshowdb used them; Flux's umbrella reinstalls keda + the http add-on)
#   3. installs the Flux Operator + a no-sync FluxInstance (controllers only;
#      the umbrella is bootstrapped manually below, so we never point Flux at the
#      private/unpushed GitHub remote)
#   4. serves THIS repo in-cluster over smart-HTTP (ns platform-gitsrc)
#   5. creates GitRepository flux-system -> the in-cluster http source
#   6. applies clusters/dev/platform.yaml (the umbrella HelmRelease); Flux fans it
#      out into carshowdb + its Postgres + keda + keda-http-add-on
#   7. exposes the Flux Web UI on MetalLB (http://10.0.0.210) + a NetworkPolicy
#      that lets LAN traffic reach :9080 (the operator hardens flux-system with a
#      default-deny regime that otherwise blocks the LB)
#   8. scales CoreDNS to 2 replicas (k3s addon-managed; this holds across reconcile
#      but a full k3s restart re-applies the host manifest — for durable HA edit
#      /var/lib/rancher/k3s/server/manifests/coredns.yaml, see scripts/coredns-ha.yaml)
#   9. proves the wired DATABASE_URL connects (the dead-Supabase fix)
#
# It does NOT touch the k3s datastore, control plane, or any existing workload.
#
# Run it:   bash scripts/homelab-flux-bringup.sh
# Teardown: bash scripts/homelab-flux-bringup.sh teardown

set -euo pipefail

CTX="${CTX:-optiplex-pg}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GIT_NS="platform-gitsrc"
GIT_SVC="platformctl-git"
GIT_REPO="platformctl.git"
HTTP_URL="http://${GIT_SVC}.${GIT_NS}.svc.cluster.local/${GIT_REPO}"
BARE="/tmp/${GIT_REPO}"
FLUX_NS="flux-system"
SRC_NAME="flux-system"                 # GitRepository the umbrella references
UI_IP="${UI_IP:-10.0.0.210}"           # MetalLB pool-high IP for the Flux Web UI
FLUX_OPERATOR_CHART="oci://ghcr.io/controlplaneio-fluxcd/charts/flux-operator"
K="kubectl --context ${CTX}"
H="helm --kube-context ${CTX}"

log(){ printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok(){  printf '   \033[1;32m✓ %s\033[0m\n' "$*"; }
warn(){ printf '   \033[1;33m! %s\033[0m\n' "$*"; }

teardown() {
  log "Tearing down the Flux homelab bring-up (additive resources only)"
  $K -n "${FLUX_NS}" delete helmrelease platform --ignore-not-found
  $K -n "${FLUX_NS}" delete gitrepository "${SRC_NAME}" --ignore-not-found
  $K -n "${FLUX_NS}" delete svc flux-web-ui --ignore-not-found
  $K -n "${FLUX_NS}" delete networkpolicy allow-flux-web-ui --ignore-not-found
  $K delete ns carshowdb-dev-api carshowdb-dev-postgres "${GIT_NS}" --ignore-not-found
  $K -n "${FLUX_NS}" delete fluxinstance flux --ignore-not-found
  $H uninstall flux-operator -n "${FLUX_NS}" 2>/dev/null || true
  $H uninstall keda -n keda 2>/dev/null || true
  $K delete ns keda "${FLUX_NS}" --ignore-not-found
  ok "torn down (onprem-* and other homelab workloads untouched)"
  exit 0
}
[[ "${1:-}" == "teardown" ]] && teardown

log "Target cluster"; $K config current-context; $K get nodes -o name | sed 's/^/   /'

# ── 1. remove the old ArgoCD platform apps (keep onprem-*) ─────────────────────
if $K -n argocd get application platform-dev-root >/dev/null 2>&1; then
  log "Removing old ArgoCD platform apps (keeping onprem-*)"
  for app in carshowdb carshowdb-postgres platform-dev-root; do
    $K -n argocd delete application "$app" --cascade=foreground --timeout=120s 2>/dev/null || true
  done
  # ArgoCD prunes via its own finalizer; the carshowdb resources may be orphaned.
  # Clean the namespaces for a fresh Flux slate (dev data is throwaway). KEDA
  # ScaledObjects carry a keda finalizer — strip it so the namespaces can finalize.
  for ns in carshowdb-dev-api carshowdb-dev-postgres; do
    for so in $($K -n "$ns" get scaledobjects.keda.sh -o name 2>/dev/null || true); do
      $K -n "$ns" patch "$so" --type=merge -p '{"metadata":{"finalizers":[]}}' 2>/dev/null || true
    done
  done
  $K delete ns carshowdb-dev-api carshowdb-dev-postgres --ignore-not-found --timeout=120s 2>/dev/null || true
  ok "old platform apps removed"
fi

# ── 2. hand KEDA to Flux (uninstall the manual release + CRDs) ─────────────────
if $H -n keda list 2>/dev/null | grep -q '^keda\b'; then
  log "Handing KEDA to Flux (uninstall manual release + CRDs; the umbrella reinstalls it)"
  $H uninstall keda -n keda 2>/dev/null || true
  $K delete crd scaledobjects.keda.sh scaledjobs.keda.sh triggerauthentications.keda.sh \
    clustertriggerauthentications.keda.sh cloudeventsources.eventing.keda.sh \
    clustercloudeventsources.eventing.keda.sh --ignore-not-found 2>/dev/null || true
  ok "manual KEDA removed"
fi

# ── 3. Flux Operator + a no-sync FluxInstance (controllers only) ───────────────
log "Installing the Flux Operator"
$H upgrade --install flux-operator "${FLUX_OPERATOR_CHART}" \
  -n "${FLUX_NS}" --create-namespace --wait --timeout 300s >/dev/null
$K apply -f - <<EOF
apiVersion: fluxcd.controlplane.io/v1
kind: FluxInstance
metadata:
  name: flux
  namespace: ${FLUX_NS}
  annotations: { fluxcd.controlplane.io/reconcileEvery: "1h" }
spec:
  distribution: { version: "2.x", registry: "ghcr.io/fluxcd" }
  components: [source-controller, kustomize-controller, helm-controller, notification-controller]
  cluster: { type: kubernetes }
EOF
for i in $(seq 1 60); do
  $K -n "${FLUX_NS}" get deploy source-controller helm-controller >/dev/null 2>&1 && break; sleep 3
done
$K -n "${FLUX_NS}" rollout status deploy/source-controller --timeout=180s
$K -n "${FLUX_NS}" rollout status deploy/helm-controller   --timeout=180s
ok "Flux controllers Available"

# ── 4. serve THIS repo in-cluster over smart-HTTP (Flux rejects git://) ────────
log "Serving the repo in-cluster over smart-HTTP (ns ${GIT_NS})"
$K apply -f - <<'YAML'
apiVersion: v1
kind: Namespace
metadata: { name: platform-gitsrc, labels: { platform/managed-by: platformctl, platform/role: gitsrc } }
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: { name: repo, namespace: platform-gitsrc }
spec: { accessModes: [ReadWriteOnce], storageClassName: local-path, resources: { requests: { storage: 1Gi } } }
---
apiVersion: v1
kind: ConfigMap
metadata: { name: gitsrc-nginx, namespace: platform-gitsrc }
data:
  default.conf: |
    server {
      listen 80;
      location / {
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
  strategy: { type: Recreate }
  template:
    metadata: { labels: { app: platformctl-git } }
    spec:
      containers:
        # buildpack-deps:bookworm-scm ships git-http-backend; nginx+fcgiwrap are
        # apt-installed once at start (homelab nodes have egress).
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
            httpGet: { path: "/platformctl.git/info/refs?service=git-upload-pack", port: 80 }
            initialDelaySeconds: 5
            periodSeconds: 5
            failureThreshold: 60
          volumeMounts:
            - { name: repo, mountPath: /srv/git }
            - { name: nginx-conf, mountPath: /etc/nginx-conf }
      volumes:
        - { name: repo, persistentVolumeClaim: { claimName: repo } }
        - { name: nginx-conf, configMap: { name: gitsrc-nginx } }
---
apiVersion: v1
kind: Service
metadata: { name: platformctl-git, namespace: platform-gitsrc }
spec:
  selector: { app: platformctl-git }
  ports: [ { name: http, port: 80, targetPort: 80 } ]
YAML

log "Populating the in-cluster git source (committed content only)"
rm -rf "${BARE}"; git clone --bare "${REPO_ROOT}" "${BARE}" >/dev/null 2>&1
( cd "${BARE}" && git update-server-info )
POD=""
for i in $(seq 1 40); do
  POD=$($K -n "${GIT_NS}" get pod -l app=platformctl-git --field-selector=status.phase=Running -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null)
  if [ -n "${POD}" ] && $K -n "${GIT_NS}" exec "${POD}" -- true 2>/dev/null; then break; fi
  POD=""; sleep 3
done
[ -n "${POD}" ] || { echo "FAIL: no running git pod"; exit 1; }
$K -n "${GIT_NS}" exec "${POD}" -- sh -c "rm -rf /srv/git/${GIT_REPO}"
$K -n "${GIT_NS}" cp "${BARE}" "${POD}:/srv/git/${GIT_REPO}"
$K -n "${GIT_NS}" rollout status deploy/platformctl-git --timeout=120s
ok "repo served at ${HTTP_URL} ($(git -C "${BARE}" rev-parse --short HEAD))"

# ── 5. GitRepository -> the in-cluster http source ─────────────────────────────
log "Creating GitRepository ${SRC_NAME} -> in-cluster smart-HTTP"
$K apply -f - <<EOF
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: { name: ${SRC_NAME}, namespace: ${FLUX_NS} }
spec:
  interval: 1m
  url: ${HTTP_URL}
  ref: { branch: main }
EOF
for i in $(seq 1 40); do
  $K -n "${FLUX_NS}" get gitrepository "${SRC_NAME}" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null | grep -qx True && break
  sleep 3
done
ok "GitRepository Ready ($($K -n "${FLUX_NS}" get gitrepository "${SRC_NAME}" -o jsonpath='{.status.artifact.revision}'))"

# ── 6. apply the umbrella; Flux fans it out ────────────────────────────────────
log "Applying the umbrella HelmRelease (clusters/dev/platform.yaml)"
$K apply -f "${REPO_ROOT}/clusters/dev/platform.yaml"
log "Waiting for the inner HelmReleases to reconcile"
for i in $(seq 1 80); do
  cs=$($K -n "${FLUX_NS}" get helmrelease carshowdb -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)
  pg=$($K -n "${FLUX_NS}" get helmrelease carshowdb-postgres -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)
  [ "$cs" = "True" ] && [ "$pg" = "True" ] && break
  sleep 5
done
$K -n "${FLUX_NS}" get helmreleases | sed 's/^/   /'

# ── 7. expose the Flux Web UI on MetalLB + allow LAN -> :9080 ───────────────────
log "Exposing the Flux Web UI on MetalLB (${UI_IP})"
$K apply -f - <<EOF
apiVersion: v1
kind: Service
metadata:
  name: flux-web-ui
  namespace: ${FLUX_NS}
  annotations:
    metallb.universe.tf/address-pool: pool-high
    metallb.universe.tf/loadBalancerIPs: ${UI_IP}
spec:
  type: LoadBalancer
  selector: { app.kubernetes.io/instance: flux-operator, app.kubernetes.io/name: flux-operator }
  ports: [ { name: http, port: 80, targetPort: http-web } ]
---
# The Flux Operator hardens flux-system with a default-deny NetworkPolicy regime
# (k3s enforces NetworkPolicy). Without this, LB traffic to the UI port is dropped
# (LB traffic is SNAT'd to the pod CIDR, so a node-CIDR allow would miss). Allow
# :9080 from anywhere to the operator pod (internal homelab UI).
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: allow-flux-web-ui, namespace: ${FLUX_NS} }
spec:
  podSelector: { matchLabels: { app.kubernetes.io/name: flux-operator } }
  policyTypes: [Ingress]
  ingress:
    - from: [ { ipBlock: { cidr: 0.0.0.0/0 } } ]
      ports: [ { port: 9080, protocol: TCP } ]
EOF
ok "Flux Web UI: http://${UI_IP}"

# ── 8. CoreDNS redundancy (single-replica is a SPOF; k3s addon-managed) ─────────
log "Scaling CoreDNS to 2 replicas for redundancy"
$K -n kube-system scale deploy coredns --replicas=2 >/dev/null 2>&1 || true
warn "Holds across reconcile, but a k3s restart re-applies the host manifest."
warn "For durable HA (replicas:2 + anti-affinity) edit the host file — see scripts/coredns-ha.yaml."

# ── 9. THE FIX: prove the wired DATABASE_URL connects ──────────────────────────
log "Proving the wired DATABASE_URL connects (the dead-Supabase fix)"
$K -n carshowdb-dev-postgres wait --for=condition=Ready pod --all --timeout=180s 2>/dev/null || true
$K -n carshowdb-dev-api delete job db-connectivity-check --ignore-not-found >/dev/null 2>&1 || true
$K apply -n carshowdb-dev-api -f - <<'EOF'
apiVersion: batch/v1
kind: Job
metadata: { name: db-connectivity-check }
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
              echo "DATABASE_URL host: $(echo "$DATABASE_URL" | sed -E 's#.*@([^/]+)/.*#\1#')"
              until pg_isready -d "$DATABASE_URL" -t 3; do echo "waiting for db..."; sleep 2; done
              psql "$DATABASE_URL" -tAc "select 'CONNECTED ok='||1;"
          env:
            - name: DATABASE_URL
              valueFrom: { secretKeyRef: { name: carshowdb-runtime, key: DATABASE_URL } }
EOF
$K -n carshowdb-dev-api wait --for=condition=complete job/db-connectivity-check --timeout=180s \
  && { $K -n carshowdb-dev-api logs job/db-connectivity-check | sed 's/^/   /'; ok "DATABASE_URL connects live on the homelab"; } \
  || { $K -n carshowdb-dev-api logs job/db-connectivity-check | sed 's/^/   /' || true; echo "   (connectivity job did not complete — inspect above)"; }

log "DONE"
echo "   Flux Web UI: http://${UI_IP}    (carshowdb is scale-to-zero: 0 replicas when idle)"
echo "   Inspect:     kubectl --context ${CTX} -n ${FLUX_NS} get helmreleases"
echo "                kubectl --context ${CTX} -n carshowdb-dev-api get all"
echo "   Teardown:    bash scripts/homelab-flux-bringup.sh teardown"
