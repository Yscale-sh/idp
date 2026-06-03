#!/usr/bin/env bash
#
# homelab-bringup.sh — wire the platformctl GitOps loop onto the homelab k3s
# cluster (context: optiplex-pg), using a LAN-LOCAL git source. Nothing leaves
# your network: the repo is served from an in-cluster nginx pod, and ArgoCD (already
# running at 10.0.0.200) reconciles carshowdb + its Postgres from it.
#
# Everything here is ADDITIVE and REVERSIBLE (see teardown at the bottom):
#   - ns platform-gitsrc : a tiny nginx dumb-HTTP git server (serves this repo)
#   - KEDA               : installed via Helm (cluster operator, its own ns)
#   - argocd repo secret : registers the in-cluster git URL (no auth, read-only)
#   - platform-dev-root  : the app-of-apps; Argo then creates
#                          ns carshowdb-dev-api + carshowdb-dev-postgres and deploys into them
#   - a one-off Job      : proves the wired DATABASE_URL connects (the dead-Supabase fix)
#
# It does NOT touch the k3s datastore, control plane, or any existing workload.
#
# Run it:   bash scripts/homelab-bringup.sh
# Teardown: bash scripts/homelab-bringup.sh teardown

set -euo pipefail

CTX="${CTX:-optiplex-pg}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
GIT_NS="platform-gitsrc"
GIT_URL="http://platformctl-git.${GIT_NS}.svc.cluster.local/platformctl.git"
BARE="/tmp/platformctl.git"
K="kubectl --context ${CTX}"
H="helm --kube-context ${CTX}"

log(){ printf '\n\033[1;34m== %s\033[0m\n' "$*"; }
ok(){  printf '   \033[1;32m✓ %s\033[0m\n' "$*"; }

teardown() {
  log "Tearing down platformctl homelab bring-up (additive resources only)"
  $K delete -n argocd application platform-dev-root --ignore-not-found
  $K delete -n argocd secret repo-platformctl --ignore-not-found
  $K delete ns carshowdb-dev-api carshowdb-dev-postgres platform-gitsrc --ignore-not-found
  $H uninstall keda -n keda 2>/dev/null || true
  $K delete ns keda --ignore-not-found
  ok "torn down (existing homelab workloads untouched)"
  exit 0
}
[[ "${1:-}" == "teardown" ]] && teardown

log "Target cluster"; $K config current-context >/dev/null; $K get nodes -o name | sed 's/^/   /'

# ── 1. bare repo (committed content only; gitignored secrets are NOT included) ──
log "Bare-cloning the repo to ${BARE} (+ dumb-HTTP server info)"
rm -rf "${BARE}"
git clone --bare "${REPO_ROOT}" "${BARE}" >/dev/null 2>&1
( cd "${BARE}" && git update-server-info )
ok "bare repo ready ($(cd "${BARE}" && git rev-parse --short HEAD))"

