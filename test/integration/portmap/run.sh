#!/bin/bash
# Run portmapper integration tests in a container without system rpcbind.
#
# Usage: ./run.sh [--system]
#   --system  Run system rpcbind tests (requires system rpcbind on host)
#   (default) Run container tests (no system rpcbind, uses Docker)
#
# The container tests verify that our embedded portmapper works correctly
# when there's no system rpcbind to interfere with rpcinfo queries.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../../.." && pwd)"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

log_info()  { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn()  { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

if [[ "${1:-}" == "--system" ]]; then
    log_info "Running system rpcbind integration tests..."
    cd "$REPO_ROOT"
    go test -tags=portmap_system -v -timeout 2m ./test/integration/portmap/
    exit $?
fi

# Container-based tests
if ! command -v docker &>/dev/null; then
    log_error "Docker not found. Install Docker or use --system for host tests."
    exit 1
fi

CONTAINER_NAME="dittofs-portmap-test"
IMAGE_NAME="dittofs-portmap-test"

cleanup() {
    log_info "Cleaning up..."
    docker rm -f "$CONTAINER_NAME" 2>/dev/null || true
}
trap cleanup EXIT

log_info "Building test container..."
docker build -t "$IMAGE_NAME" -f "$SCRIPT_DIR/Dockerfile" "$REPO_ROOT"

log_info "Running portmapper tests in container (no system rpcbind)..."

# Run the container test script
docker run --rm --name "$CONTAINER_NAME" "$IMAGE_NAME" '
set -e

# Verify no rpcbind is running
if pgrep rpcbind >/dev/null 2>&1; then
    echo "ERROR: rpcbind is running, this should not happen in container"
    exit 1
fi

# Verify port 111 is not in use
if nc -z 127.0.0.1 111 2>/dev/null; then
    echo "ERROR: port 111 is in use, this should not happen in container"
    exit 1
fi

echo "=== Starting DittoFS server with embedded portmapper ==="

# Create minimal config with portmapper on port 111
mkdir -p /tmp/dfs/cache /tmp/dfs/data
cat > /tmp/dfs/config.yaml << EOF
logging:
  level: INFO
  format: text

cache:
  path: /tmp/dfs/cache
  size: 100MB

database:
  type: sqlite
  sqlite:
    path: /tmp/dfs/controlplane.db
EOF

# Start server (runs in background by default, writes PID file)
export DITTOFS_ADMIN_INITIAL_PASSWORD=testpass123
export DITTOFS_CONTROLPLANE_SECRET=this-is-a-test-secret-at-least-32-chars
mkdir -p /tmp/dfs/data /tmp/dfs/state /root/.local/state/dittofs
/dfs start --config /tmp/dfs/config.yaml

# Wait for server to be ready
echo "Waiting for server to start..."
sleep 2

# Check if server is running via PID file
PID_FILE="/root/.local/state/dittofs/dittofs.pid"
if [ ! -f "$PID_FILE" ]; then
    echo "ERROR: PID file not found at $PID_FILE"
    cat /root/.local/state/dittofs/dittofs.log 2>/dev/null || true
    exit 1
fi

DFS_PID=$(cat "$PID_FILE")
if ! kill -0 "$DFS_PID" 2>/dev/null; then
    echo "ERROR: Server failed to start (PID $DFS_PID not running)"
    cat /root/.local/state/dittofs/dittofs.log 2>/dev/null || true
    exit 1
fi
echo "Server running with PID $DFS_PID"

# Wait for API to be ready
echo "Waiting for API server..."
MAX_WAIT=30
for i in $(seq 1 $MAX_WAIT); do
    if nc -z 127.0.0.1 8080 2>/dev/null; then
        echo "API ready after ${i}s"
        break
    fi
    if [ $i -eq $MAX_WAIT ]; then
        echo "ERROR: API not ready after ${MAX_WAIT}s"
        cat /root/.local/state/dittofs/dittofs.log 2>/dev/null || true
        exit 1
    fi
    sleep 1
done

# Login and enable NFS adapter
echo "Logging in as admin..."
/dfsctl login --server http://127.0.0.1:8080 --username admin --password testpass123

# Enable NFS adapter (portmapper is enabled by default)
echo "Enabling NFS adapter..."
/dfsctl adapter enable nfs --port 12049

# Wait for NFS port to be ready
echo "Waiting for NFS adapter to be ready..."
for i in $(seq 1 $MAX_WAIT); do
    if nc -z 127.0.0.1 12049 2>/dev/null; then
        echo "NFS port ready after ${i}s"
        break
    fi
    if [ $i -eq $MAX_WAIT ]; then
        echo "ERROR: NFS port not ready after ${MAX_WAIT}s"
        echo "=== Server logs ==="
        cat /root/.local/state/dittofs/dittofs.log 2>/dev/null || true
        exit 1
    fi
    sleep 1
done

# Wait a bit more for portmapper to start
sleep 1

# Get portmapper port from settings (default is 10111)
SETTINGS_JSON=$(/dfsctl adapter settings nfs show -o json 2>/dev/null)
echo "Settings JSON: $SETTINGS_JSON"
PMAP_PORT=$(echo "$SETTINGS_JSON" | grep -oE '"portmapper_port":\s*[0-9]+' | grep -oE '[0-9]+' || echo "10111")
echo "Portmapper port: $PMAP_PORT"

echo "=== Testing portmapper connectivity ==="

# Test 1: TCP connectivity to portmapper
echo "Test 1: TCP connectivity to portmapper port $PMAP_PORT"
if nc -zv 127.0.0.1 "$PMAP_PORT" 2>&1; then
    echo "PASS: Portmapper port is listening"
else
    echo "FAIL: Cannot connect to portmapper port"
    kill $DFS_PID 2>/dev/null || true
    exit 1
fi

# Test 2: TCP connectivity to NFS port
echo ""
echo "Test 2: TCP connectivity to NFS port 12049"
if nc -zv 127.0.0.1 12049 2>&1; then
    echo "PASS: NFS port is listening"
else
    echo "FAIL: Cannot connect to NFS port"
    kill $DFS_PID 2>/dev/null || true
    exit 1
fi

# Test 3: Verify server logs show portmapper started
echo ""
echo "Test 3: Verify portmapper started in logs"
if grep -q "Portmapper started" /root/.local/state/dittofs/dittofs.log 2>/dev/null; then
    echo "PASS: Portmapper started successfully"
    grep "Portmapper started" /root/.local/state/dittofs/dittofs.log
else
    echo "FAIL: Portmapper did not start"
    cat /root/.local/state/dittofs/dittofs.log 2>/dev/null || true
    kill $DFS_PID 2>/dev/null || true
    exit 1
fi

# Note: rpcinfo testing skipped in container due to Alpine rpcinfo quirks.
# Full RPC protocol testing is done by e2e tests (test/e2e/portmapper_test.go)
# which use pure Go RPC client implementation.
echo ""
echo "Note: rpcinfo probe tests skipped (Alpine rpcinfo requires port 111)."
echo "Full RPC protocol validation is done by e2e tests."

echo ""
echo "=== All portmapper tests passed ==="

# Cleanup
kill $DFS_PID 2>/dev/null || true
'

log_info "Container tests completed successfully!"
