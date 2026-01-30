#!/usr/bin/env bash
# Setup script for POSIX compliance testing
#
# This script:
# 1. Starts the DittoFS server
# 2. Waits for it to be ready
# 3. Configures stores, shares, and adapters via the API
# 4. Mounts the NFS share
#
# Usage:
#   ./setup-posix.sh [config-type]
#
# Config types:
#   memory         - Memory metadata store (default)
#   badger         - BadgerDB metadata store
#   postgres       - PostgreSQL metadata store (requires running postgres)
#   memory-content - Memory metadata + memory payload store
#   cache-s3       - Memory metadata + S3 payload store (requires localstack)
#
# Example:
#   sudo ./setup-posix.sh memory

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
CONFIG_TYPE="${1:-memory}"
CONFIG_FILE="$SCRIPT_DIR/configs/config.yaml"

# Set paths based on config type
DATA_DIR="/tmp/dittofs-posix-${CONFIG_TYPE}"
export DITTOFS_DATABASE_SQLITE_PATH="${DATA_DIR}/controlplane.db"
export DITTOFS_CACHE_PATH="${DATA_DIR}/cache"

MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
API_PORT=8080
NFS_PORT=12049
TEST_PASSWORD="posix-test-password-123"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

# Check if running as root
if [[ $EUID -ne 0 ]]; then
    log_error "This script must be run as root (sudo)"
    exit 1
fi

# Check if config file exists
if [[ ! -f "$CONFIG_FILE" ]]; then
    log_error "Config file not found: $CONFIG_FILE"
    exit 1
fi

# Check if binaries exist
DITTOFS_BIN="$REPO_ROOT/dittofs"
DITTOFSCTL_BIN="$REPO_ROOT/dittofsctl"

if [[ ! -x "$DITTOFS_BIN" ]]; then
    log_info "Building dittofs..."
    (cd "$REPO_ROOT" && go build -o dittofs ./cmd/dittofs)
fi

if [[ ! -x "$DITTOFSCTL_BIN" ]]; then
    log_info "Building dittofsctl..."
    (cd "$REPO_ROOT" && go build -o dittofsctl ./cmd/dittofsctl)
fi

# Clean up any existing state
cleanup_existing() {
    log_info "Cleaning up existing state..."

    # Unmount if mounted
    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        log_info "Unmounting $MOUNT_POINT"
        umount -f "$MOUNT_POINT" 2>/dev/null || true
    fi

    # Stop existing server
    if pgrep -f "dittofs start" >/dev/null 2>&1; then
        log_info "Stopping existing DittoFS server"
        "$DITTOFS_BIN" stop --force 2>/dev/null || pkill -f "dittofs start" || true
        sleep 2
    fi

    # Clean up data directory for this config type
    rm -rf "$DATA_DIR"
    mkdir -p "$DATA_DIR"
}

# Wait for API to be ready
wait_for_api() {
    log_info "Waiting for API to be ready..."
    local max_attempts=30
    local attempt=1

    while [[ $attempt -le $max_attempts ]]; do
        if curl -s "http://localhost:$API_PORT/health" >/dev/null 2>&1; then
            log_info "API is ready"
            return 0
        fi
        sleep 1
        ((attempt++))
    done

    log_error "API failed to become ready after $max_attempts seconds"
    return 1
}

# Start DittoFS server
start_server() {
    log_info "Starting DittoFS server (config type: $CONFIG_TYPE)"

    # Create data directory
    mkdir -p "$DATA_DIR"

    # Start server in foreground (to capture admin password)
    local log_file="/tmp/dittofs-posix-server.log"

    "$DITTOFS_BIN" start --foreground --config "$CONFIG_FILE" > "$log_file" 2>&1 &
    local server_pid=$!

    # Wait a bit for the server to start and print the admin password
    sleep 3

    # Extract admin password from log (if this is first start)
    local admin_password
    admin_password=$(grep -o 'password: [^ ]*' "$log_file" 2>/dev/null | head -1 | awk '{print $2}' || echo "")

    if [[ -z "$admin_password" ]]; then
        # If no password in log, might be a restart - use a known test password
        log_warn "Could not extract admin password from log"
        log_warn "If this is a fresh start, check $log_file for the password"
        admin_password="$TEST_PASSWORD"
    fi

    echo "$admin_password" > /tmp/dittofs-admin-password
    echo "$server_pid" > /tmp/dittofs-server.pid

    wait_for_api
}