# ── 2. in-cluster git server (nginx dumb-HTTP) ─────────────────────────────────
log "Deploying in-cluster git server (ns ${GIT_NS})"
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
# Smart-HTTP git server: ArgoCD's repo-server uses go-git, which only speaks the
# SMART HTTP protocol. This runs git-http-backend (CGI) behind Apache httpd via the
# canonical ScriptAliasMatch recipe — it routes git URLs to git-http-backend
# EXPLICITLY by regex (no reliance on implicit PATH_INFO), repos served at the ROOT
# path. The git-core dir is auto-detected via `git --exec-path`.
apiVersion: v1
kind: ConfigMap
metadata: { name: gitserver-entrypoint, namespace: platform-gitsrc }
data:
  git.conf.tmpl: |
    LoadModule alias_module modules/mod_alias.so
    LoadModule env_module modules/mod_env.so
    LoadModule cgid_module modules/mod_cgid.so
    ServerName localhost
    ErrorLog /dev/stderr
    LogLevel warn
    SetEnv GIT_PROJECT_ROOT /srv/git
    SetEnv GIT_HTTP_EXPORT_ALL
    ScriptAliasMatch "(?x)^/(.*/(HEAD|info/refs|objects/(info/[^/]+|[0-9a-f]{2}/[0-9a-f]{38,}|pack/pack-[0-9a-f]{40,}\.(pack|idx))|git-(upload|receive)-pack))$" __GHB__/$1
    <Directory "__COREDIR__">
        Require all granted
        Options +ExecCGI
    </Directory>
    <Directory "/srv/git">
        Require all granted
    </Directory>
  run.sh: |
    #!/bin/sh
    set -e
    # apk over the network can hit transient DNS errors — retry before giving up.
    ok=
    for i in 1 2 3 4 5 6 7 8; do
      if apk add --no-cache git >/dev/null 2>&1; then ok=1; break; fi
      echo "apk add git failed (attempt $i) — retrying in 4s"; sleep 4
    done
    [ -n "$ok" ] || { echo "FATAL: could not install git (apk/DNS). Holding container 300s for diagnosis."; sleep 300; exit 1; }
    CONF=/usr/local/apache2/conf/httpd.conf
    COREDIR="$(git --exec-path)"
    echo "git-core dir: $COREDIR ; backend: $COREDIR/git-http-backend"
    sed -e "s|__GHB__|$COREDIR/git-http-backend|" -e "s|__COREDIR__|$COREDIR|" /entrypoint/git.conf.tmpl >> "$CONF"
    echo "--- last 22 lines of httpd.conf ---"; tail -22 "$CONF"
    echo "--- httpd -t (config self-test) ---"; httpd -t 2>&1 || echo "!!! HTTPD CONFIG INVALID !!!"
    exec httpd-foreground
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
        - name: git
          image: httpd:2.4-alpine
          command: ["sh","/entrypoint/run.sh"]
          ports: [ { containerPort: 80 } ]
          readinessProbe: { tcpSocket: { port: 80 }, initialDelaySeconds: 5, periodSeconds: 3 }
          volumeMounts:
            - { name: repo, mountPath: /srv/git }
            - { name: entrypoint, mountPath: /entrypoint }
      volumes:
        - { name: repo, persistentVolumeClaim: { claimName: repo } }
        - { name: entrypoint, configMap: { name: gitserver-entrypoint } }
---
apiVersion: v1
kind: Service
metadata: { name: platformctl-git, namespace: platform-gitsrc }
spec:
  selector: { app: platformctl-git }
  ports: [ { port: 80, targetPort: 80 } ]
YAML
# Force a fresh pod so entrypoint/ConfigMap changes are picked up even when the
# Deployment spec is otherwise unchanged.
$K -n "${GIT_NS}" rollout restart deploy/platformctl-git
$K -n "${GIT_NS}" rollout status deploy/platformctl-git --timeout=90s || echo "   (not Ready in time — dumping git-server logs for diagnosis)"
NEWPOD=$($K -n "${GIT_NS}" get pod -l app=platformctl-git --sort-by=.metadata.creationTimestamp -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null)
echo "   newest git-server pod: ${NEWPOD}"
$K -n "${GIT_NS}" logs "${NEWPOD}" --tail=30 2>&1 | sed 's/^/   [gitsrv] /' || true

# ── 3. populate the git server (copy the bare repo into the pod) ───────────────
log "Copying the repo into the git server"
POD=""
for i in $(seq 1 25); do
  POD=$($K -n "${GIT_NS}" get pod -l app=platformctl-git --field-selector=status.phase=Running -o jsonpath='{.items[-1:].metadata.name}' 2>/dev/null)
  if [ -n "${POD}" ] && $K -n "${GIT_NS}" exec "${POD}" -- true 2>/dev/null; then break; fi
  POD=""; sleep 2
done
[ -n "${POD}" ] || { echo "FAIL: no ready git server pod for populate"; exit 1; }
$K -n "${GIT_NS}" exec "${POD}" -- sh -c 'rm -rf /srv/git/platformctl.git'
$K -n "${GIT_NS}" cp "${BARE}" "${POD}:/srv/git/platformctl.git"
ok "served at ${GIT_URL} (pod ${POD})"

