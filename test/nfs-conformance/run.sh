#!/usr/bin/env bash
# Run NFS Kerberos conformance tests.
#
# Starts KDC + DittoFS + NFS client in Docker, bootstraps the server,
# then runs sec=krb5 mount tests.
#
# Usage:
#   ./run.sh [--verbose]

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
VERBOSE=false

for arg in "$@"; do
    case "$arg" in
        --verbose|-v) VERBOSE=true ;;
    esac
done

cd "$SCRIPT_DIR"

log() { echo -e "\033[1m[NFS-KRB5]\033[0m $*"; }

log "Building and starting KDC + DittoFS..."
docker compose up -d --build dittofs

log "Waiting for DittoFS to be healthy..."
for i in $(seq 1 30); do
    if docker compose exec dittofs wget -qO- http://localhost:8080/health/ready >/dev/null 2>&1; then
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: DittoFS not healthy after 30s"
        docker compose logs dittofs
        docker compose down -v
        exit 1
    fi
    sleep 1
done

log "Bootstrapping DittoFS (stores, shares, users, adapter)..."
docker compose exec dittofs /bin/sh -c \
    'DFSCTL=/app/dfsctl API_URL=http://localhost:8080 /app/bootstrap.sh'

log "Running NFS Kerberos client tests..."
EXIT_CODE=0
if $VERBOSE; then
    docker compose up --build --abort-on-container-exit nfs-client || EXIT_CODE=$?
else
    docker compose up --build --abort-on-container-exit nfs-client 2>&1 | \
        grep -E '^\[NFS-TEST\]|nfs-client.*exited' || EXIT_CODE=$?
fi

log "Collecting logs..."
RESULTS_DIR="$SCRIPT_DIR/results/nfs-krb5-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$RESULTS_DIR"
docker compose logs dittofs > "$RESULTS_DIR/dittofs.log" 2>&1 || true
docker compose logs kdc > "$RESULTS_DIR/kdc.log" 2>&1 || true
docker compose logs nfs-client > "$RESULTS_DIR/nfs-client.log" 2>&1 || true

log "Cleaning up..."
docker compose down -v

if [ "$EXIT_CODE" -eq 0 ]; then
    log "ALL TESTS PASSED"
else
    log "TESTS FAILED (exit code: $EXIT_CODE)"
    log "Results: $RESULTS_DIR"
fi

exit "$EXIT_CODE"
