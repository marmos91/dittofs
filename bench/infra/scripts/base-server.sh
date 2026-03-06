#!/usr/bin/env bash
# base-server.sh — Base server VM setup (golden image for benchmarking)
#
# This script runs on first boot of the base server VM. It installs common
# dependencies shared by all competitors: Go toolchain, build tools, NFS
# utilities, and benchmark tooling. After this script completes, the VM is
# snapshotted to create the "dittofs-bench-base" image that each competitor
# build restores from.
#
# Usage: Runs via cloud-init or SSH provisioner during Pulumi "base" stack.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment)
# ---------------------------------------------------------------------------
GO_VERSION="${GO_VERSION:-1.25.0}"
EXPORT_DIR="${EXPORT_DIR:-/export}"
DATA_DIR="${DATA_DIR:-/data}"
BENCH_MOUNT="${BENCH_MOUNT:-/mnt/bench}"
DITTOFS_REPO="${DITTOFS_REPO:-https://github.com/marmos91/dittofs.git}"
DITTOFS_BRANCH="${DITTOFS_BRANCH:-main}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[base-server] $(date '+%H:%M:%S') $*"; }

# ---------------------------------------------------------------------------
# 1. System update and common packages
# ---------------------------------------------------------------------------
log "Updating apt package index..."
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get upgrade -y -qq

log "Installing common packages..."
apt-get install -y -qq \
    build-essential \
    git \
    curl \
    wget \
    jq \
    htop \
    iotop \
    sysstat \
    nfs-common \
    iperf3 \
    unzip \
    ca-certificates \
    gnupg \
    lsb-release \
    software-properties-common

# ---------------------------------------------------------------------------
# 2. Install Go
# ---------------------------------------------------------------------------
if command -v go &>/dev/null && go version | grep -q "go${GO_VERSION}"; then
    log "Go ${GO_VERSION} already installed, skipping."
else
    log "Installing Go ${GO_VERSION}..."
    GO_ARCHIVE="go${GO_VERSION}.linux-amd64.tar.gz"
    curl -fsSL "https://go.dev/dl/${GO_ARCHIVE}" -o "/tmp/${GO_ARCHIVE}"
    rm -rf /usr/local/go
    tar -C /usr/local -xzf "/tmp/${GO_ARCHIVE}"
    rm -f "/tmp/${GO_ARCHIVE}"
fi

# Ensure Go is on PATH for this script and future logins.
export PATH="/usr/local/go/bin:${PATH}"
if ! grep -q '/usr/local/go/bin' /etc/profile.d/go.sh 2>/dev/null; then
    cat > /etc/profile.d/go.sh <<'GOEOF'
export PATH="/usr/local/go/bin:${HOME}/go/bin:${PATH}"
GOEOF
    chmod +x /etc/profile.d/go.sh
fi

log "Go version: $(go version)"

# ---------------------------------------------------------------------------
# 3. Install fio (latest from apt)
# ---------------------------------------------------------------------------
log "Installing fio..."
apt-get install -y -qq fio

# ---------------------------------------------------------------------------
# 4. Create benchmark directories
# ---------------------------------------------------------------------------
log "Creating benchmark directories..."
mkdir -p "${EXPORT_DIR}"
mkdir -p "${DATA_DIR}"
mkdir -p "${BENCH_MOUNT}"

# Set permissions — world-writable for benchmark use.
chmod 777 "${EXPORT_DIR}"
chmod 777 "${DATA_DIR}"
chmod 777 "${BENCH_MOUNT}"

# ---------------------------------------------------------------------------
# 5. Build dfsctl from source
# ---------------------------------------------------------------------------
log "Cloning DittoFS repository for dfsctl..."
DITTOFS_SRC="/opt/dittofs"
if [ -d "${DITTOFS_SRC}" ]; then
    cd "${DITTOFS_SRC}"
    git fetch origin "${DITTOFS_BRANCH}"
    git checkout "${DITTOFS_BRANCH}"
    git reset --hard "origin/${DITTOFS_BRANCH}"
else
    git clone --branch "${DITTOFS_BRANCH}" --depth 1 "${DITTOFS_REPO}" "${DITTOFS_SRC}"
fi

cd "${DITTOFS_SRC}"
log "Building dfsctl..."
go build -o /usr/local/bin/dfsctl ./cmd/dfsctl/
chmod +x /usr/local/bin/dfsctl
log "dfsctl installed: $(dfsctl version 2>/dev/null || echo 'built')"

# ---------------------------------------------------------------------------
# 6. Kernel tuning for benchmarks
# ---------------------------------------------------------------------------
log "Applying kernel tuning for benchmarks..."
cat > /etc/sysctl.d/99-bench.conf <<'SYSCTL'
# NFS and network tuning for benchmarking
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
net.core.rmem_default = 1048576
net.core.wmem_default = 1048576
net.ipv4.tcp_rmem = 4096 1048576 16777216
net.ipv4.tcp_wmem = 4096 1048576 16777216
net.core.netdev_max_backlog = 5000
vm.dirty_ratio = 20
vm.dirty_background_ratio = 5
SYSCTL
sysctl --system -q

# ---------------------------------------------------------------------------
# 7. Cleanup
# ---------------------------------------------------------------------------
log "Cleaning apt cache..."
apt-get autoremove -y -qq
apt-get clean

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
log "=== Base server setup verification ==="
log "Go:     $(go version)"
log "fio:    $(fio --version)"
log "iperf3: $(iperf3 --version 2>&1 | head -1)"
log "dfsctl: $(which dfsctl)"
log "Dirs:   ${EXPORT_DIR} ${DATA_DIR} ${BENCH_MOUNT}"
log "=== Base server setup complete ==="
