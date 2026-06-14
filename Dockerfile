# syntax=docker/dockerfile:1
# idpctl image — the IDP CLI plus the tools the reusable ship workflow runs in its
# job container: helm, git, github-cli. helm is LOAD-BEARING: internal/deploy runs a
# best-effort `helm template charts/app` LoadBalancer scan only when helm is on PATH,
# so dropping it would silently downgrade the no-LB guardrail to the typed check only.
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git curl
# kubectl for idp-shipper: internal/kube + internal/builder drive build Jobs by
# shelling kubectl (idp deliberately pulls in no client-go). Fetched from the
# official k8s release (no third-party registry dep); pinned to the cluster (k3s 1.31).
RUN mkdir -p /out \
  && curl -fsSLo /out/kubectl https://dl.k8s.io/release/v1.31.5/bin/linux/amd64/kubectl \
  && chmod +x /out/kubectl
# BuildKit cache mounts: the go module cache (/go/pkg/mod) and build cache
# (/root/.cache/go-build) persist on the image-builder's per-image worker PVC root
# across builds, so a source change recompiles only what changed instead of every
# dependency from scratch (cold builds were minutes; warm are seconds).
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/idpctl ./cmd/idpctl
# idp-shipper: the in-cluster orchestrator (poll app repos -> build -> render ->
# commit). Same image; the shipper Deployment overrides command to idp-shipper.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/idp-shipper ./cmd/idp-shipper

FROM alpine:3.24
RUN apk add --no-cache git github-cli ca-certificates bash
# helm from the official pinned image (keeps the render-time LoadBalancer guardrail).
COPY --from=alpine/helm:3.16.4 /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/kubectl /usr/local/bin/kubectl
COPY --from=build /out/idpctl /usr/local/bin/idpctl
COPY --from=build /out/idp-shipper /usr/local/bin/idp-shipper
# platformctl alias: the rename (platformctl -> idpctl) is recent; keep both names
# working for any lingering call site / doc during the transition (decision D8).
RUN ln -sf /usr/local/bin/idpctl /usr/local/bin/platformctl
# Mark the workspace safe so git operations on a mounted, differently-owned checkout
# (the ship workflow's cross-repo checkout) don't trip "dubious ownership".
RUN git config --system --add safe.directory '*'
ENTRYPOINT ["idpctl"]
