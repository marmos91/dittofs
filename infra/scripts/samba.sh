#!/usr/bin/env bash
# samba.sh — Samba/CIFS server setup
#
# Installs and configures Samba to share /export as a CIFS/SMB share
# named [bench]. Creates a benchmark user with password authentication.
# This tests SMB protocol performance as a comparison point.
#
# Usage: Runs on the ephemeral server VM after restoring from base snapshot.

set -euo pipefail

# ---------------------------------------------------------------------------
# Stop handler: called by run-all.sh cleanup as "bash script.sh stop"
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    systemctl stop smbd 2>/dev/null || true
    systemctl stop nmbd 2>/dev/null || true
    rm -rf /export/*
    exit 0
fi

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
EXPORT_DIR="${EXPORT_DIR:-/export}"
SMB_PORT="${SMB_PORT:-445}"
SMB_SHARE_NAME="${SMB_SHARE_NAME:-bench}"
SMB_USER="${SMB_USER:-bench}"
SMB_PASSWORD="${SMB_PASSWORD:-bench}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[samba] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# 1. Install Samba
# ---------------------------------------------------------------------------
log "Installing Samba..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq samba

# ---------------------------------------------------------------------------
# 2. Create export directory
# ---------------------------------------------------------------------------
log "Creating export directory ${EXPORT_DIR}..."
mkdir -p "${EXPORT_DIR}"
chmod 777 "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 3. Create benchmark user
# ---------------------------------------------------------------------------
log "Creating benchmark user '${SMB_USER}'..."
if ! id "${SMB_USER}" &>/dev/null; then
    useradd --system --no-create-home --shell /usr/sbin/nologin "${SMB_USER}"
fi

# Set Samba password for the user.
printf '%s\n%s\n' "${SMB_PASSWORD}" "${SMB_PASSWORD}" | smbpasswd -s -a "${SMB_USER}"
smbpasswd -e "${SMB_USER}"

# Ensure the user owns the export dir.
chown "${SMB_USER}:${SMB_USER}" "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 4. Configure Samba
# ---------------------------------------------------------------------------
log "Configuring Samba..."

# Back up original config.
if [ -f /etc/samba/smb.conf ] && [ ! -f /etc/samba/smb.conf.orig ]; then
    cp /etc/samba/smb.conf /etc/samba/smb.conf.orig
fi

cat > /etc/samba/smb.conf <<CONF
[global]
    workgroup = WORKGROUP
    server string = DittoFS Bench Samba Server
    security = user
    map to guest = Never
    log file = /var/log/samba/log.%m
    max log size = 1000
    logging = file
    server role = standalone server

    # Performance tuning
    socket options = TCP_NODELAY IPTOS_LOWDELAY
    read raw = yes
    write raw = yes
    max xmit = 65535
    dead time = 15
    getwd cache = yes

    # SMB protocol versions
    server min protocol = SMB2
    server max protocol = SMB3

    # Listen on configurable port
    smb ports = ${SMB_PORT}

[${SMB_SHARE_NAME}]
    comment = Benchmark Share
    path = ${EXPORT_DIR}
    browseable = yes
    read only = no
    guest ok = no
    valid users = ${SMB_USER}
    create mask = 0666
    directory mask = 0777
    force user = ${SMB_USER}
CONF

log "Samba config written."

# Validate config.
testparm -s 2>/dev/null || {
    log "WARNING: Samba config validation reported issues."
}

# ---------------------------------------------------------------------------
# 5. Start Samba
# ---------------------------------------------------------------------------
log "Starting Samba..."
systemctl enable --now smbd

# nmbd is optional for benchmarking but start it anyway for completeness.
systemctl enable --now nmbd 2>/dev/null || true

log "Waiting for Samba to start on port ${SMB_PORT}..."
for i in $(seq 1 30); do
    if ss -tlnp | grep -q ":${SMB_PORT} "; then
        log "Samba is listening on port ${SMB_PORT}."
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: Samba did not start within 30 seconds."
        systemctl status smbd --no-pager
        exit 1
    fi
    sleep 1
done

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
log "=== Samba verification ==="
log "Service: $(systemctl is-active smbd)"
log "Port:    ${SMB_PORT}"
log "Share:   [${SMB_SHARE_NAME}] at ${EXPORT_DIR}"
log "User:    ${SMB_USER}"
log "Shares:"
smbclient -L localhost -U "${SMB_USER}%${SMB_PASSWORD}" 2>/dev/null || true
log "=== Setup complete ==="
