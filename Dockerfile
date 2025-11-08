FROM golang:1.25-alpine AS builder

# Install build dependencies
RUN apk add --no-cache git ca-certificates tzdata

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download
RUN go mod verify

# Copy source code
COPY . .

# Build the binary
# CGO_ENABLED=0 for static binary
# -ldflags="-w -s" to strip debug info and reduce size
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -ldflags="-w -s -X main.version=$(git describe --tags --always --dirty)" \
    -o dittofs \
    cmd/dittofs/main.go

# Runtime stage
FROM scratch

# Copy timezone data
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo

# Copy CA certificates for HTTPS
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

# Copy binary from builder
COPY --from=builder /build/dittofs /dittofs

# Create necessary directories (these will be mount points)
# Note: scratch doesn't have mkdir, so we copy from builder
COPY --from=builder /tmp /tmp

# Set environment variables
ENV LOG_LEVEL=INFO
ENV PORT=2049
ENV CONTENT_PATH=/data/content

# Expose NFS port (standard is 2049)
EXPOSE 2049/tcp

# Create a volume for persistent data
VOLUME ["/data"]

# Health check
# Note: This is basic - in production you'd want a proper health endpoint
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD ["/dittofs", "-help"] || exit 1

# Run the server
ENTRYPOINT ["/dittofs"]
CMD ["-port", "2049", "-log-level", "INFO", "-content-path", "/data/content"]
