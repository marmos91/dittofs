#!/usr/bin/env bash
# dittofs-badger-fs.sh — DittoFS with BadgerDB metadata + filesystem payload
#
# Builds DittoFS from source, creates a configuration using BadgerDB for
# metadata and the local filesystem for payload storage, then starts the
# dfs server with an NFS export at /export on port 12049.
#
# Usage: Runs on the ephemeral server VM after restoring from base snapshot.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
EXPORT_DIR="${EXPORT_DIR:-/export}"
DATA_DIR="${DATA_DIR:-/data}"
NFS_PORT="${NFS_PORT:-12049}"
DITTOFS_REPO="${DITTOFS_REPO:-https://github.com/marmos91/dittofs.git}"
DITTOFS_BRANCH="${DITTOFS_BRANCH:-main}"
BADGER_PATH="${BADGER_PATH:-/data/metadata/badger}"
PAYLOAD_PATH="${PAYLOAD_PATH:-/data/payload}"
CONFIG_DIR="${CONFIG_DIR:-/etc/dfs}"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[dittofs-badger-fs] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# 1. Ensure Go is on PATH (from base snapshot)
# ---------------------------------------------------------------------------
export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"

# ---------------------------------------------------------------------------
# 2. Build DittoFS from source
# ---------------------------------------------------------------------------
DITTOFS_SRC="/opt/dittofs"
if [ -d "${DITTOFS_SRC}" ]; then
    cd "${DITTOFS_SRC}"
    git fetch origin "${DITTOFS_BRANCH}"
    git checkout "${DITTOFS_BRANCH}"
    git reset --hard "origin/${DITTOFS_BRANCH}"
else
    log "Cloning DittoFS repository..."
    git clone --branch "${DITTOFS_BRANCH}" --depth 1 "${DITTOFS_REPO}" "${DITTOFS_SRC}"
fi

cd "${DITTOFS_SRC}"
log "Building dfs server..."
go build -o /usr/local/bin/dfs ./cmd/dfs/
chmod +x /usr/local/bin/dfs
log "dfs binary built successfully."

# Also ensure dfsctl is up to date.
log "Building dfsctl..."
go build -o /usr/local/bin/dfsctl ./cmd/dfsctl/
chmod +x /usr/local/bin/dfsctl

# ---------------------------------------------------------------------------
# 3. Create directories
# ---------------------------------------------------------------------------
log "Creating data directories..."
mkdir -p "${EXPORT_DIR}"
mkdir -p "${BADGER_PATH}"
mkdir -p "${PAYLOAD_PATH}"
mkdir -p "${CONFIG_DIR}"

chmod 777 "${EXPORT_DIR}"
chmod 755 "${BADGER_PATH}"
chmod 755 "${PAYLOAD_PATH}"

# ---------------------------------------------------------------------------
# 4. Generate DittoFS config
# ---------------------------------------------------------------------------
# NOTE: Only static config goes in YAML. Dynamic resources (stores, shares,
# adapters) are created via the control plane REST API after server starts.
log "Writing DittoFS config to ${CONFIG_FILE}..."
cat > "${CONFIG_FILE}" <<YAML
logging:
  level: INFO
  format: text
  output: stdout

shutdown_timeout: 30s

cache:
  path: "${DATA_DIR}/cache"
  size: "512MB"
YAML

log "Config written."

# ---------------------------------------------------------------------------
# 5. Create systemd service
# ---------------------------------------------------------------------------
log "Creating systemd service for dfs..."
cat > /etc/systemd/system/dfs.service <<SERVICE
[Unit]
Description=DittoFS Server (BadgerDB + Filesystem)
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/dfs start --foreground --config ${CONFIG_FILE}
Restart=on-failure
RestartSec=5
LimitNOFILE=65536
Environment=DITTOFS_LOGGING_LEVEL=INFO
Environment=DITTOFS_CONTROLPLANE_SECRET=dittofs-bench-secret-key-for-jwt-1234567890
Environment=DITTOFS_ADMIN_INITIAL_PASSWORD=dittofs-bench-admin-1234567890

[Install]
WantedBy=multi-user.target
SERVICE

systemctl daemon-reload

# ---------------------------------------------------------------------------
# 6. Start DittoFS
# ---------------------------------------------------------------------------
# Remove any stale control plane database from prior runs so that
# admin user is created fresh with our known password.
# systemd may resolve HOME=/ for root, so clean both locations.
log "Removing stale control plane database..."
systemctl stop dfs.service 2>/dev/null || true
rm -rf /.config/dittofs /root/.config/dittofs

log "Starting DittoFS server..."
systemctl enable --now dfs.service

# Wait for the control plane API to be ready (port 8080).
log "Waiting for DittoFS API on port 8080..."
for i in $(seq 1 30); do
    if ss -tlnp | grep -q ":8080 "; then
        log "DittoFS API is listening on port 8080."
        break
    fi
    if [ "$i" -eq 30 ]; then
        log "ERROR: DittoFS API did not start within 30 seconds."
        journalctl -u dfs.service --no-pager -n 50
        exit 1
    fi
    sleep 1
done

# ---------------------------------------------------------------------------
# 7. Configure stores, share via control plane API
# ---------------------------------------------------------------------------
ADMIN_PASSWORD="dittofs-bench-admin-1234567890"

log "Logging in to DittoFS API..."
dfsctl login --server http://localhost:8080 --username admin --password "${ADMIN_PASSWORD}"

log "Creating BadgerDB metadata store..."
dfsctl store metadata add --name badger-meta --type badger --db-path "${BADGER_PATH}"

log "Creating local block store (filesystem)..."
dfsctl store block local add --name fs-payload --type filesystem --path "${PAYLOAD_PATH}"

log "Creating /export share..."
dfsctl share create --name /export --metadata badger-meta --local fs-payload

# Wait for NFS adapter to be listening (auto-created on port 12049 by default).
log "Waiting for NFS on port ${NFS_PORT}..."
for i in $(seq 1 15); do
    if ss -tlnp | grep -q ":${NFS_PORT} "; then
        log "DittoFS NFS is listening on port ${NFS_PORT}."
        break
    fi
    if [ "$i" -eq 15 ]; then
        log "ERROR: DittoFS NFS did not start within 15 seconds."
        journalctl -u dfs.service --no-pager -n 50
        exit 1
    fi
    sleep 1
done

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
log "=== DittoFS (BadgerDB + FS) verification ==="
log "Service: $(systemctl is-active dfs.service)"
log "Port:    ${NFS_PORT}"
log "Config:  ${CONFIG_FILE}"
log "Meta:    badger @ ${BADGER_PATH}"
log "Payload: filesystem @ ${PAYLOAD_PATH}"
log "Export:  /export"
log "=== Setup complete ==="
