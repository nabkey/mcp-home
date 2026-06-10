# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.4-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

ARG VERSION=dev
COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/mcp-server ./cmd/mcp-server

# Pinned for reproducible builds; Dependabot does not track this, so bump
# CLOUDFLARED_VERSION when updating. Override either arg at build time, e.g.
#   --build-arg CLOUDFLARED_VERSION=2026.6.0
FROM --platform=$BUILDPLATFORM alpine:3.24 AS cloudflared
ARG TARGETARCH
ARG CLOUDFLARED_VERSION=2026.6.0
ARG CLOUDFLARED_URL=https://github.com/cloudflare/cloudflared/releases/download/${CLOUDFLARED_VERSION}
ADD --chmod=0755 ${CLOUDFLARED_URL}/cloudflared-linux-${TARGETARCH} /cloudflared

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=cloudflared /cloudflared /usr/local/bin/cloudflared
COPY --from=build /out/mcp-server /usr/local/bin/mcp-server
USER nonroot
ENTRYPOINT ["/usr/local/bin/mcp-server"]
