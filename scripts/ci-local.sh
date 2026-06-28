#!/usr/bin/env bash
# Local CI gate — mirrors what .github/workflows/ci.yml used to run on GitHub.
#
# Why this exists: ~2/3 of commits on this repo are machine-generated GitOps
# pushes (idp-shipper / litewindow deploy bots) that only touch
# clusters/<env>/platform.yaml — they carry no Go change yet each one fired the
# full remote CI suite. That burned the (free-plan) Actions quota for nothing.
# CI now runs HERE, locally, wired as the pre-push hook (.githooks/pre-push), so
# real breakage is caught before it ever reaches GitHub. ci.yml is kept only as
# a manual (workflow_dispatch) clean-room escape hatch.
#
# Runnable by hand any time:  make ci-local   (or ./scripts/ci-local.sh)
# Bypass the pre-push gate in a pinch:  git push --no-verify
#
# Non-destructive: the render/catalog smokes write to stdout//tmp so they never
# dirty tracked state. Optional tools (helm, golangci-lint, govulncheck) are
# skipped with a warning when not installed rather than blocking the push.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

GO=${GO:-go}
step()  { printf '\033[1m\n▸ %s\033[0m\n' "$*"; }
ok()    { printf '\033[32m  ✓ %s\033[0m\n' "$*"; }
warn()  { printf '\033[33m  ! %s\033[0m\n' "$*" >&2; }
fail()  { printf '\033[31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

step "build"
$GO build -o idpctl ./cmd/idpctl || fail "build failed"
ok "idpctl built"

step "gofmt (formatting gate)"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf '%s\n' "$unformatted" >&2
  fail "not gofmt-clean — run: make fmt"
fi
ok "gofmt clean"

step "go vet"
$GO vet ./... || fail "vet failed"
ok "vet clean"

step "unit + golden tests (race detector)"
$GO test -race ./... || fail "tests failed"
ok "tests passed"

step "helm lint"
if command -v helm >/dev/null 2>&1; then
  make helm-lint || fail "helm lint failed"
  ok "charts linted"
else
  warn "helm not on PATH — skipping helm-lint (CI-parity gap)"
fi

step "validate examples"
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl validate --file examples/dim/api.deploy.yaml
./idpctl validate --file examples/dim/scanner.deploy.yaml
./idpctl validate --file examples/dim/ui.deploy.yaml
ok "examples valid"

step "render smoke (non-destructive --stdout)"
./idpctl render --env dev --file examples/carshowdb/deploy.yaml \
  --image ghcr.io/jakenesler/carshowdb-api:dev-ci0000 --stdout >/dev/null \
  || fail "render smoke failed"
ok "render ok"

step "catalog smoke (read-only -> /tmp)"
./idpctl catalog --env dev --format json >/dev/null
./idpctl catalog --all --out-dir /tmp/idp-ci-site
test -f /tmp/idp-ci-site/index.html || fail "catalog did not produce index.html"
ok "catalog ok"

step "golangci-lint (optional)"
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run ./... || fail "golangci-lint failed"
  ok "golangci-lint clean"
else
  warn "golangci-lint not on PATH — skipping (CI-parity gap)"
fi

step "govulncheck (optional, informational)"
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./... || warn "govulncheck reported advisories (informational, non-blocking)"
  ok "govulncheck done"
else
  warn "govulncheck not on PATH — skipping (informational)"
fi

printf '\033[1;32m\n✓ local CI gate passed\033[0m\n'
