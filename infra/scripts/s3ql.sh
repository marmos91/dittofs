#!/usr/bin/env bash
# s3ql.sh — s3ql FUSE filesystem with S3 backend + NFS re-export
#
# Installs s3ql, formats a filesystem on S3, FUSE-mounts it at /export,
# then re-exports via the kernel NFS server so clients can mount over NFS.
#
# Architecture:
#   Client --NFS--> kernel-nfsd --local--> /export (s3ql FUSE) --> S3
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
ALLOWED_NETWORK="${ALLOWED_NETWORK:-*}"

# S3 configuration — must be provided via environment.
: "${S3_BUCKET:?S3_BUCKET environment variable is required}"
: "${S3_REGION:?S3_REGION environment variable is required}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID environment variable is required}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY environment variable is required}"
S3_ENDPOINT="${S3_ENDPOINT:-}"

# Build the s3ql storage URL.
# s3ql uses s3c:// for S3-compatible endpoints.
if [ -n "${S3_ENDPOINT}" ]; then
    S3QL_URL="s3c://${S3_ENDPOINT}:443/${S3_BUCKET}/s3ql/"
else
    S3QL_URL="s3c://s3.${S3_REGION}.scw.cloud:443/${S3_BUCKET}/s3ql/"
fi

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[s3ql] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# Stop handler: s3ql.sh stop
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    log "Stopping s3ql and NFS..."
    systemctl stop nfs-kernel-server 2>/dev/null || true
    systemctl disable nfs-kernel-server 2>/dev/null || true
    # s3ql requires its own unmount command for clean shutdown.
    umount.s3ql "${EXPORT_DIR}" 2>/dev/null || fusermount -u "${EXPORT_DIR}" 2>/dev/null || umount -f "${EXPORT_DIR}" 2>/dev/null || true
    log "Stopped."
    exit 0
fi

# ---------------------------------------------------------------------------
# 1. Install s3ql
# ---------------------------------------------------------------------------
if command -v mount.s3ql &>/dev/null; then
    log "s3ql already installed: $(mount.s3ql --version 2>&1 || true)"
else
    log "Installing s3ql..."
    export DEBIAN_FRONTEND=noninteractive
    apt-get update -qq
    apt-get install -y -qq libfuse3-dev fuse3 pkg-config libsqlite3-dev python3-pip python3-dev
    pip3 install --break-system-packages git+https://github.com/s3ql/s3ql.git
fi

log "s3ql version: $(mount.s3ql --version 2>&1 || echo 'unknown')"

# ---------------------------------------------------------------------------
# 2. Write s3ql auth file
# ---------------------------------------------------------------------------
log "Writing s3ql auth file..."
mkdir -p /root/.s3ql
cat > /root/.s3ql/authinfo2 <<AUTH
[s3ql]
storage-url: ${S3QL_URL}
backend-login: ${AWS_ACCESS_KEY_ID}
backend-password: ${AWS_SECRET_ACCESS_KEY}
AUTH
chmod 600 /root/.s3ql/authinfo2

# ---------------------------------------------------------------------------
# 3. Create export directory
# ---------------------------------------------------------------------------
log "Creating export directory ${EXPORT_DIR}..."
mkdir -p "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 4. Format s3ql filesystem (if not already formatted)
# ---------------------------------------------------------------------------
log "Formatting s3ql filesystem at ${S3QL_URL}..."

# --plain disables encryption (not needed for benchmarking).
# mkfs.s3ql will fail if already formatted, so we check first.
if s3qladm --batch passphrase "${S3QL_URL}" </dev/null 2>/dev/null; then
    log "s3ql filesystem already formatted, skipping."
else
    mkfs.s3ql --plain "${S3QL_URL}"
    log "s3ql filesystem formatted."
fi

# ---------------------------------------------------------------------------
# 5. Mount s3ql FUSE filesystem
# ---------------------------------------------------------------------------
log "Mounting s3ql at ${EXPORT_DIR}..."

# Unmount if already mounted (idempotent).
if mountpoint -q "${EXPORT_DIR}"; then
    umount.s3ql "${EXPORT_DIR}" 2>/dev/null || fusermount -u "${EXPORT_DIR}" 2>/dev/null || true
fi

# --allow-other lets NFS server access the FUSE mount.
# --nfs flag optimizes for NFS re-export.
mount.s3ql --allow-other --nfs --log syslog --cachesize 2097152 "${S3QL_URL}" "${EXPORT_DIR}"

# Wait for mount to be ready.
log "Waiting for s3ql FUSE mount..."
for i in $(seq 1 60); do
    if mountpoint -q "${EXPORT_DIR}"; then
        log "s3ql mounted at ${EXPORT_DIR}."
        break
    fi
    if [ "$i" -eq 60 ]; then
        log "ERROR: s3ql did not mount within 60 seconds."
        exit 1
    fi
    sleep 1
done

chmod 777 "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 6. Re-export via kernel NFS
# ---------------------------------------------------------------------------
log "Installing NFS server to re-export s3ql..."
export DEBIAN_FRONTEND=noninteractive
apt-get install -y -qq nfs-kernel-server

# Configure exports.
if [ -f /etc/exports ]; then
    sed -i "\|^${EXPORT_DIR} |d" /etc/exports
fi

# fsid=2 for s3ql (different from juicefs fsid=1).
cat >> /etc/exports <<EXPORTS
${EXPORT_DIR} ${ALLOWED_NETWORK}(rw,sync,no_subtree_check,no_root_squash,fsid=2)
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
log "=== s3ql verification ==="
log "s3ql:      $(mount.s3ql --version 2>&1 || echo 'unknown')"
log "FUSE:      $(mountpoint -q ${EXPORT_DIR} && echo 'mounted' || echo 'NOT mounted')"
log "NFS:       $(systemctl is-active nfs-kernel-server)"
log "Port:      ${NFS_PORT}"
log "Export:    ${EXPORT_DIR}"
log "Storage:   ${S3QL_URL}"
log "NFS exports:"
exportfs -v
log "=== Setup complete ==="
