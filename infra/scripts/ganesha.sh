#!/usr/bin/env bash
# ganesha.sh — NFS-Ganesha userspace NFS server setup
#
# Installs and configures NFS-Ganesha with the VFS FSAL (filesystem
# abstraction layer) to serve /export via NFSv3 and NFSv4. This provides
# a userspace NFS server comparison point against the kernel NFS server
# and DittoFS.
#
# Usage: Runs on the ephemeral server VM after restoring from base snapshot.

set -euo pipefail

# ---------------------------------------------------------------------------
# Stop handler: called by run-all.sh cleanup as "bash script.sh stop"
# ---------------------------------------------------------------------------
if [ "${1:-}" = "stop" ]; then
    systemctl stop nfs-ganesha 2>/dev/null || true
    systemctl disable nfs-ganesha 2>/dev/null || true
    rm -rf /export/*
    exit 0
fi

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
EXPORT_DIR="${EXPORT_DIR:-/export}"
NFS_PORT="${NFS_PORT:-2049}"
GANESHA_CONF="${GANESHA_CONF:-/etc/ganesha/ganesha.conf}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[ganesha] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# 1. Stop kernel NFS if running (conflicts with Ganesha on same port)
# ---------------------------------------------------------------------------
log "Ensuring kernel NFS server is stopped..."
systemctl stop nfs-kernel-server 2>/dev/null || true
systemctl disable nfs-kernel-server 2>/dev/null || true

# ---------------------------------------------------------------------------
# 2. Install NFS-Ganesha
# ---------------------------------------------------------------------------
log "Installing nfs-ganesha and VFS FSAL..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq nfs-ganesha nfs-ganesha-vfs

# ---------------------------------------------------------------------------
# 3. Create export directory
# ---------------------------------------------------------------------------
log "Creating export directory ${EXPORT_DIR}..."
mkdir -p "${EXPORT_DIR}"
chmod 777 "${EXPORT_DIR}"

# ---------------------------------------------------------------------------
# 4. Configure NFS-Ganesha
# ---------------------------------------------------------------------------
log "Writing Ganesha config to ${GANESHA_CONF}..."
mkdir -p "$(dirname "${GANESHA_CONF}")"

cat > "${GANESHA_CONF}" <<CONF
# NFS-Ganesha configuration for DittoFS benchmarking

NFS_CORE_PARAM {
    NFS_Port = ${NFS_PORT};
    NFS_Protocols = 3, 4;
    Bind_Addr = 0.0.0.0;
}

NFSV4 {
    Grace_Period = 10;
    Lease_Lifetime = 30;
}

EXPORT_DEFAULTS {
    Access_Type = RW;
    Squash = No_Root_Squash;
    SecType = sys, none;
    Protocols = 3, 4;
    Transports = TCP;
}

LOG {
    Default_Log_Level = EVENT;

    COMPONENTS {
        ALL = EVENT;
    }
}

EXPORT {
    Export_Id = 1;
    Path = ${EXPORT_DIR};
    Pseudo = /export;

    FSAL {
        Name = VFS;
    }

    Access_Type = RW;
    Squash = No_Root_Squash;
    SecType = sys, none;
    Protocols = 3, 4;
    Transports = TCP;

    CLIENT {
        Clients = *;
        Access_Type = RW;
    }
}
CONF

log "Ganesha config written."

# ---------------------------------------------------------------------------
# 5. Start NFS-Ganesha
# ---------------------------------------------------------------------------
log "Starting NFS-Ganesha..."
systemctl enable --now nfs-ganesha

log "Waiting for Ganesha to start on port ${NFS_PORT}..."
for i in $(seq 1 30); do
    if ss -tlnp | grep -q ":${NFS_PORT} "; then
        log "NFS-Ganesha is listening on port ${NFS_PORT}."
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: NFS-Ganesha did not start within 30 seconds."
        systemctl status nfs-ganesha --no-pager
        journalctl -u nfs-ganesha --no-pager -n 50
        exit 1
    fi
    sleep 1
done

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
log "=== NFS-Ganesha verification ==="
log "Service: $(systemctl is-active nfs-ganesha)"
log "Port:    ${NFS_PORT}"
log "Config:  ${GANESHA_CONF}"
log "Export:  ${EXPORT_DIR}"
log "Listening:"
ss -tlnp | grep ":${NFS_PORT} " || true
log "=== Setup complete ==="
