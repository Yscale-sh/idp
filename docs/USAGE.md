# Operating the Yscale IDP

These runbooks cover routine operations in an adopted Yscale IDP repository. For command design, read
[`DEPLOY_GO_CLI.md`](DEPLOY_GO_CLI.md); for the target production shape, read
[`ARCHITECTURE.md`](../ARCHITECTURE.md).

Each operation starts from an application's `deploy.yaml`. `idpctl render` upserts the application
into `clusters/<env>/platform.yaml`; commit that file on the environment's Flux branch and let Flux
reconcile it. Do not deploy applications with `kubectl apply` or `helm upgrade`.

## 1. Deploy a brand-new app

1. Scaffold the app manifest. The CLI takes the app name as a flag:

   ```bash
   idpctl new app --name myapp --dir ./myapp
   ```

2. Edit `myapp/deploy.yaml`. Declare the tagless image repository, port, routes, stores,
   secrets, probes, and sizing. Do not write Kubernetes manifests.
3. Validate and render it into dev with an immutable image built by CI:

   ```bash
   TAG=abc1234
   idpctl validate --file myapp/deploy.yaml
   idpctl render --env dev --file myapp/deploy.yaml \
     --image ghcr.io/<owner>/myapp:$TAG
   ```

4. Review and commit `clusters/dev/platform.yaml`, then push the branch named by
   `environments/dev/cluster.yaml` under `flux.branch`:

   ```bash
   git diff -- clusters/dev/platform.yaml
   git add clusters/dev/platform.yaml
   git commit -m "deploy(dev): add myapp"
   git push
   ```

5. Flux sees the commit and reconciles the dev umbrella. Run `idpctl doctor --env dev`
   and inspect the Flux UI if it does not settle.

To put the app on the automatic path, add its repo, watched branch, and deploy file to
the `idp-shipper` registry in `environments/dev/cluster.yaml`, run
`idpctl infra render --env dev`, and commit the resulting umbrella change.

## 2. Ship the normal daily change

The in-cluster `idp-shipper` handles continuous development delivery. Its registry (which apps to
ship, with repo, branch,
and deploy file paths) is infra-owned config, a ConfigMap declared in
`environments/dev/cluster.yaml` and applied with `idpctl infra render`. A developer never
touches the registry or the platform repo; they only edit their own `deploy.yaml`.

1. Push the application commit to the branch registered with the shipper. For an app
   registered on `master`:

   ```bash
   git push origin master
   ```

2. The in-cluster `idp-shipper` reads the GitHub head SHA for the registered repo and
   branch. When it changes, it fetches the registered `deploy.yaml` at that commit and
   derives the build set from the app manifests, deduped by `runtime.image`.
3. The image-builder clones that commit, builds with rootless BuildKit, and pushes
   `<runtime.image>:<short-sha>` to GHCR. Build inputs are self-declared by the app:

   ```yaml
   build:
     context: .
     dockerfile: Dockerfile
     submodules: []
   ```

   If none of those inputs changed, the shipper reuses the image tag already pinned in
   the umbrella instead of rebuilding it.
4. The shipper runs the same validate/policy/render pipeline as `idpctl render`, commits
   the updated `clusters/dev/platform.yaml` to the configured platform branch, and pushes.
5. Flux reconciles that commit. No app deploy uses `kubectl apply` or `helm upgrade`.

## 3. Promote dev to prod

1. From the app revision that supplied its `deploy.yaml`, promote the workload:

   ```bash
   idpctl promote myapp prod --from dev --file deploy.yaml
   ```

2. Review the rendered `clusters/prod/platform.yaml`. Promotion reads the image digest
   already pinned in dev's umbrella and carries that digest forward; it never rebuilds the
   artifact. Prod's policy, secrets backend, and namespaces are applied during the
   re-render. The promotion gate is data: `environments/prod/cluster.yaml` declares
   `promotion.from: dev`, so `--from dev` is the only accepted source until stage is wired.
3. Commit the result on the branch printed by the command. That branch comes from prod's
   `flux.branch`; in the reference skeleton it is `prod`, and the prod cluster syncs
   `refs/heads/prod`.
   Prod also has `allowMutableTags: false`, and prod is hard-rejected in code regardless, so
   mutable tags such as `latest` cannot reach it.
