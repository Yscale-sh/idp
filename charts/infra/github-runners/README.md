# github-runners

A `localChart` platform module: **scale-to-zero GitHub Actions self-hosted runner
pools** (summerwind ARC) for one or more GitHub orgs, on the **shared** in-cluster
ARC controller. One `RunnerDeployment` + `HorizontalRunnerAutoscaler` per org, each
scaling **0 → N** on queued workflow runs (idle footprint is zero).

This chart deploys **only the pools**. It assumes the ARC **controller + CRDs**
(`actions.summerwind.dev`) are already installed in the target namespace (`github`).

## How it's wired (the idp way)

- Cataloged in `modules/registry.yaml` (`github-runners`, `source: localChart`).
- Enabled + given its per-org pools in `environments/dev/cluster.yaml`
  (`modules.github-runners.values.runners`).
- `make infra-render ENV=dev` (→ `idpctl infra render`) writes the module's
  HelmRelease into `clusters/dev/platform.yaml`; Flux reconciles it.

## Per-org auth (out-of-band, never templated)

The controller's default credentials are pinned to one org, and we do **not** make
any GitHub App public — so each pool authenticates with its **own** fine-grained
PAT scoped to just that org, via `githubAPICredentialsFrom` → Secret
`github-token-<org>`. Both the RunnerDeployment **and** the HRA reference it (the
HRA's queued-runs metric query needs its own creds, or it 404s on private repos).

Create the Secret once per org (the chart never sees the PAT):

```bash
# Fine-grained PAT, resource owner = the org, All repositories,
#   Repository: Actions = Read-only;  Organization: Self-hosted runners = Read and write
./setup-org-runner-secret.sh Yscale-sh      github-token-yscale
./setup-org-runner-secret.sh Cartogopher-ai github-token-cartogopher
```

It validates the PAT can mint an org runner-registration token before writing the
Secret. Also expects the shared `stackmaster-dockerhub` pull secret in the namespace.

## Adding an org

Add an entry under `modules.github-runners.values.runners` in
`environments/<env>/cluster.yaml`:

```yaml
runners:
  myorg:
    organization: My-Org
    credentialsSecret: github-token-myorg
    extraLabels: [myorg]
    maxReplicas: 2
    repositories: [repo-a, repo-b]
```

then `./setup-org-runner-secret.sh My-Org github-token-myorg`, `make infra-render
ENV=dev`, commit, push.