# Login and configure via API
configure_via_api() {
    log_info "Configuring DittoFS via API..."

    local admin_password
    admin_password=$(cat /tmp/dittofs-admin-password 2>/dev/null || echo "$TEST_PASSWORD")

    # Login
    log_info "Logging in as admin..."
    "$DITTOFSCTL_BIN" login --server "http://localhost:$API_PORT" --username admin --password "$admin_password" || {
        log_error "Failed to login. Admin password might be different."
        log_error "Check /tmp/dittofs-posix-server.log for the actual password"
        return 1
    }

    # Change password (required for new admin user)
    log_info "Changing admin password (first login requirement)..."
    "$DITTOFSCTL_BIN" user change-password --current "$admin_password" --new "$TEST_PASSWORD" 2>/dev/null || {
        log_info "Password already changed or change-password not required"
    }

    # Create metadata store based on config type
    log_info "Creating metadata store..."
    case "$CONFIG_TYPE" in
        memory|memory-content|cache-s3)
            "$DITTOFSCTL_BIN" store metadata add --name default --type memory
            ;;
        badger)
            "$DITTOFSCTL_BIN" store metadata add --name default --type badger \
                --config "{\"db_path\":\"${DATA_DIR}/metadata\"}"
            ;;
        postgres)
            "$DITTOFSCTL_BIN" store metadata add --name default --type postgres \
                --config '{"host":"localhost","port":5432,"user":"dittofs","password":"dittofs","database":"dittofs_test","sslmode":"disable","max_conns":50,"min_conns":10}'
            ;;
    esac

    # Create payload store based on config type
    log_info "Creating payload store..."
    case "$CONFIG_TYPE" in
        memory-content)
            "$DITTOFSCTL_BIN" store payload add --name default --type memory
            ;;
        cache-s3)
            "$DITTOFSCTL_BIN" store payload add --name default --type s3 \
                --config '{"bucket":"dittofs-posix-test","region":"us-east-1","endpoint":"http://localhost:4566","force_path_style":true}'
            ;;
        *)
            # Default: filesystem payload store
            local content_path="${DATA_DIR}/content"
            mkdir -p "$content_path"
            "$DITTOFSCTL_BIN" store payload add --name default --type filesystem \
                --config "{\"path\":\"$content_path\"}"
            ;;
    esac

    # Create share
    log_info "Creating share..."
    "$DITTOFSCTL_BIN" share create --name /export --metadata default --payload default

    # Enable NFS adapter
    log_info "Enabling NFS adapter..."
    "$DITTOFSCTL_BIN" adapter enable nfs --port $NFS_PORT

    # Wait for NFS adapter to start and register shares
    log_info "Waiting for NFS adapter to be ready..."
    sleep 3

    # Verify NFS port is listening
    if ! nc -zv localhost $NFS_PORT 2>&1; then
        log_error "NFS adapter failed to start on port $NFS_PORT"
        cat /tmp/dittofs-posix-server.log | tail -50
        return 1
    fi

    log_info "API configuration complete"
}

# Mount NFS share
mount_nfs() {
    log_info "Mounting NFS share..."

    mkdir -p "$MOUNT_POINT"

    # Mount with NFSv3
    # noac disables attribute caching to ensure fresh attributes for tests
    # that delete and recreate files with the same name
    # sync forces synchronous operations to prevent SETATTR coalescing issues
    # lookupcache=none disables name lookup caching
    mount -t nfs -o nfsvers=3,tcp,port=$NFS_PORT,mountport=$NFS_PORT,nolock,noac,sync,lookupcache=none \
        localhost:/export "$MOUNT_POINT"

    log_info "NFS share mounted at $MOUNT_POINT"
}

# Main
main() {
    log_info "Setting up POSIX tests with config type: $CONFIG_TYPE"

    cleanup_existing
    start_server
    configure_via_api
    mount_nfs

    echo ""
    log_info "Setup complete!"
    log_info ""
    log_info "Mount point: $MOUNT_POINT"
    log_info "Server log:  /tmp/dittofs-posix-server.log"
    log_info "Data dir:    $DATA_DIR"
    log_info ""
    log_info "To run POSIX tests:"
    log_info "  cd $MOUNT_POINT"
    log_info "  sudo env PATH=\"\$PATH\" $SCRIPT_DIR/run-posix.sh"
    log_info ""
    log_info "To clean up:"
    log_info "  sudo $SCRIPT_DIR/teardown-posix.sh"
}

main "$@"
