# syntax=docker/dockerfile:1
# idpctl image — the IDP CLI plus the tools the reusable ship workflow runs in its
# job container: helm, git, github-cli. helm is LOAD-BEARING: internal/deploy runs a
# best-effort `helm template charts/app` LoadBalancer scan only when helm is on PATH,
# so dropping it would silently downgrade the no-LB guardrail to the typed check only.
FROM --platform=$BUILDPLATFORM golang:1.25.13-alpine AS build
ARG TARGETARCH
ARG KUBECTL_VERSION=v1.35.6
WORKDIR /src
RUN apk add --no-cache git curl
# kubectl for idp-shipper: internal/kube + internal/builder drive build Jobs by
# shelling kubectl (idp deliberately pulls in no client-go). Fetched from the
# official Kubernetes release and verified against its published checksum.
RUN mkdir -p /out \
  && curl -fsSLo /out/kubectl https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl \
  && curl -fsSLo /out/kubectl.sha256 https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${TARGETARCH}/kubectl.sha256 \
  && echo "$(cat /out/kubectl.sha256)  /out/kubectl" | sha256sum -c - \
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
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/idpctl ./cmd/idpctl
# idp-shipper: the in-cluster orchestrator (poll app repos -> build -> render ->
# commit). Same image; the shipper Deployment overrides command to idp-shipper.
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux GOARCH=${TARGETARCH} GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/idp-shipper ./cmd/idp-shipper

FROM alpine:3.24
ARG SOURCE_REPOSITORY=https://example.invalid/your-org/idp
LABEL org.opencontainers.image.source="${SOURCE_REPOSITORY}"
RUN apk add --no-cache git github-cli ca-certificates bash tini \
  && addgroup -g 10001 -S idp \
  && adduser -u 10001 -S -G idp idp
# helm from the official pinned image (keeps the render-time LoadBalancer guardrail).
COPY --from=alpine/helm:3.16.4 /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/kubectl /usr/local/bin/kubectl
COPY --from=build /out/idpctl /usr/local/bin/idpctl
COPY --from=build /out/idp-shipper /usr/local/bin/idp-shipper
# platformctl alias: the rename (platformctl -> idpctl) is recent; keep both names
# working for any lingering call site / doc during the transition (decision D8).
RUN ln -sf /usr/local/bin/idpctl /usr/local/bin/platformctl
ENV HOME=/home/idp
USER 10001:10001
ENTRYPOINT ["/sbin/tini", "--", "idpctl"]
