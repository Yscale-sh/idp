# argocd/ — the root app-of-apps (the only imperative step)

This directory holds the two **root Argo CD Applications**, one per environment:

| File | Root app | Watches |
|---|---|---|
| `platform-dev-root.yaml`  | `platform-dev-root`  | `environments/dev/`  (recurse) |
| `platform-prod-root.yaml` | `platform-prod-root` | `environments/prod/` (recurse) |

Each root is an **app-of-apps**: it points at `environments/<env>/` with
`directory.recurse: true`, so Argo CD picks up every rendered child Application
under it and reconciles them:

```
platform-<env>-root
  ├─ environments/<env>/infra/*.yaml   ← one Application per ENABLED module
  │                                       (rendered by `platformctl infra render`)
  └─ environments/<env>/apps/*.yaml    ← one Application per app
                                          (rendered by `platformctl render`)
```

## The bootstrap is the ONLY imperative step

Applying a root app is the **single, one-time `kubectl` command** in the whole
platform. Everything after it is GitOps: Argo CD is the only writer, and
"deploying" is a git commit into this repo that Argo CD reconciles. You never run
`kubectl apply` or `helm upgrade` for an app or a module by hand again.

```bash
# dev (homelab k3s):
kubectl apply -n argocd -f argocd/platform-dev-root.yaml

# prod (cloud k3s), after pointing repoURL/targetRevision at your release:
kubectl apply -n argocd -f argocd/platform-prod-root.yaml
```

(Installing Argo CD itself is a prerequisite, not part of this repo — see the
homelab install notes. On the homelab cluster Argo CD serves plain HTTP at
`http://10.0.0.200`.)

## What you change, and what you never change

- **You rarely touch these two files.** Adding an app or enabling a module does
  **not** require editing the root — it is just a new rendered file under
  `environments/<env>/apps/` or `environments/<env>/infra/`, which the root
  auto-discovers on the next sync.
- You edit a root app only to change the **environment-wide** wiring: the
  `repoURL`, the `targetRevision` (dev tracks `HEAD`; prod is pinned to a release
  tag like `v1` so prod only moves on a deliberate bump), or the sync policy.

## Key fields

- `source.repoURL` — **placeholder** `https://github.com/jakenesler/platformctl`.
  Replace with the real remote. For local dev you can register a path/remote with
  `argocd repo add <url>` (and for private repos, add credentials) before applying.
- `source.path` + `directory.recurse: true` — watch the whole environment tree.
- `directory.include`/`exclude` — only `apps/*.yaml` and `infra/*.yaml` are treated
  as children; `cluster.yaml` (the module **registry** that feeds
  `platformctl infra render`) is **not** an Argo object and is excluded.
- `syncPolicy.automated` (`prune` + `selfHeal`) — deleting a rendered file removes
  its workload; out-of-band drift is reverted back to git.
- `destination.server: https://kubernetes.default.svc` — in-cluster (Argo CD
  reconciles into the same cluster it runs in).

## Render → commit → reconcile (the everyday path)

```bash
# 1. render an app's desired state into environments/<env>/apps/<app>.yaml
platformctl render --env dev --file examples/carshowdb/deploy.yaml \
  --image ghcr.io/jakenesler/carshowdb-api:dev-<sha>

# 2. render the enabled infra modules into environments/<env>/infra/*.yaml
platformctl infra render --env dev

# 3. commit + push. Argo CD (via the root app) reconciles the change.
git add environments/dev && git commit -m "deploy carshowdb to dev" && git push
```
