# Releases

Yscale IDP publishes versioned `idpctl` binaries and a multi-architecture Linux container from a
SemVer tag. The release workflow accepts tags shaped like `vX.Y.Z` and publishes only after its
verification job succeeds.

## Published artifacts

Release `vX.Y.Z` contains:

- `idpctl_vX.Y.Z_linux_amd64.tar.gz`
- `idpctl_vX.Y.Z_linux_arm64.tar.gz`
- `idpctl_vX.Y.Z_darwin_amd64.tar.gz`
- `idpctl_vX.Y.Z_darwin_arm64.tar.gz`
- `idpctl_vX.Y.Z_windows_amd64.tar.gz`
- `checksums.txt`
- `go-modules.json`
- `ghcr.io/yscale-sh/idpctl:vX.Y.Z` for Linux amd64 and arm64
- `ghcr.io/yscale-sh/idpctl:sha-<full-commit-sha>` for the same image

The workflow builds archives with a fixed timestamp and numeric ownership, generates SHA-256
checksums, attaches a software bill of materials and provenance to the container build, and records
the immutable image digest in the workflow summary.

## Release gates

Before creating a tag, a maintainer verifies that:

1. the tag target is on the protected default branch and all required pull-request checks passed;
2. `make ci-local` passes, including build, formatting, vet, race-enabled tests, chart lint,
   example validation, render smoke tests, `golangci-lint`, and `govulncheck`;
3. `actionlint .github/workflows/*.yml` passes;
4. the current tree and all reachable public history pass the repository's Gitleaks workflow;
5. template placeholders still fail closed and no generated operational state, credential,
   private endpoint, customer data, or private topology is tracked; and
6. the release notes describe user-visible changes and any compatibility or migration impact.

Pushing a `vX.Y.Z` tag reruns the strict CI gate before the publish job receives `contents: write`
and `packages: write`. Publication uses the repository-scoped `GITHUB_TOKEN`; it does not require a
personal access token.

## Verify a release

Download the archive and `checksums.txt` from the same release. For example:

```bash
VERSION=v1.0.0
ASSET="idpctl_${VERSION}_linux_amd64.tar.gz"
curl -fLO "https://github.com/Yscale-sh/idp/releases/download/${VERSION}/${ASSET}"
curl -fLO "https://github.com/Yscale-sh/idp/releases/download/${VERSION}/checksums.txt"
grep "${ASSET}$" checksums.txt | sha256sum -c -
```

The container tag is convenient for discovery, but production automation should pin the immutable
digest recorded by the release workflow:

```bash
docker pull ghcr.io/yscale-sh/idpctl:v1.0.0
docker inspect --format='{{index .RepoDigests 0}}' ghcr.io/yscale-sh/idpctl:v1.0.0
```

## Public release boundary

A release is built only from this repository's reusable product source. Generated
`clusters/*/platform.yaml` files and operator-specific deployment state belong in the adopter's
repository and are not release inputs. See [`TEMPLATE.md`](TEMPLATE.md) for the complete public
source boundary.
