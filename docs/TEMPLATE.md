# Using this repository as a template

This repository is a reusable product source and starter GitOps layout, not a
copy of Yscale's or any customer's live cluster state. A repository created from the template
starts fail-closed: registry, shipper image, and Flux URLs use reserved
`example.invalid` values until the operator replaces them.

## Public source boundary

The public repository contains the reusable CLI, charts, schemas, examples,
placeholder environment profiles, and Flux bootstrap structure. It does not contain:

- rendered `clusters/*/platform.yaml` state;
- credentials, kubeconfigs, deploy keys, customer manifests, or account data;
- private endpoints, production topology, or provider inventory; or
- Yscale's operational environment configuration or deployment history.

Template adopters own the generated state, access controls, release policy, and
cluster-specific configuration in their repository. Upstream changes never grant Yscale access to
an adopted installation.

## One-time adoption

1. Create a new repository with **Use this template**. Prefer a fresh template
   repository over a fork when the new installation should have independent
   history and access controls.
2. Build the CLI with `make build`.
3. Replace the registry and repository URL in `idp.yaml`.
4. Generate each environment from your identity. Start with development:

   ```bash
   ./idpctl init --env dev --force
   ```

5. Review the generated `environments/dev/cluster.yaml`. Enable only modules
   and seams the target cluster already provides.
6. Replace the URL in `clusters/dev/flux-instance.yaml`, then render locally:

   ```bash
   ./idpctl infra render --env dev
   ./idpctl validate --file examples/hello-dev/deploy.yaml
   ./idpctl plan --env dev --file examples/hello-dev/deploy.yaml \
     --image ghcr.io/your-org/hello-dev:<immutable-tag>
   ```

7. Run `make ci-local` and a secret scan before the first push. Only bootstrap
   Flux after reviewing the generated `clusters/dev/platform.yaml`.

## Safety boundaries

- Keep `clusters/*/platform.yaml` generated and out of the template source.
  Each installation commits its own rendered state after adoption.
- Never copy `Secret`, kubeconfig, Flux deploy key, cloud credential, private
  hostname, LAN address, or production app manifest into this repository.
- Keep production on a separate protected branch and reject moving image tags.
  Promotion should reuse the artifact already selected in the source
  environment; it must not rebuild from a branch name.
- Flux is the only steady-state writer. Direct `kubectl apply` is limited to the
  initial Flux bootstrap resource; application and module changes are commits.
- Do not attach secret-bearing or organization-wide self-hosted runners to
  pull-request workflows. The template CI uses GitHub-hosted runners and passes
  no deployment credentials to pull-request code.
- GitHub Actions are pinned to commit SHAs. Dependabot may propose updates, but
  review the upstream release and resulting SHA before merging.
- Release publishing uses the repository-scoped `GITHUB_TOKEN`. Do not add a
  long-lived personal access token as a fallback.

The template intentionally does not provision Kubernetes itself. Cluster,
storage, network, backup, ingress, and secret-store availability remain an
operator contract and must exist before the corresponding seam is enabled.
