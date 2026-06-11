# Contributing

Thanks for taking a look. This is a small, opinionated project — issues and PRs are welcome,
but read [`CONVENTIONS.md`](CONVENTIONS.md) first: it's the platform contract, and changes that
fight it will be declined even if the code is good.

## Dev setup

You need Go ≥ 1.25 and (optionally) Helm and k3d.

```bash
make build       # compiles ./idpctl
make test        # unit + golden tests (no cluster needed)
make lint        # go vet + helm lint (helm lint skips if helm isn't installed)
make e2e         # ephemeral k3d cluster end-to-end (skips cleanly if k3d is absent)
```

The render pipeline is covered by golden tests in `internal/render/testdata/`. If you change
rendering output on purpose, update the golden files and say so in the PR — golden diffs are the
review surface.

## Before you open a PR

- `make test` and `make lint` pass.
- `make e2e` passes if you touched `internal/render`, `internal/policy`, or anything in `charts/`.
- New `deploy.yaml` fields need all four: schema (`schemas/deploy.schema.json`), types + validation
  (`internal/appconfig`), rendering (`internal/render`), and a row in the app-contract table in
  `README.md` / `CONVENTIONS.md`.
- Commit messages follow the existing style: short, imperative, `area: what changed`
  (e.g. `feat(expose): pool — MetalLB auto-assign from a named IPAddressPool`).

## Design boundaries (the short version)

- **Flux is the only writer.** Nothing in this repo may `kubectl apply` outside of tests.
- **The contract is a shopping list, not Kubernetes.** New fields describe *what the app needs*,
  never raw k8s objects. If a field only makes sense to someone who knows k8s, it's wrong.
- **Fail closed on identity.** Nothing may default to a specific registry, repo, or account —
  identity lives in `idp.yaml` only (`internal/tenant` enforces this; there's a test asserting
  scaffolds contain no hardcoded identity).
- **No LoadBalancers** except the labeled MetalLB LAN escape hatch (`internal/policy`).

## Reporting bugs

A failing `deploy.yaml` + the `idpctl` command + actual vs expected output is the perfect bug
report. For anything security-sensitive, see [`SECURITY.md`](SECURITY.md).
