#!/usr/bin/env bash
# bootstrap-dittofs.sh - Create stores, shares, and adapters for a DittoFS benchmark profile
#
# Provisions a running DittoFS instance with the correct store backends,
# an /export share, and the appropriate protocol adapter.
#
# Usage:
#   docker compose exec <service> /app/bootstrap.sh badger-s3
#   docker compose exec <service> /app/bootstrap.sh postgres-s3
#   docker compose exec <service> /app/bootstrap.sh badger-fs
#   docker compose exec <service> /app/bootstrap.sh smb

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Source common.sh if available (when running from host); skip inside container
if [ -f "${SCRIPT_DIR}/lib/common.sh" ]; then
    # shellcheck source=lib/common.sh
    source "${SCRIPT_DIR}/lib/common.sh"
else
    # Minimal logging fallback when running inside container without common.sh
    log_info()  { echo "[INFO]  $*" >&2; }
    log_warn()  { echo "[WARN]  $*" >&2; }
    log_error() { echo "[ERROR] $*" >&2; }
    die()       { log_error "$@"; exit 1; }
fi

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
PROFILE="${1:?Usage: bootstrap-dittofs.sh <profile> (badger-s3|postgres-s3|badger-fs|smb)}"

# Validate profile upfront to fail fast with a clear message
case "$PROFILE" in
    badger-s3|postgres-s3|badger-fs|smb) ;;
    *) die "Unknown profile: ${PROFILE}. Valid profiles: badger-s3, postgres-s3, badger-fs, smb" ;;
esac

DFSCTL="${DFSCTL:-/app/dfsctl}"
SERVER="${SERVER:-http://localhost:8080}"
ADMIN_PASSWORD="${DITTOFS_CONTROLPLANE_SECRET:-BenchmarkInfrastructureSecret32ch!}"

# S3 configuration (from environment)
S3_BUCKET="${S3_BUCKET:-bench}"
S3_ENDPOINT="${S3_ENDPOINT:-http://localstack:4566}"

# PostgreSQL configuration (from environment)
PG_HOST="${POSTGRES_HOST:-postgres}"
PG_PORT="${POSTGRES_PORT:-5432}"
PG_USER="${POSTGRES_USER:-bench}"
PG_PASS="${POSTGRES_PASSWORD:-bench}"
PG_DB="${POSTGRES_DB:-bench}"

# ---------------------------------------------------------------------------
# Wait for DittoFS API to be ready
# ---------------------------------------------------------------------------
wait_for_ready() {
    local max_attempts=60
    local attempt=1

    log_info "Waiting for DittoFS API at ${SERVER}/health/ready ..."

    while [ "$attempt" -le "$max_attempts" ]; do
        if curl -sf "${SERVER}/health/ready" >/dev/null 2>&1; then
            log_info "DittoFS API is ready (attempt ${attempt})"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    die "DittoFS API not ready after ${max_attempts}s"
}

# ---------------------------------------------------------------------------
# Create metadata store based on profile
# ---------------------------------------------------------------------------
create_metadata_store() {
    case "$PROFILE" in
        badger-s3|badger-fs|smb)
            log_info "Creating BadgerDB metadata store..."
            "$DFSCTL" store metadata add --name default --type badger \
                --config '{"db_path":"/data/metadata"}'
            ;;
        postgres-s3)
            log_info "Creating PostgreSQL metadata store..."
            "$DFSCTL" store metadata add --name default --type postgres \
                --config "{\"host\":\"${PG_HOST}\",\"port\":${PG_PORT},\"user\":\"${PG_USER}\",\"password\":\"${PG_PASS}\",\"database\":\"${PG_DB}\",\"sslmode\":\"disable\"}"
            ;;
    esac
}

# ---------------------------------------------------------------------------
# Create payload store based on profile
# ---------------------------------------------------------------------------
create_payload_store() {
    case "$PROFILE" in
        badger-s3|postgres-s3|smb)
            log_info "Creating S3 payload store..."
            "$DFSCTL" store payload add --name default --type s3 \
                --config "{\"bucket\":\"${S3_BUCKET}\",\"region\":\"us-east-1\",\"endpoint\":\"${S3_ENDPOINT}\",\"force_path_style\":true}"
            ;;
        badger-fs)
            log_info "Creating filesystem payload store..."
            "$DFSCTL" store payload add --name default --type filesystem \
                --config '{"path":"/data/content"}'
            ;;
    esac
}

# ---------------------------------------------------------------------------
# Create adapter based on profile
# ---------------------------------------------------------------------------
create_adapter() {
    case "$PROFILE" in
        badger-s3|postgres-s3|badger-fs)
            log_info "Enabling NFS adapter on port 12049..."
            "$DFSCTL" adapter enable nfs --port 12049
            ;;
        smb)
            log_info "Enabling SMB adapter on port 12445..."
            "$DFSCTL" adapter enable smb --port 12445
            ;;
    esac
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    log_info "Starting DittoFS bootstrap (profile: ${PROFILE})"

    wait_for_ready

    # Login as admin
    log_info "Logging in as admin..."
    "$DFSCTL" login --server "$SERVER" --username admin --password "$ADMIN_PASSWORD"

    # Create stores
    create_metadata_store
    create_payload_store

    # Create share
    log_info "Creating /export share..."
    "$DFSCTL" share create --name /export --metadata default --payload default

    # Enable adapter
    create_adapter

    log_info "Bootstrap complete (profile: ${PROFILE})"
}

main
