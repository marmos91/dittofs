# Build stage
FROM golang:1.24-alpine AS builder

RUN apk add --no-cache git make

WORKDIR /build

COPY go.mod go.sum ./

RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s" \
    -o dittofs \
    cmd/dittofs/main.go

FROM gcr.io/distroless/static-debian12:nonroot

WORKDIR /app

# Copy the binary from builder
COPY --from=builder --chown=65532:65532 /build/dittofs /app/dittofs

USER 65532:65532

EXPOSE 2049/tcp 9090/tcp

# - /data/metadata: Metadata store (BadgerDB, etc.)
# - /data/content: Content store for local filesystem backend (unused for S3)
# - /config: Configuration file location
VOLUME ["/data/metadata", "/data/content", "/config"]

HEALTHCHECK --interval=30s --timeout=10s --start-period=10s --retries=3 \
    CMD ["/app/dittofs", "help"]

ENTRYPOINT ["/app/dittofs"]
CMD ["start", "--config", "/config/config.yaml"]
