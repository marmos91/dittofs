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

FROM alpine:latest

RUN apk add --no-cache \
    ca-certificates \
    tzdata

RUN addgroup -g 1000 dittofs && \
    adduser -D -u 1000 -G dittofs dittofs

RUN mkdir -p /data/metadata /data/content /config && \
    chown -R dittofs:dittofs /data /config

WORKDIR /app

COPY --from=builder /build/dittofs .

RUN chown dittofs:dittofs /app/dittofs

USER dittofs

EXPOSE 12049/tcp 12049/udp 9090/tcp

VOLUME ["/data", "/config"]

ENTRYPOINT ["/app/dittofs"]
CMD ["start", "--config", "/config/config.yaml"]
