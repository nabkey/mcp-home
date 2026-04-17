# syntax=docker/dockerfile:1.7

FROM --platform=$BUILDPLATFORM golang:1.26.2-bookworm AS build
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

# Pin to a specific release by overriding CLOUDFLARED_URL, e.g.
#   --build-arg CLOUDFLARED_URL=https://github.com/cloudflare/cloudflared/releases/download/2025.10.0
FROM --platform=$BUILDPLATFORM alpine:3.20 AS cloudflared
ARG TARGETARCH
ARG CLOUDFLARED_URL=https://github.com/cloudflare/cloudflared/releases/latest/download
ADD --chmod=0755 ${CLOUDFLARED_URL}/cloudflared-linux-${TARGETARCH} /cloudflared

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=cloudflared /cloudflared /usr/local/bin/cloudflared
COPY --from=build /out/mcp-server /usr/local/bin/mcp-server
USER nonroot
ENTRYPOINT ["/usr/local/bin/mcp-server"]
