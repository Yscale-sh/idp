# syntax=docker/dockerfile:1
# jdpctl image — the JDP CLI plus the tools the reusable ship workflow runs in its
# job container: helm, git, github-cli. helm is LOAD-BEARING: internal/deploy runs a
# best-effort `helm template charts/app` LoadBalancer scan only when helm is on PATH,
# so dropping it would silently downgrade the no-LB guardrail to the typed check only.
FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOFLAGS=-trimpath go build -ldflags="-s -w" -o /out/jdpctl ./cmd/jdpctl

FROM alpine:3.20
RUN apk add --no-cache git github-cli ca-certificates bash
# helm from the official pinned image (keeps the render-time LoadBalancer guardrail).
COPY --from=alpine/helm:3.16.4 /usr/bin/helm /usr/local/bin/helm
COPY --from=build /out/jdpctl /usr/local/bin/jdpctl
# platformctl alias: the rename (platformctl -> jdpctl) is recent; keep both names
# working for any lingering call site / doc during the transition (decision D8).
RUN ln -sf /usr/local/bin/jdpctl /usr/local/bin/platformctl
# Mark the workspace safe so git operations on a mounted, differently-owned checkout
# (the ship workflow's cross-repo checkout) don't trip "dubious ownership".
RUN git config --system --add safe.directory '*'
ENTRYPOINT ["jdpctl"]
