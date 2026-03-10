#!/usr/bin/env bash
# setup-dittofs-demo.sh - Provision a Scaleway VM for DittoFS demo
#
# Prerequisites:
#   - Fresh Ubuntu 24.04 VM (Scaleway DEV1-S/M)
#   - DittoFS source tarball on the VM (default: ~/dittofs.tar.gz)
#   - Local SSD attached at /dev/sda1
#
# Usage:
#   sudo ./setup-dittofs-demo.sh [tarball-path]

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
TARBALL="${1:-$HOME/dittofs.tar.gz}"
SSD_DEVICE="/dev/sda1"
SSD_MOUNT="/mnt/ssd"
CACHE_DIR="${SSD_MOUNT}/dittofs-cache"
METADATA_DIR="${SSD_MOUNT}/dittofs-metadata"
CONFIG_DIR="/etc/dittofs"
CONFIG_FILE="${CONFIG_DIR}/config.yaml"
GO_VERSION="1.25.0"
ADMIN_PASSWORD="$(openssl rand -base64 16)"

# Cubbit DS3 S3 credentials
S3_ENDPOINT="https://s3.cubbit.eu"
S3_BUCKET="dittofs-demo"
S3_ACCESS_KEY="dxp2eUyJ+fK307J+aqreAoy63rh4w4+B"
S3_SECRET_KEY="6XxMNrcAXk/vvMzM11PTsZqfZ5jG3e8gkUVBnhi6g/k="

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
info()  { echo -e "\033[1;34m[INFO]\033[0m  $*"; }
warn()  { echo -e "\033[1;33m[WARN]\033[0m  $*"; }
error() { echo -e "\033[1;31m[ERROR]\033[0m $*"; }
die()   { error "$@"; exit 1; }

check_root() {
    if [ "$(id -u)" -ne 0 ]; then
        die "This script must be run as root (use sudo)"
    fi
}

# ---------------------------------------------------------------------------
# 1. System preparation
# ---------------------------------------------------------------------------
install_system_deps() {
    info "Installing system dependencies..."
    apt-get update -qq
    apt-get install -y -qq git build-essential curl nfs-common > /dev/null
    info "System dependencies installed"
}

install_go() {
    if command -v go &>/dev/null; then
        local current
        current="$(go version | awk '{print $3}' | sed 's/go//')"
        if [ "$current" = "$GO_VERSION" ]; then
            info "Go ${GO_VERSION} already installed"
            return
        fi
    fi

    info "Installing Go ${GO_VERSION}..."
    local tarball="go${GO_VERSION}.linux-amd64.tar.gz"
    curl -sSfL "https://go.dev/dl/${tarball}" -o "/tmp/${tarball}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${tarball}"
    rm -f "/tmp/${tarball}"

    # Ensure go is in PATH for this script and future sessions
    export PATH="/usr/local/go/bin:${PATH}"
    if ! grep -q '/usr/local/go/bin' /etc/profile.d/go.sh 2>/dev/null; then
        echo 'export PATH="/usr/local/go/bin:${PATH}"' > /etc/profile.d/go.sh
    fi

    info "Go $(go version | awk '{print $3}') installed"
}

disable_conflicting_services() {
    info "Stopping services that may conflict on ports 111, 445, 2049..."

    # Port 111: rpcbind / portmapper
    for svc in rpcbind.socket rpcbind.service; do
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
        systemctl mask "$svc" 2>/dev/null || true
    done

    # Port 2049: nfs-server / nfs-kernel-server
    for svc in nfs-server nfs-kernel-server nfs-mountd nfs-idmapd; do
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
    done

    # Port 445: smbd (Samba)
    for svc in smbd smb nmbd; do
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
    done

    # Port 80: web servers
    for svc in apache2 nginx; do
        systemctl stop "$svc" 2>/dev/null || true
        systemctl disable "$svc" 2>/dev/null || true
    done

    info "Conflicting services disabled"
}

