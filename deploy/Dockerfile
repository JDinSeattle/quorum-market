# syntax=docker/dockerfile:1

# One image holds every service binary. The services share almost all of their
# code, so building them separately would recompile the same packages six times
# and push six nearly identical layers. Which service a container runs is
# decided by its command.

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build

WORKDIR /src

# Dependencies first: this layer is invalidated only when go.mod or go.sum
# change, so ordinary code edits reuse the cached module download.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .

# Stamped so a running container can say exactly what it is. /version reports
# these, and every log line carries the version.
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
ARG TARGETOS
ARG TARGETARCH

# CGO off produces a static binary that runs on a distroless base.
# -trimpath keeps build paths out of the binary; -s -w drop the symbol table.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -trimpath \
      -ldflags="-s -w \
        -X github.com/JDinSeattle/quorum-market/internal/obs.version=${VERSION} \
        -X github.com/JDinSeattle/quorum-market/internal/obs.commit=${COMMIT} \
        -X github.com/JDinSeattle/quorum-market/internal/obs.date=${BUILD_DATE}" \
      -o /out/ ./cmd/...

# Distroless: no shell, no package manager, no setuid binaries. There is
# nothing in the image for an attacker who gains execution to pivot with, and
# nothing to patch on a CVE treadmill. The cost is that a container healthcheck
# has no curl to call, which is why cmd/healthcheck exists.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/ /usr/local/bin/

USER nonroot:nonroot

# No default command: every service is the same image, so the command is what
# selects one. See deploy/docker-compose.yml and the Terraform user data.

ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown
LABEL org.opencontainers.image.title="quorum-market" \
      org.opencontainers.image.description="Distributed e-commerce backend: microservices, custom KV replication, RabbitMQ fulfilment." \
      org.opencontainers.image.source="https://github.com/JDinSeattle/quorum-market" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.revision="${COMMIT}" \
      org.opencontainers.image.created="${BUILD_DATE}"
