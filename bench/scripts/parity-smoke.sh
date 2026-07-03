#!/usr/bin/env bash
# parity-smoke.sh — validate the rclone-parity harness (#1467) end-to-end
# against a throwaway local MinIO container. No cloud credentials involved:
# the only "secrets" are the MinIO defaults for a container that lives for the
# duration of the run on localhost.
#
# Usage:
#   ./bench/scripts/parity-smoke.sh [extra dfsbench parity flags...]
#
# Requirements: docker, rclone, go.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CONTAINER="dfsbench-parity-minio-$$"
MINIO_PORT="${MINIO_PORT:-19000}"

for tool in docker rclone go; do
    command -v "$tool" >/dev/null || { echo "parity-smoke: $tool not found in PATH" >&2; exit 1; }
done

cleanup() {
    docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "parity-smoke: starting MinIO on :$MINIO_PORT"
docker run -d --name "$CONTAINER" \
    -p "127.0.0.1:${MINIO_PORT}:9000" \
    -e MINIO_ROOT_USER=minioadmin \
    -e MINIO_ROOT_PASSWORD=minioadmin \
    minio/minio server /data >/dev/null

# Local-only MinIO credentials (not cloud secrets). Exported so both the
# dittofs S3 client and rclone read the target from the environment — the
# harness never writes credentials to disk.
export AWS_S3_BUCKET="parity-smoke"
export AWS_ACCESS_KEY_ID="minioadmin"
export AWS_SECRET_ACCESS_KEY="minioadmin"
export AWS_ENDPOINT_URL="http://127.0.0.1:${MINIO_PORT}"
export AWS_S3_REGION="us-east-1"

echo "parity-smoke: waiting for MinIO"
export RCLONE_CONFIG_SMOKE_TYPE=s3 \
    RCLONE_CONFIG_SMOKE_PROVIDER=Minio \
    RCLONE_CONFIG_SMOKE_ACCESS_KEY_ID="$AWS_ACCESS_KEY_ID" \
    RCLONE_CONFIG_SMOKE_SECRET_ACCESS_KEY="$AWS_SECRET_ACCESS_KEY" \
    RCLONE_CONFIG_SMOKE_ENDPOINT="$AWS_ENDPOINT_URL" \
    RCLONE_CONFIG_SMOKE_FORCE_PATH_STYLE=true
for _ in $(seq 1 60); do
    if rclone mkdir "smoke:${AWS_S3_BUCKET}" 2>/dev/null; then
        break
    fi
    sleep 1
done
rclone lsf "smoke:${AWS_S3_BUCKET}" >/dev/null # fail fast if bucket never came up

echo "parity-smoke: building dfsbench"
BIN="$(mktemp -d)/dfsbench"
(cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/bench)

echo "parity-smoke: running harness (profile=smoke)"
(cd "$REPO_ROOT" && "$BIN" parity --profile smoke --label local-smoke --out-dir bench/results "$@")

echo "parity-smoke: OK — scorecard in bench/results/parity-local-smoke-*.{json,csv,md}"