# ---------------------------------------------------------------------------
# 2. Mount SSD
# ---------------------------------------------------------------------------
setup_ssd() {
    info "Setting up SSD at ${SSD_DEVICE}..."

    # Format if not already ext4
    local fstype
    fstype="$(blkid -o value -s TYPE "${SSD_DEVICE}" 2>/dev/null || echo "")"
    if [ "$fstype" != "ext4" ]; then
        info "Formatting ${SSD_DEVICE} as ext4..."
        mkfs.ext4 -F "${SSD_DEVICE}"
    else
        info "${SSD_DEVICE} already formatted as ext4"
    fi

    # Create mount point and mount
    mkdir -p "${SSD_MOUNT}"
    if ! mountpoint -q "${SSD_MOUNT}"; then
        mount "${SSD_DEVICE}" "${SSD_MOUNT}"
    fi

    # Add to fstab if not already present
    if ! grep -q "${SSD_DEVICE}" /etc/fstab; then
        echo "${SSD_DEVICE}  ${SSD_MOUNT}  ext4  defaults,nofail  0  2" >> /etc/fstab
        info "Added ${SSD_DEVICE} to /etc/fstab"
    fi

    # Create data directories
    mkdir -p "${CACHE_DIR}" "${METADATA_DIR}"
    info "SSD mounted at ${SSD_MOUNT}"
}

# ---------------------------------------------------------------------------
# 3. Build DittoFS from tarball
# ---------------------------------------------------------------------------
build_dittofs() {
    if [ ! -f "${TARBALL}" ]; then
        die "Tarball not found: ${TARBALL}"
    fi

    info "Extracting tarball..."
    local build_dir="/tmp/dittofs-build"
    rm -rf "${build_dir}"
    mkdir -p "${build_dir}"
    tar -xzf "${TARBALL}" -C "${build_dir}"

    # Find the source root (tarball may have a top-level directory or not)
    local src_dir
    src_dir="$(find "${build_dir}" -name "go.mod" -path "*/dittofs*" -exec dirname {} \; | head -1)"
    if [ -z "$src_dir" ]; then
        # Fallback: go.mod directly in extracted dir
        src_dir="$(find "${build_dir}" -maxdepth 2 -name "go.mod" -exec dirname {} \; | head -1)"
    fi
    if [ -z "$src_dir" ]; then
        die "Cannot find Go source in tarball"
    fi

    info "Building DittoFS from ${src_dir}..."
    cd "${src_dir}"

    export PATH="/usr/local/go/bin:${PATH}"
    go build -o /usr/local/bin/dfs ./cmd/dfs/
    go build -o /usr/local/bin/dfsctl ./cmd/dfsctl/

    info "Binaries installed: /usr/local/bin/dfs, /usr/local/bin/dfsctl"

    # Cleanup
    rm -rf "${build_dir}"
}

# ---------------------------------------------------------------------------
# 4. Generate config
# ---------------------------------------------------------------------------
generate_config() {
    info "Generating config at ${CONFIG_FILE}..."
    local jwt_secret
    jwt_secret="$(openssl rand -hex 32)"

    mkdir -p "${CONFIG_DIR}"

    cat > "${CONFIG_FILE}" <<YAML
# DittoFS Demo Configuration
logging:
  level: "INFO"
  format: "text"
  output: "stdout"

shutdown_timeout: 30s

database:
  type: sqlite
  sqlite:
    path: ""

controlplane:
  port: 80
  read_timeout: 10s
  write_timeout: 10s
  idle_timeout: 60s
  jwt:
    secret: "${jwt_secret}"
    access_token_duration: 15m
    refresh_token_duration: 168h

cache:
  path: "${CACHE_DIR}"
  size: 50Gi

admin:
  username: "admin"
  email: ""
  password_hash: ""
YAML

    chmod 600 "${CONFIG_FILE}"
    info "Config generated (JWT secret auto-generated)"
}

