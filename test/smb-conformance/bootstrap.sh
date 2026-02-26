#!/bin/sh
# bootstrap.sh - Configure DittoFS with WPTS-required stores, shares, and users
#
# This script provisions a running DittoFS instance for Microsoft
# WindowsProtocolTestSuites (WPTS) SMB conformance testing.
#
# Works in both Docker Compose mode (dfsctl inside container) and
# local mode (dfsctl on host).
#
# Usage:
#   PROFILE=memory ./bootstrap.sh
#   DFSCTL=/app/dfsctl API_URL=http://localhost:8080 ./bootstrap.sh

set -euo pipefail

# Configuration (overridable via environment)
DFSCTL="${DFSCTL:-dfsctl}"
API_URL="${API_URL:-http://localhost:8080}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-${DITTOFS_CONTROLPLANE_SECRET:-WptsConformanceTesting2026!Secret}}"
TEST_PASSWORD="${TEST_PASSWORD:-TestPassword01!}"
PROFILE="${PROFILE:-memory}"
SMB_PORT="${SMB_PORT:-12445}"

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
NC='\033[0m'

log_info() { echo -e "${GREEN}[BOOTSTRAP]${NC} $*"; }
log_error() { echo -e "${RED}[BOOTSTRAP]${NC} $*"; }

# Wait for DittoFS API to be ready
wait_for_ready() {
    local max=30
    local attempt=1

    log_info "Waiting for DittoFS API at ${API_URL}/health/ready ..."

    while [ "$attempt" -le "$max" ]; do
        if curl -sf "${API_URL}/health/ready" >/dev/null 2>&1; then
            log_info "DittoFS API is ready"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "DittoFS not ready after ${max}s"
    return 1
}

# Wait for SMB port to be accepting connections
wait_for_smb() {
    local max=15
    local attempt=1
    local host="${1:-localhost}"

    log_info "Waiting for SMB adapter on ${host}:${SMB_PORT}..."

    while [ "$attempt" -le "$max" ]; do
        # Try nc first, fall back to /dev/tcp for minimal containers without nc
        if nc -z "$host" "$SMB_PORT" 2>/dev/null || (echo >/dev/tcp/"$host"/"$SMB_PORT") 2>/dev/null; then
            log_info "SMB adapter is listening on port ${SMB_PORT}"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    log_error "SMB adapter not listening after ${max}s"
    return 1
}

# Create metadata store based on profile
create_metadata_store() {
    log_info "Creating metadata store for profile: ${PROFILE}"

    case "$PROFILE" in
        memory|memory-fs)
            $DFSCTL store metadata add --name default --type memory
            ;;
        badger*)
            $DFSCTL store metadata add --name default --type badger \
                --config '{"db_path":"/data/metadata"}'
            ;;
        postgres*)
            $DFSCTL store metadata add --name default --type postgres \
                --config '{"host":"postgres","port":5432,"user":"dittofs","password":"dittofs","database":"dittofs_test","sslmode":"disable"}'
            ;;
        *)
            log_error "Unknown profile: ${PROFILE}"
            return 1
            ;;
    esac
}

# Create payload store based on profile
create_payload_store() {
    log_info "Creating payload store for profile: ${PROFILE}"

    case "$PROFILE" in
        memory)
            $DFSCTL store payload add --name default --type memory
            ;;
        *-fs)
            $DFSCTL store payload add --name default --type filesystem \
                --config '{"path":"/data/content"}'
            ;;
        *-s3)
            $DFSCTL store payload add --name default --type s3 \
                --config '{"bucket":"dittofs-test","region":"us-east-1","endpoint":"http://localstack:4566","force_path_style":true}'
            ;;
        *)
            log_error "Unknown profile payload pattern: ${PROFILE}"
            return 1
            ;;
    esac
}

# Main bootstrap flow
main() {
    log_info "Starting DittoFS bootstrap (profile: ${PROFILE})"

    # Wait for API
    wait_for_ready

    # Login as admin
    log_info "Logging in as admin..."
    $DFSCTL login --server "$API_URL" --username admin --password "$ADMIN_PASSWORD"

    # Change password (required for new admin user on first login)
    log_info "Changing admin password (first login requirement)..."
    $DFSCTL user change-password --current "$ADMIN_PASSWORD" --new "$TEST_PASSWORD" 2>/dev/null || true

    # Re-login with new password
    $DFSCTL login --server "$API_URL" --username admin --password "$TEST_PASSWORD"

    # Create stores
    create_metadata_store
    create_payload_store

    # Create WPTS-required shares
    log_info "Creating WPTS shares..."
    $DFSCTL share create --name /SMBBasic --metadata default --payload default
    $DFSCTL share create --name /SMBEncrypted --metadata default --payload default

    # Create test users
    log_info "Creating test users..."
    $DFSCTL user create --username wpts-admin --password "$TEST_PASSWORD"
    $DFSCTL user create --username nonadmin --password "$TEST_PASSWORD"

    # Enable SMB adapter
    log_info "Enabling SMB adapter on port ${SMB_PORT}..."
    $DFSCTL adapter enable smb --port "$SMB_PORT"

    # Wait for SMB adapter to start
    wait_for_smb localhost

    log_info "Bootstrap complete: shares=SMBBasic,SMBEncrypted users=wpts-admin,nonadmin adapter=smb:${SMB_PORT}"
}

main "$@"
