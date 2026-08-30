#!/usr/bin/env bash
# Local and hosted CI share this non-destructive gate. Set IDP_CI_STRICT=1 to
# require optional analysis tools instead of reporting a local parity gap.
set -euo pipefail

cd "$(git rev-parse --show-toplevel)"

GO=${GO:-go}
STRICT=${IDP_CI_STRICT:-0}
step()  { printf '\033[1m\n▸ %s\033[0m\n' "$*"; }
ok()    { printf '\033[32m  ✓ %s\033[0m\n' "$*"; }
warn()  { printf '\033[33m  ! %s\033[0m\n' "$*" >&2; }
fail()  { printf '\033[31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

step "build"
$GO build -o idpctl ./cmd/idpctl || fail "build failed"
ok "idpctl built"

step "gofmt"
unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  printf '%s\n' "$unformatted" >&2
  fail "not gofmt-clean — run: make fmt"
fi
ok "gofmt clean"

step "go vet"
$GO vet ./... || fail "vet failed"
ok "vet clean"

step "unit and golden tests"
$GO test -race ./... || fail "tests failed"
ok "tests passed"

step "helm lint"
if command -v helm >/dev/null 2>&1; then
  make helm-lint || fail "helm lint failed"
  ok "charts linted"
elif [ "$STRICT" = "1" ]; then
  fail "helm not on PATH"
else
  warn "helm not on PATH — skipping helm-lint"
fi

step "validate examples"
./idpctl validate --file examples/carshowdb/deploy.yaml
./idpctl validate --file examples/yscale-media/yscale-media.deploy.yaml
./idpctl validate --file examples/hello-dev/deploy.yaml
ok "examples valid"

step "render smoke"
smoke_root="$(mktemp -d)"
trap 'rm -rf -- "$smoke_root"' EXIT
mkdir -p "$smoke_root/environments"
cp -R environments/dev "$smoke_root/environments/dev"
./idpctl new app --registry registry.example.invalid/ci \
  --name ci-smoke --dir "$smoke_root/app" >/dev/null
./idpctl render --root "$smoke_root" --env dev \
  --file "$smoke_root/app/deploy.yaml" \
  --image registry.example.invalid/ci/ci-smoke:dev-ci0000 >/dev/null \
  || fail "render smoke failed"
./idpctl catalog --root "$smoke_root" --env dev --format json >/dev/null
./idpctl catalog --root "$smoke_root" --all --out-dir "$smoke_root/site"
test -f "$smoke_root/site/index.html" || fail "catalog did not produce index.html"
ok "render and catalog smokes passed"

step "golangci-lint"
if command -v golangci-lint >/dev/null 2>&1; then
  golangci-lint run ./... || fail "golangci-lint failed"
  ok "golangci-lint clean"
elif [ "$STRICT" = "1" ]; then
  fail "golangci-lint not on PATH"
else
  warn "golangci-lint not on PATH — skipping"
fi

step "govulncheck"
if command -v govulncheck >/dev/null 2>&1; then
  govulncheck ./... || fail "govulncheck reported reachable advisories"
  ok "govulncheck clean"
elif [ "$STRICT" = "1" ]; then
  fail "govulncheck not on PATH"
else
  warn "govulncheck not on PATH — skipping"
fi

printf '\033[1;32m\n✓ local CI gate passed\033[0m\n'