# ---------------------------------------------------------------------------
# 5. Systemd service
# ---------------------------------------------------------------------------
install_systemd_service() {
    info "Installing systemd service..."

    cat > /etc/systemd/system/dittofs.service <<EOF
[Unit]
Description=DittoFS Virtual Filesystem Server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
Environment=DITTOFS_ADMIN_INITIAL_PASSWORD=${ADMIN_PASSWORD}
ExecStart=/usr/local/bin/dfs start --foreground --config ${CONFIG_FILE}
Restart=on-failure
RestartSec=5
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF

    systemctl daemon-reload
    info "Systemd service installed"
}

# ---------------------------------------------------------------------------
# 6. Start server and configure
# ---------------------------------------------------------------------------
wait_for_api() {
    local max_attempts=60
    local attempt=1

    info "Waiting for DittoFS API..."
    while [ "$attempt" -le "$max_attempts" ]; do
        if curl -sf http://localhost:80/health/ready >/dev/null 2>&1; then
            info "DittoFS API is ready"
            return 0
        fi
        sleep 1
        attempt=$((attempt + 1))
    done

    die "DittoFS API not ready after ${max_attempts}s"
}

configure_server() {
    info "Logging in as admin..."
    dfsctl login --server http://localhost:80 --username admin --password "${ADMIN_PASSWORD}"

    info "Creating BadgerDB metadata store..."
    dfsctl store metadata add --name demo-meta --type badger --db-path "${METADATA_DIR}"

    info "Creating local block store..."
    dfsctl store block local add --name demo-local --type memory

    info "Creating S3 remote block store (Cubbit DS3)..."
    dfsctl store block remote add --name demo-remote --type s3 \
        --bucket "${S3_BUCKET}" \
        --endpoint "${S3_ENDPOINT}" \
        --access-key "${S3_ACCESS_KEY}" \
        --secret-key "${S3_SECRET_KEY}"

    info "Creating /demo share..."
    dfsctl share create --name /demo --metadata demo-meta --local demo-local --remote demo-remote

    info "Enabling NFS adapter on port 2049..."
    dfsctl adapter enable nfs --port 2049

    info "Enabling SMB adapter on port 445..."
    dfsctl adapter enable smb --port 445

    info "Enabling portmapper on port 111..."
    dfsctl adapter settings nfs update --portmapper-enabled --portmapper-port 111
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo "============================================="
    echo "  DittoFS Demo - Scaleway VM Setup"
    echo "============================================="
    echo ""

    check_root

    install_system_deps
    install_go
    disable_conflicting_services
    setup_ssd
    build_dittofs
    generate_config
    install_systemd_service

    # Clean any stale DB from a previous run (systemd may use /.config as XDG default)
    rm -f /.config/dittofs/controlplane.db /root/.config/dittofs/controlplane.db

    info "Starting DittoFS..."
    systemctl enable --now dittofs

    wait_for_api

    configure_server

    # Restart so portmapper settings take effect on the NFS adapter
    info "Restarting DittoFS to apply adapter settings..."
    systemctl restart dittofs

    wait_for_api

    echo ""
    echo "============================================="
    echo "  DittoFS Demo Setup Complete"
    echo "============================================="
    echo ""
    echo "  Admin password: ${ADMIN_PASSWORD}"
    echo ""
    echo "  Services:"
    echo "    NFS  -> port 2049"
    echo "    SMB  -> port 445"
    echo "    API  -> port 80"
    echo "    Portmapper -> port 111"
    echo ""
    echo "  Mount (NFS):"
    echo "    mount -t nfs -o tcp,vers=3 <VM_IP>:/demo /mnt/demo"
    echo ""
    echo "  Mount (SMB):"
    echo "    mount -t cifs -o user=admin,pass=${ADMIN_PASSWORD} //<VM_IP>/demo /mnt/demo"
    echo ""
    echo "  Verify:"
    echo "    systemctl status dittofs"
    echo "    dfsctl adapter list"
    echo "    dfsctl share list"
    echo "    showmount -e localhost"
    echo ""
    echo "  Security group ports: 22, 80, 111, 445, 2049"
    echo ""
    echo "============================================="
}

main
