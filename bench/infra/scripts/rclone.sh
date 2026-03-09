#!/usr/bin/env bash
# rclone.sh — Rclone NFS serve setup
#
# Installs rclone and configures it to serve a local directory via NFS
# using `rclone serve nfs`. Creates a systemd service for reliability.
# This tests rclone's NFS server implementation as a comparison point.
#
# Usage: Runs on the ephemeral server VM after restoring from base snapshot.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
EXPORT_DIR="${EXPORT_DIR:-/export}"
NFS_PORT="${NFS_PORT:-2049}"
RCLONE_ADDR="${RCLONE_ADDR:-:${NFS_PORT}}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[rclone] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# Stop handler: rclone.sh stop
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    log "Stopping rclone NFS server..."
    systemctl stop rclone-nfs 2>/dev/null || true
    systemctl disable rclone-nfs 2>/dev/null || true
    rm -rf "${EXPORT_DIR:?}"/*
    log "Stopped."
    exit 0
fi

# ---------------------------------------------------------------------------
# 1. Stop kernel NFS if running (conflicts on same port)
# ---------------------------------------------------------------------------
log "Ensuring kernel NFS server is stopped..."
systemctl stop nfs-kernel-server 2>/dev/null || true
systemctl disable nfs-kernel-server 2>/dev/null || true

# Also stop Ganesha if present.
systemctl stop nfs-ganesha 2>/dev/null || true
systemctl disable nfs-ganesha 2>/dev/null || true

# ---------------------------------------------------------------------------
# 2. Install rclone
# ---------------------------------------------------------------------------
if command -v rclone &>/dev/null; then
    log "Rclone already installed: $(rclone version | head -1)"
else
    log "Installing rclone..."
    curl -fsSL https://rclone.org/install.sh | bash
fi

log "Rclone version: $(rclone version | head -1)"

# ---------------------------------------------------------------------------
# 3. Create export directory
# ---------------------------------------------------------------------------
log "Creating export directory ${EXPORT_DIR}..."
mkdir -p "${EXPORT_DIR}"
chmod 777 "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 4. Create systemd service for rclone NFS
# ---------------------------------------------------------------------------
log "Creating systemd service for rclone NFS..."
cat > /etc/systemd/system/rclone-nfs.service <<SERVICE
[Unit]
Description=Rclone NFS Server
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/rclone serve nfs ${EXPORT_DIR} --addr ${RCLONE_ADDR} --vfs-cache-mode writes
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload

# ---------------------------------------------------------------------------
# 5. Start rclone NFS server
# ---------------------------------------------------------------------------
log "Starting rclone NFS server on ${RCLONE_ADDR}..."
systemctl enable --now rclone-nfs.service

log "Waiting for rclone NFS to start on port ${NFS_PORT}..."
for i in $(seq 1 30); do
    if ss -tlnp | grep -q ":${NFS_PORT} "; then
        log "Rclone NFS is listening on port ${NFS_PORT}."
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: Rclone NFS did not start within 30 seconds."
        systemctl status rclone-nfs.service --no-pager
        journalctl -u rclone-nfs.service --no-pager -n 50
        exit 1
    fi
    sleep 1
done

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
log "=== Rclone NFS verification ==="
log "Service: $(systemctl is-active rclone-nfs.service)"
log "Port:    ${NFS_PORT}"
log "Export:  ${EXPORT_DIR}"
log "Rclone:  $(rclone version | head -1)"
log "Listening:"
ss -tlnp | grep ":${NFS_PORT} " || true
log "=== Setup complete ==="
