#!/usr/bin/env bash
# rclone-s3.sh — Rclone NFS serve with S3 backend
#
# Installs rclone and configures it to serve an S3 bucket via NFS
# using `rclone serve nfs`. Creates a systemd service for reliability.
#
# Architecture:
#   Client --NFS--> rclone serve nfs --> S3
#
# Requires environment variables:
#   S3_BUCKET, S3_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY
#   S3_ENDPOINT (optional, for non-AWS S3 like Scaleway)
#
# Usage: Runs on the ephemeral server VM after restoring from base snapshot.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
NFS_PORT="${NFS_PORT:-2049}"
RCLONE_ADDR="${RCLONE_ADDR:-:${NFS_PORT}}"

# S3 configuration — must be provided via environment.
: "${S3_BUCKET:?S3_BUCKET environment variable is required}"
: "${S3_REGION:?S3_REGION environment variable is required}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID environment variable is required}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY environment variable is required}"
S3_ENDPOINT="${S3_ENDPOINT:-}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[rclone-s3] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# Stop handler: rclone-s3.sh stop
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    log "Stopping rclone S3 NFS server..."
    systemctl stop rclone-nfs 2>/dev/null || true
    systemctl disable rclone-nfs 2>/dev/null || true
    # Clean up the S3 prefix used by rclone.
    if command -v rclone &>/dev/null; then
        log "Purging rclone S3 prefix..."
        rclone purge scw:${S3_BUCKET}/rclone/ 2>/dev/null || true
    fi
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
# 3. Write rclone config for Scaleway S3
# ---------------------------------------------------------------------------
log "Writing rclone config for Scaleway S3..."
mkdir -p /root/.config/rclone

RCLONE_ENDPOINT="${S3_ENDPOINT}"
if [ -z "${RCLONE_ENDPOINT}" ]; then
    RCLONE_ENDPOINT="s3.${S3_REGION}.scw.cloud"
fi

cat > /root/.config/rclone/rclone.conf <<CONF
[scw]
type = s3
provider = Scaleway
access_key_id = ${AWS_ACCESS_KEY_ID}
secret_access_key = ${AWS_SECRET_ACCESS_KEY}
region = ${S3_REGION}
endpoint = ${RCLONE_ENDPOINT}
acl = private
CONF

log "Rclone config written."

# ---------------------------------------------------------------------------
# 4. Create systemd service for rclone NFS with S3 backend
# ---------------------------------------------------------------------------
log "Creating systemd service for rclone S3 NFS..."
cat > /etc/systemd/system/rclone-nfs.service <<SERVICE
[Unit]
Description=Rclone NFS Server (S3 Backend)
After=network.target

[Service]
Type=simple
ExecStart=/usr/bin/rclone serve nfs scw:${S3_BUCKET}/rclone/ --addr ${RCLONE_ADDR} --vfs-cache-mode writes --vfs-cache-max-size 2G
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
log "Starting rclone S3 NFS server on ${RCLONE_ADDR}..."
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
log "=== Rclone S3 NFS verification ==="
log "Service: $(systemctl is-active rclone-nfs.service)"
log "Port:    ${NFS_PORT}"
log "Backend: s3://${S3_BUCKET}/rclone/"
log "Rclone:  $(rclone version | head -1)"
log "Listening:"
ss -tlnp | grep ":${NFS_PORT} " || true
log "=== Setup complete ==="
