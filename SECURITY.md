# Security

If you find a vulnerability (in the CLI, the charts, the rendered manifests, or the trust model
described in [`docs/SECRETS.md`](docs/SECRETS.md)), please **do not open a public issue**.

Report it privately via [GitHub private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guided-security-vulnerability-reporting)
on this repository (Security tab → "Report a vulnerability").

You should expect an acknowledgement within a week. This is a solo-maintained project; fixes ship
on a best-effort basis, prioritized by whether the issue affects rendered cluster state.

## Scope notes

- Rendered desired state lands in `clusters/<env>/` in your fork. Secret *names* and RFC1918
  LAN addresses appearing there are by design and are not vulnerabilities — the design rule is
  that everything rendered into `clusters/` must be secret-free (references only: ExternalSecrets
  paths, SSM parameter names). Real credential **values** anywhere in the repo absolutely are.
  Report those.
- The platform's stance: secrets are never committed; they live in the configured secrets backend
  (e.g. SSM) and reach pods via External Secrets. Any real credential rendered into `clusters/`
  is a critical bug.
