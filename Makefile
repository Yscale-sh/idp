# idpctl — developer Makefile.
#
# The everyday loop: `make build` (compile the CLI), `make test` (unit/golden),
# `make lint` (go vet + helm lint), `make validate`/`make render`/`make infra-render`
# (drive the binary on the carshowdb example), `make e2e` (ephemeral k3d cluster).
#
# Conventions honored here:
#   - binary is `./idpctl` at the repo root, built from ./cmd/idpctl
#     (matches .gitignore's /idpctl entry; Flux/CI reference ./idpctl).
#   - charts live at charts/app and charts/infra/<x> (helm lint targets below).
#   - the example app is examples/carshowdb/deploy.yaml.
#
# Targets are tolerant of the in-progress repo: charts/CLI authored by sibling
# work may not exist yet, so chart/render targets skip (with a notice) rather than
# hard-fail when their inputs are missing. `make build` / `make test` always run.

# ── config ───────────────────────────────────────────────────────────────────
BINARY      := idpctl
CMD_PKG     := ./cmd/idpctl
IMAGE_REPO  ?= ghcr.io/yscale-sh/idpctl
TAG         ?= dev
APP_CHART   := charts/app
PG_CHART    := charts/infra/dev-postgres
BUCKET_CHART := charts/infra/bucket-provisioner
EXAMPLE     := examples/carshowdb/deploy.yaml
ENV         ?= dev
# Image must be an immutable tag (never :latest in prod). Override on the CLI:
#   make render IMAGE=ghcr.io/yscale-sh/carshowdb-api:dev-abc123
IMAGE       ?= ghcr.io/yscale-sh/carshowdb-api:dev-local

GO          ?= go
HELM        ?= helm

.DEFAULT_GOAL := help

# ── help ─────────────────────────────────────────────────────────────────────
.PHONY: help
help: ## Show this help.
	@echo "idpctl — make targets:"
	@grep -hE '^[a-zA-Z0-9_-]+:.*?## ' $(MAKEFILE_LIST) \
	  | sort \
	  | awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

# ── build / test ───────────────────────────────────────────────────────────────
.PHONY: build
build: ## Compile the idpctl binary to ./idpctl.
	$(GO) build -o $(BINARY) $(CMD_PKG)

.PHONY: test
test: ## Run all Go unit/golden tests.
	$(GO) test ./...

.PHONY: fmt
fmt: ## Format Go sources (gofmt -w).
	gofmt -l -w .

.PHONY: tidy
tidy: ## Sync go.mod/go.sum.
	$(GO) mod tidy

# ── local CI gate (replaces the auto GitHub ci.yml) ─────────────────────────────
.PHONY: ci-local
ci-local: ## Run the full local CI gate (build, gofmt, vet, race tests, lint, validate, smokes).
	@./scripts/ci-local.sh

.PHONY: hooks
hooks: ## Wire .githooks as the repo hooks dir — enables the pre-push CI gate.
	@git config core.hooksPath .githooks
	@echo "core.hooksPath -> .githooks  (pre-push now runs 'make ci-local')"

# ── lint ───────────────────────────────────────────────────────────────────────
.PHONY: lint
lint: vet helm-lint ## go vet + helm lint (charts/app, charts/infra/dev-postgres).

.PHONY: vet
vet: ## Run go vet across the module.
	$(GO) vet ./...

.PHONY: helm-lint
helm-lint: ## Lint the app + dev-postgres charts (skips a chart that isn't present yet).
	@command -v $(HELM) >/dev/null 2>&1 || { \
	  echo "SKIP helm-lint: 'helm' not found on PATH (install Helm to lint charts)."; \
	  exit 0; }
	@if [ -f "$(APP_CHART)/Chart.yaml" ]; then \
	  echo ">> helm lint $(APP_CHART)"; \
	  $(HELM) lint $(APP_CHART) -f $(APP_CHART)/ci/carshowdb-dev-values.yaml; \
	else \
	  echo "SKIP helm lint $(APP_CHART): no Chart.yaml yet."; \
	fi
	@if [ -f "$(PG_CHART)/Chart.yaml" ]; then \
	  echo ">> helm lint $(PG_CHART)"; $(HELM) lint $(PG_CHART); \
	else \
	  echo "SKIP helm lint $(PG_CHART): no Chart.yaml yet."; \
	fi
	@if [ -f "$(BUCKET_CHART)/Chart.yaml" ]; then \
	  echo ">> helm lint $(BUCKET_CHART)"; \
	  $(HELM) lint $(BUCKET_CHART) -f $(BUCKET_CHART)/ci/lint-values.yaml; \
	else \
	  echo "SKIP helm lint $(BUCKET_CHART): no Chart.yaml yet."; \
	fi

# ── drive the CLI on the example ───────────────────────────────────────────────
# These depend on `build`; they no-op gracefully if the subcommand isn't wired yet.
.PHONY: validate
validate: build ## Validate the carshowdb example deploy.yaml.
	./$(BINARY) validate --file $(EXAMPLE)

.PHONY: render
render: build ## Render carshowdb into environments/$(ENV)/apps (IMAGE=... ENV=...).
	./$(BINARY) render --env $(ENV) --file $(EXAMPLE) --image $(IMAGE)

.PHONY: infra-render
infra-render: build ## Render enabled infra modules into environments/$(ENV)/infra.
	./$(BINARY) infra render --env $(ENV)

.PHONY: catalog
catalog: build ## Generate the read-only HTML catalog for $(ENV) -> catalog.html.
	./$(BINARY) catalog --env $(ENV) --format html --out catalog.html
	@echo "open catalog.html in a browser (read-only view of clusters/$(ENV)/platform.yaml)."

.PHONY: site
site: build ## Generate the whole-platform catalog site (every env + index) -> public/.
	./$(BINARY) catalog --all --out-dir public
	@echo "open public/index.html (read-only view of every environment)."

PAGES_PROJECT ?= idp-catalog
.PHONY: deploy-catalog
deploy-catalog: site ## Deploy the catalog site to Cloudflare Pages (needs CLOUDFLARE_API_TOKEN + CLOUDFLARE_ACCOUNT_ID).
	npx --yes wrangler pages deploy public --project-name=$(PAGES_PROJECT) --branch=main

# ── image (the idpctl container the reusable ship workflow runs) ─────────────────
.PHONY: docker-build
docker-build: ## Build the idpctl image (idpctl+helm+git+gh). Override TAG=...
	docker build -t $(IMAGE_REPO):$(TAG) .

.PHONY: docker-push
docker-push: docker-build ## Build + push the idpctl image to ghcr.
	docker push $(IMAGE_REPO):$(TAG)

# ── e2e ─────────────────────────────────────────────────────────────────────────
.PHONY: e2e
e2e: ## Ephemeral k3d end-to-end test (SKIPs cleanly if k3d is absent).
	./test/e2e/run.sh

# ── housekeeping ────────────────────────────────────────────────────────────────
.PHONY: clean
clean: ## Remove build artifacts.
	rm -f $(BINARY)
	$(GO) clean
