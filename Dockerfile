# DittoFS Production Dockerfile
# Multi-stage build for minimal, secure production image

# Build stage
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

# Build arguments for cross-compilation
ARG TARGETOS
ARG TARGETARCH

# Build arguments for version info (injected by GoReleaser or CI)
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_DATE=unknown

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build

# Copy dependency files first for better layer caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build the binary with version info
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build \
    -ldflags="-w -s -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${BUILD_DATE}" \
    -o dittofs \
    cmd/dittofs/main.go

# Runtime stage - using Alpine for healthcheck support via wget
FROM alpine:3.21

# OCI Image Labels
LABEL org.opencontainers.image.title="DittoFS" \
      org.opencontainers.image.description="Modular virtual filesystem with pluggable storage backends" \
      org.opencontainers.image.url="https://github.com/marmos91/dittofs" \
      org.opencontainers.image.source="https://github.com/marmos91/dittofs" \
      org.opencontainers.image.vendor="Marco Moschettini" \
      org.opencontainers.image.licenses="MIT"

# Install ca-certificates for HTTPS (S3, etc.) and create non-root user
RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -g 65532 -S dittofs && \
    adduser -u 65532 -S -G dittofs -h /app dittofs

WORKDIR /app

# Copy the binary from builder
COPY --from=builder --chown=65532:65532 /build/dittofs /app/dittofs

# Create data directories with proper permissions
RUN mkdir -p /data/metadata /data/content /data/cache /config && \
    chown -R 65532:65532 /data /config

# Run as non-root user
USER 65532:65532

# Expose ports:
# - 12049: NFS server (default)
# - 12445: SMB server (default)
# - 9090: Prometheus metrics
# - 8080: REST API (health checks, management)
EXPOSE 12049/tcp 12445/tcp 9090/tcp 8080/tcp

# Volume mounts:
# - /data/metadata: Metadata store (BadgerDB, etc.)
# - /data/content: Content store for local filesystem backend
# - /data/cache: Cache and WAL directory
# - /config: Configuration file location
VOLUME ["/data/metadata", "/data/content", "/data/cache", "/config"]

# Health check using the REST API health endpoint
# Checks liveness every 30s, allows 10s startup grace period
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

ENTRYPOINT ["/app/dittofs"]
CMD ["start", "--config", "/config/config.yaml"]