4. Push and watch prod Flux reconcile.

The dev and prod umbrellas live on different branches: dev's on `main`, prod's on `prod`.
When you run `promote` from a `prod`-branch checkout, point `--source-root` at a `main`
checkout so it reads dev's pinned image from there while writing the prod render into the
current tree. The command defaults the source read to `--root` for same-tree promotes.

Rollback is a Git revert of the promotion commit:

```bash
git revert <promotion-commit>
git push
```

## 4. Expose a dev app behind login (Cloudflare Access)

By default a `public: true` route in dev is served on the LAN (a MetalLB
LoadBalancer at `<host>.local`). To also provide a public HTTPS URL gated by SSO login, add a second
route under the instance's development Cloudflare zone:
it is served through a Cloudflare Tunnel and sits behind that account's wildcard
Cloudflare Access app, so every request must authenticate. The two routes coexist: the LAN address
stays open on `.local`, and the internet-facing route requires login.

> **Your dev zone is per-instance.** It is the `cloudflareZone` key in
> [`environments/dev/cluster.yaml`](../environments/dev/cluster.yaml), paired with
> a matching `*.<zone>` Cloudflare Access app on your account. In these examples the
> zone is `yscale.sh`; substitute your own zone throughout the steps below.

**Domain convention.** Your dev host is a subdomain of that zone. Take the
distinctive name of your production domain and reuse it as a subdomain, so a
teammate can guess the URL:

| production domain | dev host (the `yscale.sh` example zone) |
|---|---|
| `acme.com` | `acme.yscale.sh` |
| `blog.example.org` | `blog.yscale.sh` |
| `carshowdb.example.com` | `carshowdb.yscale.sh` |

1. **Declare the routes** in `deploy.yaml`. A route under the CF zone MUST declare
   `access` (render-time policy rejects an unguarded one). See the worked example
   [`examples/hello-dev/deploy.yaml`](../examples/hello-dev/deploy.yaml):

   ```yaml
   routes:
     - host: hello.local          # LAN, unrestricted
       public: true
     - host: hello.yscale.sh      # Cloudflare Tunnel + Access login (your zone here)
       public: true
       access:
         humans: true             # interactive human SSO
   ```

2. **Bootstrap the tunnel once.** This creates/adopts the tunnel, mints its
   connector token, upserts the DNS record, and stashes the token as the app's
   dev Secret. Needs `CLOUDFLARE_API_TOKEN` + `CLOUDFLARE_ACCOUNT_ID` in the
   environment and a kubeconfig pointed at the dev cluster:

   ```bash
   idpctl tunnel up --env dev --token-out /tmp/tunnel-token
   scripts/setup-dev-tunnel-secret.sh hello /tmp/tunnel-token
   rm /tmp/tunnel-token          # delete the token file once it is stashed
   ```

3. **Render, commit, and let Flux reconcile.** The render adds the cloudflared
   sidecar and the `TUNNEL_TOKEN` ExternalSecret; Flux brings them up:

   ```bash
   idpctl render --env dev --file deploy.yaml --image ghcr.io/OWNER/hello:<tag>
   git add deploy.yaml clusters/dev/platform.yaml
   git commit -m "expose hello in dev"
   git push
   ```

4. **Verify the login gate.** This probes each CF-zone host and confirms it
   redirects (30x) to `*.cloudflareaccess.com`, retrying with backoff to allow for
   DNS + certificate propagation:

   ```bash
   idpctl tunnel up --env dev --verify-access --skip-dns
   ```

For the model behind this (which hosts tunnel, why `.local` stays on the LAN, and
the policy rail), read [`CONVENTIONS.md`](../CONVENTIONS.md) section 9.

## 5. Inspect declared deployments

1. Read the committed desired state in the terminal:

   ```bash
   idpctl catalog --env dev
   ```

2. Produce HTML or JSON when needed:

   ```bash
   idpctl catalog --env dev --format html --out catalog.html
   idpctl catalog --env dev --format json
   ```

