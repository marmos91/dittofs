#!/usr/bin/env bash
# juicefs-s3.sh — JuiceFS with S3 backend + NFS re-export
#
# Installs JuiceFS Community Edition, formats a filesystem with SQLite
# for metadata and S3 for object storage, FUSE-mounts it at /export,
# then re-exports via the kernel NFS server so clients can mount over NFS.
#
# Architecture:
#   Client --NFS--> kernel-nfsd --local--> /export (FUSE mount) --JuiceFS--> SQLite + S3
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
EXPORT_DIR="${EXPORT_DIR:-/export}"
NFS_PORT="${NFS_PORT:-2049}"
JUICEFS_VERSION="${JUICEFS_VERSION:-1.2.2}"
JUICEFS_NAME="${JUICEFS_NAME:-dittofs-bench-s3}"
JUICEFS_META="${JUICEFS_META:-/data/juicefs-meta}"
SQLITE_URL="sqlite3://${JUICEFS_META}/juicefs.db"
ALLOWED_NETWORK="${ALLOWED_NETWORK:-*}"

# S3 configuration — must be provided via environment.
: "${S3_BUCKET:?S3_BUCKET environment variable is required}"
: "${S3_REGION:?S3_REGION environment variable is required}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID environment variable is required}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY environment variable is required}"
S3_ENDPOINT="${S3_ENDPOINT:-}"

# Build the S3 bucket URL for JuiceFS.
if [ -n "${S3_ENDPOINT}" ]; then
    JUICEFS_BUCKET="https://${S3_ENDPOINT}/${S3_BUCKET}"
else
    JUICEFS_BUCKET="https://s3.${S3_REGION}.scw.cloud/${S3_BUCKET}"
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[juicefs-s3] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# Stop handler: juicefs-s3.sh stop
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    log "Stopping JuiceFS S3 and NFS..."
    systemctl stop nfs-kernel-server 2>/dev/null || true
    systemctl disable nfs-kernel-server 2>/dev/null || true
    systemctl stop juicefs-mount 2>/dev/null || true
    systemctl disable juicefs-mount 2>/dev/null || true
    umount "${EXPORT_DIR}" 2>/dev/null || fusermount -u "${EXPORT_DIR}" 2>/dev/null || true
    # Destroy JuiceFS filesystem to clean S3 data.
    if command -v juicefs &>/dev/null; then
        log "Destroying JuiceFS filesystem to clean S3..."
        juicefs destroy --yes "${SQLITE_URL}" "${JUICEFS_NAME}" 2>/dev/null || true
    fi
    rm -rf "${JUICEFS_META:?}"/* 2>/dev/null || true
    log "Stopped."
    exit 0
fi

# ---------------------------------------------------------------------------
# 1. Install JuiceFS
# ---------------------------------------------------------------------------
if command -v juicefs &>/dev/null; then
    log "JuiceFS already installed: $(juicefs version 2>&1 | head -1)"
else
    log "Installing JuiceFS ${JUICEFS_VERSION}..."
    JUICEFS_ARCH="amd64"
    JUICEFS_URL="https://github.com/juicedata/juicefs/releases/download/v${JUICEFS_VERSION}/juicefs-${JUICEFS_VERSION}-linux-${JUICEFS_ARCH}.tar.gz"

    curl -fsSL "${JUICEFS_URL}" -o /tmp/juicefs.tar.gz
    tar -xzf /tmp/juicefs.tar.gz -C /tmp juicefs
    mv /tmp/juicefs /usr/local/bin/juicefs
    chmod +x /usr/local/bin/juicefs
    rm -f /tmp/juicefs.tar.gz
fi

log "JuiceFS version: $(juicefs version 2>&1 | head -1)"

# ---------------------------------------------------------------------------
# 2. Create metadata directory and export mount point
# ---------------------------------------------------------------------------
log "Creating JuiceFS directories..."
mkdir -p "${JUICEFS_META}"
mkdir -p "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 3. Format JuiceFS filesystem (SQLite metadata + S3 storage)
# ---------------------------------------------------------------------------
log "Formatting JuiceFS filesystem '${JUICEFS_NAME}' with SQLite + S3..."

# Check if already formatted (idempotent).
if juicefs status "${SQLITE_URL}" &>/dev/null 2>&1; then
    log "JuiceFS filesystem already formatted, skipping."
else
    juicefs format \
        --storage s3 \
        --bucket "${JUICEFS_BUCKET}" \
        --access-key "${AWS_ACCESS_KEY_ID}" \
        --secret-key "${AWS_SECRET_ACCESS_KEY}" \
        "${SQLITE_URL}" \
        "${JUICEFS_NAME}"
    log "JuiceFS filesystem formatted with S3 backend."
fi

# ---------------------------------------------------------------------------
# 4. Mount JuiceFS via FUSE
# ---------------------------------------------------------------------------
log "Mounting JuiceFS at ${EXPORT_DIR}..."

# Unmount if already mounted (idempotent).
if mountpoint -q "${EXPORT_DIR}"; then
    umount "${EXPORT_DIR}" || fusermount -u "${EXPORT_DIR}" || true
fi

# Create systemd mount service for JuiceFS.
# Pass S3 credentials via environment so JuiceFS can access S3.
cat > /etc/systemd/system/juicefs-mount.service <<SERVICE
[Unit]
Description=JuiceFS FUSE Mount (S3 Backend)

[Service]
Type=simple
Environment=AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID}
Environment=AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY}
ExecStart=/usr/local/bin/juicefs mount ${SQLITE_URL} ${EXPORT_DIR} --no-syslog --cache-size 2048 --buffer-size 300
ExecStop=/bin/umount ${EXPORT_DIR}
Restart=on-failure
RestartSec=5

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload
systemctl enable --now juicefs-mount.service

# Wait for FUSE mount to be ready.
log "Waiting for JuiceFS FUSE mount..."
for i in $(seq 1 30); do
    if mountpoint -q "${EXPORT_DIR}"; then
        log "JuiceFS mounted at ${EXPORT_DIR}."
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: JuiceFS did not mount within 30 seconds."
        systemctl status juicefs-mount.service --no-pager
        journalctl -u juicefs-mount.service --no-pager -n 50
        exit 1
    fi
    sleep 1
done

# Set permissions on the mounted filesystem.
chmod 777 "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 5. Re-export via kernel NFS
# ---------------------------------------------------------------------------
log "Installing NFS server to re-export JuiceFS..."
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq nfs-kernel-server

# Configure exports.
if [ -f /etc/exports ]; then
    sed -i "\|^${EXPORT_DIR} |d" /etc/exports
fi

# fsid=1 is required for re-exporting FUSE mounts over NFS.
cat >> /etc/exports <<EXPORTS
${EXPORT_DIR} ${ALLOWED_NETWORK}(rw,sync,no_subtree_check,no_root_squash,fsid=1)
EXPORTS

log "NFS exports configured:"
cat /etc/exports

# Start NFS server.
systemctl enable --now nfs-kernel-server
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
log "=== JuiceFS S3 verification ==="
log "JuiceFS:   $(juicefs version 2>&1 | head -1)"
log "FUSE:      $(mountpoint -q ${EXPORT_DIR} && echo 'mounted' || echo 'NOT mounted')"
log "NFS:       $(systemctl is-active nfs-kernel-server)"
log "Port:      ${NFS_PORT}"
log "Export:    ${EXPORT_DIR}"
log "Metadata:  ${SQLITE_URL}"
log "Storage:   ${JUICEFS_BUCKET}"
log "NFS exports:"
exportfs -v
log "=== Setup complete ==="
