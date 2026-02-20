# Minimal container for testing portmapper without system rpcbind.
# Alpine doesn't have rpcbind by default, so our embedded portmapper is the only one.
FROM golang:alpine

# Install build dependencies and rpcinfo (from nfs-utils)
RUN apk add --no-cache gcc musl-dev git rpcbind netcat-openbsd

# Note: We install rpcbind package to get the rpcinfo binary, but we don't
# start the rpcbind service. This gives us rpcinfo for testing while ensuring
# no system portmapper is running.

WORKDIR /app

# Copy go.mod and go.sum first for better caching
COPY go.mod go.sum ./
RUN go mod download

# Copy source code
COPY . .

# Build both binaries
RUN go build -o /dfs ./cmd/dfs/
RUN go build -o /dfsctl ./cmd/dfsctl/

# Entry point runs the portmapper container test
ENTRYPOINT ["/bin/sh", "-c"]