3. The repository shortcut writes `catalog.html`:

   ```bash
   make catalog ENV=dev
   ```

4. Build the platform catalog (one page per environment plus `index.html`) with `--all`:

   ```bash
   idpctl catalog --all --out-dir public
   ```

   `.github/workflows/catalog.yml` publishes this site to Cloudflare Pages.

The catalog does not contact the cluster. It projects `clusters/<env>/platform.yaml`,
so it is a read-only view of committed git state, never a writer.

## 6. Check the live cluster seams

1. Select the intended kubeconfig context, then run:

   ```bash
   idpctl doctor --env dev
   ```

2. Treat failures as contract violations. Doctor resolves the seams the env declares in
   `environments/dev/cluster.yaml` and probes the live cluster for each: API reachability,
   the FluxInstance plus the Git source and the env umbrella, the external-secrets store,
   observability (Loki), the KEDA operator when autoscale is declared, and the ingress or
   tunnel and LAN seams.
3. Use an explicit context when the current one is ambiguous:

   ```bash
   idpctl doctor --env prod --context <kube-context>
   ```

## 7. Debug a stuck reconcile

1. Open the Flux Operator's embedded UI:

   ```bash
   kubectl -n flux-system port-forward svc/flux-operator 9080:9080
   ```

   Open `http://localhost:9080` and start at the environment umbrella (`platform` in
   dev, `platform-<env>` elsewhere).
2. Find the failing child release and read its conditions and events:

   ```bash
   kubectl get helmrelease -A
   kubectl -n <app-namespace> describe helmrelease <workload>
   kubectl -n <app-namespace> get events --sort-by=.lastTimestamp
   ```

   A failed release usually identifies the rendered resource that is not Ready. Inspect
   that resource and its pod logs; do not bypass GitOps with a manual Helm install.
3. Check the common causes:

   - Bad or unpushed image tag: pods report `ImagePullBackOff`. Confirm the umbrella's
     repository/tag exists in GHCR and that the namespace has the configured pull secret.
   - Missing secret seam: `ExternalSecret` or `SecretStore` is not Ready, or the pod has
     `CreateContainerConfigError`. Seed the declared backend key and verify the store with
     `idpctl doctor`.
   - KEDA never Ready: inspect the `ScaledObject`, its trigger authentication, and the KEDA
     operator events. The app owns replicas only when the declared autoscale seam is healthy.
   - GPU workload never schedules: pod events show an unavailable extended resource or
     selector mismatch. The device plugin and a matching GPU node must be Ready before the
     HelmRelease can settle.
4. `disableWait` on the umbrella makes Flux create and reconcile child HelmReleases without
   waiting for every child resource. One broken app therefore reports failure on its own
   HelmRelease instead of wedging or rolling back its siblings. Keep the isolation; fix the
   child release's underlying condition.

## 8. `idpctl` command reference

| Verb | Purpose |
|---|---|
| `idpctl init` | Scaffold `environments/<env>/cluster.yaml` for a platform instance. |
| `idpctl new app` | Scaffold a new app's `deploy.yaml`. |
| `idpctl validate` | Validate the app contract without environment policy. |
| `idpctl plan` | Validate, apply policy, and render to stdout without writes. |
| `idpctl render` | Upsert an app into an environment umbrella. |
| `idpctl promote` | Carry a source environment's pinned image into a target environment. |
| `idpctl build` | Build and push an image with the in-cluster image-builder. |
| `idpctl doctor` | Probe whether the live cluster provides its declared seams. |
| `idpctl catalog` | Project committed desired state as text, HTML, JSON, or a site. |
| `idpctl remove` | Remove an app or component from an umbrella for Flux to prune. |
| `idpctl dns sync` / `prune` | Reconcile or remove Cloudflare CNAMEs for public routes. |
| `idpctl tunnel up` / `down` | Create/adopt or remove an app's Cloudflare Tunnel wiring. |
| `idpctl infra plan` / `render` / `apply` | Inspect or render infra modules; direct apply is explicitly non-default. |
| `idpctl completion` | Generate shell completion for bash, fish, PowerShell, or zsh. |
| `idpctl help` | Show command help. |
