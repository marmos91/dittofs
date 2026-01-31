#!/usr/bin/env bash
# Teardown script for POSIX compliance testing
#
# This script:
# 1. Unmounts the NFS share
# 2. Stops the DittoFS server
# 3. Cleans up temporary files
#
# Usage:
#   sudo ./teardown-posix.sh

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
DITTOFS_BIN="$REPO_ROOT/dittofs"

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

log_info "Tearing down POSIX test environment..."

# Unmount NFS share
if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
    log_info "Unmounting $MOUNT_POINT"
    umount -f "$MOUNT_POINT" || {
        log_warn "Normal unmount failed, trying lazy unmount..."
        umount -l "$MOUNT_POINT" || true
    }
else
    log_info "Mount point not mounted"
fi

# Stop DittoFS server
if [[ -f /tmp/dittofs-server.pid ]]; then
    pid=$(cat /tmp/dittofs-server.pid)
    if kill -0 "$pid" 2>/dev/null; then
        log_info "Stopping DittoFS server (PID: $pid)"
        kill -TERM "$pid" || true
        sleep 2
        kill -0 "$pid" 2>/dev/null && kill -KILL "$pid" 2>/dev/null || true
    fi
    rm -f /tmp/dittofs-server.pid
fi

# Also try to stop via dittofs stop command
"$DITTOFS_BIN" stop --force 2>/dev/null || true

# Kill any remaining dittofs processes
pkill -f "dittofs start" 2>/dev/null || true

# Clean up temporary files
log_info "Cleaning up temporary files..."
rm -rf /tmp/dittofs-posix-*
rm -f /tmp/dittofs-admin-password
rm -f /tmp/dittofs-server.pid
rm -f /tmp/dittofs-posix-server.log

# Remove mount point directory
rmdir "$MOUNT_POINT" 2>/dev/null || true

log_info "Teardown complete!"
