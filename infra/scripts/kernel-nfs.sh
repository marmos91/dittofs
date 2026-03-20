#!/usr/bin/env bash
# kernel-nfs.sh — Linux kernel NFS server setup
#
# Installs and configures the standard Linux kernel NFS server (knfsd)
# with an export at /export. This serves as the baseline reference for
# NFS performance benchmarking.
#
# Usage: Runs on the ephemeral server VM after restoring from base snapshot.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
EXPORT_DIR="${EXPORT_DIR:-/export}"
NFS_PORT="${NFS_PORT:-2049}"
NFS_THREADS="${NFS_THREADS:-8}"
ALLOWED_NETWORK="${ALLOWED_NETWORK:-*}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[kernel-nfs] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# Stop handler: kernel-nfs.sh stop
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    log "Stopping kernel NFS server..."
    systemctl stop nfs-kernel-server 2>/dev/null || true
    systemctl disable nfs-kernel-server 2>/dev/null || true
    rm -rf "${EXPORT_DIR:?}"/*
    log "Stopped."
    exit 0
fi

# ---------------------------------------------------------------------------
# 1. Install NFS kernel server
# ---------------------------------------------------------------------------
log "Installing nfs-kernel-server..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq nfs-kernel-server

# ---------------------------------------------------------------------------
# 2. Create export directory
# ---------------------------------------------------------------------------
log "Creating export directory ${EXPORT_DIR}..."
mkdir -p "${EXPORT_DIR}"
chmod 777 "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 3. Configure exports
# ---------------------------------------------------------------------------
log "Configuring /etc/exports..."
# Remove any existing entry for our export path to stay idempotent.
if [ -f /etc/exports ]; then
    sed -i "\|^${EXPORT_DIR} |d" /etc/exports
fi

cat >> /etc/exports <<EXPORTS
${EXPORT_DIR} ${ALLOWED_NETWORK}(rw,sync,no_subtree_check,no_root_squash,fsid=0)
EXPORTS

log "Exports configured:"
cat /etc/exports

# ---------------------------------------------------------------------------
# 4. Configure NFS server options
# ---------------------------------------------------------------------------
log "Configuring NFS server threads and options..."
if [ -f /etc/default/nfs-kernel-server ]; then
    sed -i "s/^RPCNFSDCOUNT=.*/RPCNFSDCOUNT=${NFS_THREADS}/" /etc/default/nfs-kernel-server
fi

# ---------------------------------------------------------------------------
# 5. Start NFS server
# ---------------------------------------------------------------------------
log "Starting NFS server..."
systemctl enable --now nfs-kernel-server

# Re-export to pick up any changes.
exportfs -ra

log "Waiting for NFS server to start on port ${NFS_PORT}..."
for i in $(seq 1 30); do
    if ss -tlnp | grep -q ":${NFS_PORT} " || rpcinfo -p 2>/dev/null | grep -q nfs; then
        log "NFS server is running."
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: NFS server did not start within 30 seconds."
        systemctl status nfs-kernel-server --no-pager
        exit 1
    fi
    sleep 1
done

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
log "=== Kernel NFS verification ==="
log "Service:  $(systemctl is-active nfs-kernel-server)"
log "Port:     ${NFS_PORT}"
log "Threads:  ${NFS_THREADS}"
log "Exports:"
exportfs -v
log "RPC info:"
rpcinfo -p 2>/dev/null | grep nfs || true
log "=== Setup complete ==="