# ── 4. verify the git source clones from inside the cluster ────────────────────
log "Verifying in-cluster clone (smart HTTP)"
$K -n "${GIT_NS}" delete pod git-verify --ignore-not-found >/dev/null 2>&1 || true
$K -n "${GIT_NS}" run git-verify --image=alpine/git --restart=Never --command -- \
  sh -c "for i in \$(seq 1 12); do if git clone ${GIT_URL} /tmp/c 2>/tmp/e; then cd /tmp/c && git log --oneline -1 && echo CLONE_OK && exit 0; fi; echo \"retry \$i:\"; cat /tmp/e; rm -rf /tmp/c; sleep 3; done; echo CLONE_FAIL; exit 1"
$K -n "${GIT_NS}" wait --for=jsonpath='{.status.phase}'=Succeeded pod/git-verify --timeout=120s 2>/dev/null || true
$K -n "${GIT_NS}" logs git-verify 2>/dev/null | sed 's/^/   /'
if ! $K -n "${GIT_NS}" logs git-verify 2>/dev/null | grep -q CLONE_OK; then
  echo "   ───── git server logs (diagnostics) ─────"
  $K -n "${GIT_NS}" logs deploy/platformctl-git --tail=50 2>&1 | sed 's/^/   /' || true
  $K -n "${GIT_NS}" delete pod git-verify --ignore-not-found >/dev/null 2>&1 || true
  echo "FAIL: git source not clonable via smart HTTP"; exit 1
fi
ok "git source is clonable in-cluster (smart HTTP)"
$K -n "${GIT_NS}" delete pod git-verify --ignore-not-found >/dev/null 2>&1 || true

# ── 5. KEDA (the carshowdb ScaledObject needs the CRDs) ────────────────────────
log "Installing KEDA"
$H repo add kedacore https://kedacore.github.io/charts >/dev/null 2>&1 || true
$H repo update kedacore >/dev/null 2>&1 || true
$H upgrade --install keda kedacore/keda -n keda --create-namespace --wait --timeout 180s
$K -n keda rollout status deploy/keda-operator --timeout=180s
ok "KEDA operator Available"

# ── 6. register the in-cluster repo in ArgoCD (no auth, read-only) ─────────────
log "Registering the git repo in ArgoCD"
$K apply -f - <<YAML
apiVersion: v1
kind: Secret
metadata:
  name: repo-platformctl
  namespace: argocd
  labels: { argocd.argoproj.io/secret-type: repository }
stringData:
  type: git
  name: platformctl
  url: ${GIT_URL}
YAML
ok "repo registered"

# ── 7. apply the root app-of-apps; Argo takes over from here ───────────────────
log "Applying platform-dev-root (the one imperative bootstrap step)"
$K apply -n argocd -f "${REPO_ROOT}/argocd/platform-dev-root.yaml"
$K -n argocd annotate application platform-dev-root argocd.argoproj.io/refresh=hard --overwrite >/dev/null 2>&1 || true
echo "   waiting for Argo to reconcile (namespaces are created from the rendered yaml)..."
for i in $(seq 1 60); do
  $K -n carshowdb-dev-api get deploy carshowdb >/dev/null 2>&1 && break
  sleep 5
done
$K -n carshowdb-dev-postgres rollout status statefulset --timeout=240s 2>/dev/null || true
$K -n carshowdb-dev-api rollout status deploy/carshowdb --timeout=240s 2>/dev/null || true

log "ArgoCD application status"
$K -n argocd get applications -o wide 2>/dev/null | sed 's/^/   /' || true

# ── 8. THE FIX: prove the wired DATABASE_URL connects on the homelab ───────────
log "Proving the wired DATABASE_URL connects (homelab)"
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
echo "   ArgoCD UI: http://10.0.0.200  (app: platform-dev-root -> carshowdb, carshowdb-postgres)"
echo "   Inspect:   kubectl --context ${CTX} -n argocd get applications"
echo "              kubectl --context ${CTX} -n carshowdb-dev-api get all"
echo "   Teardown:  bash scripts/homelab-bringup.sh teardown"
