# Security

Report suspected vulnerabilities in the CLI, charts, rendered manifests, release artifacts, or
the trust model described in [`docs/SECRETS.md`](docs/SECRETS.md). Do not open a public issue.

Report it privately via [GitHub private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guided-security-vulnerability-reporting)
on this repository (Security tab → "Report a vulnerability").

Include the affected version, impact, reproduction steps, and any suggested mitigation. Maintainers
coordinate disclosure on a best-effort basis; the project does not promise a response or remediation
SLA. Reports involving credentials or rendered cluster state receive the highest priority.

## Supported versions

Security fixes target the latest published release and the default branch. Older releases may need
to upgrade to receive a fix.

## Scope notes

- Rendered desired state lands in `clusters/<env>/` in an adopted template repository. Secret
  *names* and operator-owned private addresses may appear there by design; they are not, by
  themselves, vulnerabilities. Rendered state must contain references only, such as
  `ExternalSecret` paths or parameter names. Report any real credential **value** committed or
  rendered into the repository.
- The platform's stance: secrets are never committed; they live in the configured secrets backend
  and reach pods through External Secrets. A real credential rendered into `clusters/` is a
  critical bug.
