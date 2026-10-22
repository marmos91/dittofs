#!/usr/bin/env bash
# Setup script for POSIX compliance testing
#
# This script:
# 1. Starts the DittoFS server
# 2. Waits for it to be ready
# 3. Configures stores, shares, and adapters via the API
# 4. Mounts the NFS share
#
# Usage:
#   ./setup-posix.sh [config-type] [--nfs-version 3|4|4.0|4.1] [--no-mount]
#                    [--squash MODE] [--default-permission LEVEL]
#                    [--test-user NAME:UID] [--system-rpcbind]
#
# Config types:
#   memory         - In-memory BadgerDB metadata store (default)
#   badger         - On-disk BadgerDB metadata store
#   memory-content - In-memory BadgerDB metadata + memory block store
#   cache-s3       - In-memory BadgerDB metadata + S3 block store (requires localstack)
#   badger-s3      - On-disk BadgerDB metadata + S3 block store (requires localstack)
#
# The S3 profiles expect a localstack on localhost:4566 and create their bucket
# there; setup fails if it cannot.
#
# NFS versions:
#   3   - NFSv3 (default, backward compatible)
#   4   - NFSv4.0
#   4.0 - NFSv4.0 (explicit minor version)
#   4.1 - NFSv4.1
#
# --no-mount provisions the server but skips the NFS mount, for suites that
# mount it themselves. Nothing then needs privilege, so the root check is
# skipped too.
#
# The remaining flags serve suites that run as an ordinary user on a client in
# its own network namespace (test/nfs-mount/), and change nothing for pjdfstest:
#   --squash MODE         export squash instead of root_to_admin
#   --default-permission LEVEL
#                         what a user without a grant gets on the export; a
#                         squashed root needs read to mount it over NFSv4
#   --test-user NAME:UID  a DittoFS user NAME with uid and gid UID and a
#                         read-write grant on the export
#   --system-rpcbind      register MOUNT and NLM with this host's rpcbind, so a
#                         kernel NFSv3 client on another network stack can lock
#
# Example:
#   sudo ./setup-posix.sh memory
#   sudo ./setup-posix.sh memory --nfs-version 4
#   sudo ./setup-posix.sh badger --nfs-version 4.1
#   ./setup-posix.sh memory --no-mount

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Parse arguments: first positional arg is config type, then named params
CONFIG_TYPE="memory"
NFS_VERSION="3"
NO_MOUNT=false
SQUASH="root_to_admin"
DEFAULT_PERMISSION=""
TEST_USER=""
SYSTEM_RPCBIND=false

# Parse positional and named arguments
POSITIONAL_ARGS=()
while [[ $# -gt 0 ]]; do
    case $1 in
        --nfs-version)
            NFS_VERSION="${2:-3}"
            shift 2
            ;;
        --nfs-version=*)
            NFS_VERSION="${1#*=}"
            shift
            ;;
        --no-mount)
            NO_MOUNT=true
            shift
            ;;
        --squash)
            SQUASH="${2:?--squash requires a value}"
            shift 2
            ;;
        --default-permission)
            DEFAULT_PERMISSION="${2:?--default-permission requires a value}"
            shift 2
            ;;
        --test-user)
            TEST_USER="${2:?--test-user requires NAME:UID}"
            shift 2
            ;;
        --system-rpcbind)
            SYSTEM_RPCBIND=true
            shift
            ;;
        --help|-h)
            echo "Usage: $0 [config-type] [--nfs-version 3|4|4.0|4.1] [--no-mount]"
            echo ""
            echo "Config types: memory (default), badger, memory-content, cache-s3, badger-s3"
            echo "NFS versions: 3 (default), 4, 4.0, 4.1"
            echo ""
            echo "Options:"
            echo "  --no-mount   Provision the server but do not mount (does not need root)"
            echo "  --squash MODE          Export squash (default root_to_admin)"
            echo "  --default-permission LEVEL  What users without a grant get on the export"
            echo "  --test-user NAME:UID   Also create user NAME (uid=gid=UID) with a read-write grant"
            echo "  --system-rpcbind       Register MOUNT/NLM with the host rpcbind (needs root)"
            echo ""
            echo "Examples:"
            echo "  sudo $0 memory                     # NFSv3 with memory stores"
            echo "  sudo $0 memory --nfs-version 4     # NFSv4.0 with memory stores"
            echo "  sudo $0 badger --nfs-version 4.1   # NFSv4.1 with BadgerDB stores"
            echo "  $0 memory --no-mount               # server only"
            exit 0
            ;;
        -*)
            echo "Unknown option: $1"
            echo "Usage: $0 [config-type] [--nfs-version 3|4|4.0|4.1] [--no-mount] [--squash MODE] [--default-permission LEVEL] [--test-user NAME:UID] [--system-rpcbind]"
            exit 1
            ;;
        *)
            POSITIONAL_ARGS+=("$1")
            shift
            ;;
    esac
done

# First positional arg is config type
if [[ ${#POSITIONAL_ARGS[@]} -gt 0 ]]; then
    CONFIG_TYPE="${POSITIONAL_ARGS[0]}"
fi

# Normalize NFS version: "4" -> "4.0"
case "$NFS_VERSION" in
    3) ;;
    4|4.0)
        NFS_VERSION="4.0"
        ;;
    4.1)
        NFS_VERSION="4.1"
        ;;
    *)
        echo "Error: Invalid NFS version '$NFS_VERSION'. Valid values: 3, 4, 4.0, 4.1"
        exit 1
        ;;
esac

CONFIG_FILE="$SCRIPT_DIR/configs/config.yaml"

# Set paths based on config type
DATA_DIR="/tmp/dittofs-posix-${CONFIG_TYPE}"
export DITTOFS_DATABASE_SQLITE_PATH="${DATA_DIR}/controlplane.db"
export DITTOFS_CACHE_PATH="${DATA_DIR}/cache"

MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
API_PORT=8080
NFS_PORT="${NFS_PORT:-12049}"
TEST_PASSWORD="posix-test-password-123"
S3_ENDPOINT="http://localhost:4566"
S3_BUCKET="dittofs-posix-test"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

log_info() { echo -e "${GREEN}[INFO]${NC} $*"; }
log_warn() { echo -e "${YELLOW}[WARN]${NC} $*"; }
log_error() { echo -e "${RED}[ERROR]${NC} $*"; }

if [[ -n "$TEST_USER" && ! "$TEST_USER" =~ ^[a-z_][a-z0-9_-]*:[0-9]+$ ]]; then
    echo "Error: --test-user takes NAME:UID, got '$TEST_USER'"
    exit 1
fi

# Mounting and starting rpcbind are the only steps that need privilege.
if [[ $EUID -ne 0 && ( "$NO_MOUNT" != true || "$SYSTEM_RPCBIND" == true ) ]]; then
    log_error "This script must be run as root (sudo)"
    exit 1
fi

# An unprivileged run cannot clean up after a previous sudo run: the state under
# /tmp is root-owned, and the failure would otherwise surface much later as a
# confusing permission error from rm or from a redirection.
if [[ $EUID -ne 0 ]]; then
    for path in "$DATA_DIR" /tmp/dittofs-posix-server.log /tmp/dittofs-server.pid; do
        if [[ -e "$path" && ! -w "$path" ]]; then
            log_error "$path exists and is not writable by $(id -un) — a previous run left root-owned state."
            log_error "Clear it first:  sudo $SCRIPT_DIR/teardown-posix.sh"
            exit 1
        fi
    done
fi

# Check if config file exists
if [[ ! -f "$CONFIG_FILE" ]]; then
    log_error "Config file not found: $CONFIG_FILE"
    exit 1
fi

# Binaries the suite runs against.
DITTOFS_BIN="$REPO_ROOT/dfs"
DITTOFSCTL_BIN="$REPO_ROOT/dfsctl"

# Always rebuild. Building only when the binary is absent silently serves a
# stale one to every later run, so a suite reports on code that is not the code
# under test — a fix reads as not working, and a regression reads as absent.
# Go's build cache makes the no-change case cheap enough that skipping is not
# worth the failure mode.
log_info "Building dfs..."
(cd "$REPO_ROOT" && go build -o dfs ./cmd/dfs)

log_info "Building dfsctl..."
(cd "$REPO_ROOT" && go build -o dfsctl ./cmd/dfsctl)

# Clean up any existing state
cleanup_existing() {
    log_info "Cleaning up existing state..."

    # Unmount if mounted
    if [[ "$NO_MOUNT" != true ]] && mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        log_info "Unmounting $MOUNT_POINT"
        umount -f "$MOUNT_POINT" 2>/dev/null || true
    fi

    # Stop existing server
    if pgrep -f "dfs start" >/dev/null 2>&1; then
        log_info "Stopping existing DittoFS server"
        "$DITTOFS_BIN" stop --force 2>/dev/null || pkill -f "dfs start" || true
        sleep 2
    fi

    # Clean up data directory for this config type
    rm -rf "$DATA_DIR"
    mkdir -p "$DATA_DIR"
}

# Wait for API to be ready
wait_for_api() {
    log_info "Waiting for API to be ready..."
    local max_attempts=30
    local attempt=1

    while [[ $attempt -le $max_attempts ]]; do
        if curl -s "http://localhost:$API_PORT/health" >/dev/null 2>&1; then
            log_info "API is ready"
            return 0
        fi
        sleep 1
        ((attempt++))
    done

    log_error "API failed to become ready after $max_attempts seconds"
    return 1
}

# Start DittoFS server
start_server() {
    log_info "Starting DittoFS server (config type: $CONFIG_TYPE, NFS version: $NFS_VERSION)"

    # Create data directory
    mkdir -p "$DATA_DIR"

    # Start server in foreground. The admin password is set deterministically
    # via DITTOFS_ADMIN_INITIAL_PASSWORD so the harness knows it without
    # scraping the log — the daemon no longer prints generated secrets to a
    # non-interactive (file-redirected) stdout.
    local log_file="/tmp/dittofs-posix-server.log"
    local admin_password="$TEST_PASSWORD"

    DITTOFS_ADMIN_INITIAL_PASSWORD="$admin_password" \
        "$DITTOFS_BIN" start --foreground --config "$CONFIG_FILE" > "$log_file" 2>&1 &
    local server_pid=$!

    # Wait a bit for the server to start.
    sleep 3

    echo "$admin_password" > /tmp/dittofs-admin-password
    echo "$server_pid" > /tmp/dittofs-server.pid

    wait_for_api
}

# The S3 block store never creates its bucket. Without one, every request 404s,
# the remote stays unhealthy, and an S3 profile silently exercises only the local
# tier — so a missing bucket is a setup failure, not something to run through.
# localstack accepts unsigned requests, so curl needs no AWS CLI on the host.
create_s3_bucket() {
    log_info "Creating S3 bucket ${S3_BUCKET} at ${S3_ENDPOINT}..."
    local code=""
    for _ in $(seq 1 15); do
        code=$(curl -s -o /dev/null -w '%{http_code}' -X PUT "${S3_ENDPOINT}/${S3_BUCKET}" || true)
        # 409 is BucketAlreadyOwnedByYou from a previous run.
        [[ "$code" == 200 || "$code" == 409 ]] && break
        sleep 2
    done

    if ! curl -sf -o /dev/null -I "${S3_ENDPOINT}/${S3_BUCKET}"; then
        log_error "Bucket ${S3_BUCKET} does not exist at ${S3_ENDPOINT} (last create returned HTTP ${code:-none})."
        log_error "Start localstack first, e.g.: docker run -d -p 4566:4566 -e SERVICES=s3 localstack/localstack:4.13.1"
        return 1
    fi
}

# Login and configure via API
configure_via_api() {
    log_info "Configuring DittoFS via API..."

    local admin_password
    admin_password=$(cat /tmp/dittofs-admin-password 2>/dev/null || echo "$TEST_PASSWORD")

    # Login
    log_info "Logging in as admin..."
    "$DITTOFSCTL_BIN" login --server "http://localhost:$API_PORT" --username admin --password "$admin_password" || {
        log_error "Failed to login. Admin password might be different."
        log_error "Check /tmp/dittofs-posix-server.log for the actual password"
        return 1
    }

    # Change password (required for new admin user)
    log_info "Changing admin password (first login requirement)..."
    "$DITTOFSCTL_BIN" user change-password --current "$admin_password" --new "$TEST_PASSWORD" 2>/dev/null || {
        log_info "Password already changed or change-password not required"
    }

    # Create metadata store based on config type
    log_info "Creating metadata store..."
    case "$CONFIG_TYPE" in
        memory|memory-content|cache-s3)
            "$DITTOFSCTL_BIN" store metadata add --name default --type badger --in-memory
            ;;
        badger|badger-s3)
            "$DITTOFSCTL_BIN" store metadata add --name default --type badger \
                --config "{\"db_path\":\"${DATA_DIR}/metadata\"}"
            ;;
        *)
            log_error "Unknown config type: $CONFIG_TYPE"
            exit 1
            ;;
    esac

    # Create block store based on config type
    log_info "Creating block store..."
    case "$CONFIG_TYPE" in
        cache-s3|badger-s3)
            "$DITTOFSCTL_BIN" store block add --name default --type s3 \
                --config "{\"bucket\":\"${S3_BUCKET}\",\"region\":\"us-east-1\",\"endpoint\":\"${S3_ENDPOINT}\",\"force_path_style\":true,\"access_key_id\":\"test\",\"secret_access_key\":\"test\",\"allow_private_endpoint\":true}"
            # decision: this proves the server can reach the bucket with this
            # store config, not that the suite's data is ever uploaded — a suite
            # that unlinks its files before the upload delay sends no PutObject
            # at all. It catches a missing or unreachable bucket, which otherwise
            # leaves the suite silently grading the local tier only.
            log_info "Checking that the server can reach the S3 bucket..."
            "$DITTOFSCTL_BIN" store block health --name default || {
                log_error "Block store 'default' cannot reach bucket ${S3_BUCKET} at ${S3_ENDPOINT}."
                "$DITTOFS_BIN" stop --force 2>/dev/null || pkill -f "dfs start" || true
                exit 1
            }
            ;;
        *)
            # Default: memory block store
            "$DITTOFSCTL_BIN" store block add --name default --type memory
            ;;
    esac

    # Create share
    log_info "Creating share..."
    "$DITTOFSCTL_BIN" share create --name /export --metadata default --block-store default \
        ${DEFAULT_PERMISSION:+--default-permission "$DEFAULT_PERMISSION"}

    # pjdfstest's prove(1) harness runs as root and relies on uid 0 retaining
    # POSIX superuser privileges to build/tear down the test tree (chown to
    # arbitrary uids, mknod, setuid bits) before it seteuid()s to the unprivileged
    # test UIDs. The server default is now root_to_guest (root squashed to the
    # anonymous identity), which strips that bypass and breaks the harness setup.
    # POSIX conformance inherently requires a privileged root, so this export
    # explicitly opts into no_root_squash — the legitimate root_to_admin use case,
    # not a workaround for the new default. A suite that runs its tests as an
    # ordinary user passes --squash to keep root's bypass out of them.
    log_info "Setting export squash to $SQUASH..."
    "$DITTOFSCTL_BIN" share nfs-config set /export --squash "$SQUASH"

    # Create and gate-grant the principals pjdfstest operates as.
    #
    # pjdfstest runs prove(1) as root (uid 0 superuser-bypasses POSIX, so it sets
    # up the working tree) and then seteuid()s to a fixed set of unprivileged
    # test UIDs to verify POSIX permission semantics. With the secure share
    # default (default-permission=none) those UIDs are unknown to the export and
    # are denied at the gate before POSIX is ever consulted, so each must be a
    # known user with a read-write grant. The grant opens the export gate and
    # projects a NON-inheriting ACL onto the share ROOT directory only (so a
    # grantee can write the root); it does not propagate onto the per-test
    # working dirs and files, so the per-file POSIX mode bits — exactly what
    # pjdfstest asserts — still govern. UIDs mirror test/posix/Dockerfile.pjdfstest.
    # No --owner is needed: root owns the export root and creates the per-test
    # working dirs, which pjdfstest then chmod/chowns for the unprivileged UIDs.
    log_info "Creating pjdfstest test users..."
    for uid in 65532 65533 65534; do
        "$DITTOFSCTL_BIN" user create --username "pjdfstest-${uid}" --password pjdfstest --uid "$uid" --gid "$uid"
        "$DITTOFSCTL_BIN" share permission grant /export --user "pjdfstest-${uid}" --level read-write
    done

    # The same grant for a suite's own test user, whose uid and gid must match
    # the local account the suite runs as.
    if [[ -n "$TEST_USER" ]]; then
        local name="${TEST_USER%%:*}" uid="${TEST_USER##*:}"
        log_info "Creating test user $name (uid $uid)..."
        "$DITTOFSCTL_BIN" user create --username "$name" --password "$name" --uid "$uid" --gid "$uid"
        "$DITTOFSCTL_BIN" share permission grant /export --user "$name" --level read-write
    fi

    # Both settings are read when the adapter starts, so they go in before it
    # is enabled. A kernel NFSv3 client asks the server's rpcbind where the lock
    # manager is; without the registration it reaches no lock manager at all.
    # The registration replaces any lock manager already registered with this
    # host's rpcbind, and is withdrawn when dfs stops, so a host running its
    # own NFS server or locking NFS client loses its lock manager to it. An
    # rpcbind started here is left running.
    if [[ "$SYSTEM_RPCBIND" == true ]]; then
        if ! rpcinfo -p 127.0.0.1 >/dev/null 2>&1; then
            log_info "Starting rpcbind..."
            rpcbind || { log_error "rpcbind failed to start"; return 1; }
        elif rpcinfo -p 127.0.0.1 2>/dev/null | grep -qE '^ *100021 '; then
            log_warn "a lock manager is already registered with rpcbind; dfs will replace it until it stops"
        fi
        "$DITTOFSCTL_BIN" adapter settings nfs update --portmapper-register-with-system --udp-enabled
    fi

    # Enable NFS adapter
    log_info "Enabling NFS adapter..."
    "$DITTOFSCTL_BIN" adapter enable nfs --port $NFS_PORT

    # Wait for NFS adapter to start and register shares
    log_info "Waiting for NFS adapter to be ready..."
    sleep 3

    # For NFSv4 POSIX testing: disable delegations.
    # With WRITE delegations enabled, the Linux NFS client services writes
    # locally without sending WRITE/SETATTR to the server. This prevents
    # server-side SUID/SGID clearing (chmod/12.t) and other POSIX semantics
    # that require the server to process every operation.
    if [ "$NFS_VERSION" = "4.0" ] || [ "$NFS_VERSION" = "4.1" ]; then
        log_info "Disabling NFSv4 delegations for POSIX compliance testing..."
        "$DITTOFSCTL_BIN" adapter settings nfs update --delegations-enabled=false --force || {
            log_warn "Failed to disable delegations (non-fatal)"
        }
        # Wait for SettingsWatcher to pick up the change (polls every 10s)
        log_info "Waiting for settings to propagate..."
        sleep 12
    fi

    # Verify NFS port is listening
    log_info "Checking NFS port..."
    if ! nc -zv localhost $NFS_PORT 2>&1; then
        log_error "NFS adapter failed to start on port $NFS_PORT"
        tail -50 /tmp/dittofs-posix-server.log
        return 1
    fi
    log_info "NFS port $NFS_PORT is listening"

    if [[ "$SYSTEM_RPCBIND" == true ]]; then
        local _
        for _ in $(seq 1 30); do
            rpcinfo -p 127.0.0.1 2>/dev/null | grep -qE '^ *100021 ' && break
            sleep 1
        done
        if ! rpcinfo -p 127.0.0.1 2>/dev/null | grep -qE '^ *100021 '; then
            log_error "the lock manager (100021) never registered with rpcbind"
            rpcinfo -p 127.0.0.1 2>&1 || true
            return 1
        fi
        log_info "Lock manager registered with rpcbind"
    fi

    log_info "API configuration complete"
}

# Mount NFS share
mount_nfs() {
    log_info "Mounting NFS share (version: NFSv${NFS_VERSION})..."

    mkdir -p "$MOUNT_POINT"

    local mount_opts=""
    case "$NFS_VERSION" in
        3)
            # NFSv3 mount options:
            # noac disables attribute caching to ensure fresh attributes for tests
            # that delete and recreate files with the same name
            # sync forces synchronous operations to prevent SETATTR coalescing issues
            # lookupcache=none disables name lookup caching
            mount_opts="nfsvers=3,tcp,port=$NFS_PORT,mountport=$NFS_PORT,nolock,noac,sync,lookupcache=none"
            ;;
        4.0)
            # NFSv4.0 mount options:
            # No mountport (NFSv4 does not use separate mount protocol)
            # No nolock (NFSv4 has integrated locking, not NLM-based)
            # noac and sync for test consistency
            # lookupcache=none disables name lookup caching
            mount_opts="vers=4.0,port=$NFS_PORT,noac,sync,lookupcache=none"
            ;;
        4.1)
            # NFSv4.1 mount options:
            # Same as v4.0 but with vers=4.1.
            mount_opts="vers=4.1,port=$NFS_PORT,noac,sync,lookupcache=none"
            ;;
    esac

    log_info "Mount command: mount -t nfs -o $mount_opts localhost:/export $MOUNT_POINT"

    # Use timeout to prevent infinite hangs during mount negotiation
    if ! timeout 60 mount -t nfs -o "$mount_opts" localhost:/export "$MOUNT_POINT"; then
        log_error "Mount failed or timed out after 60 seconds"
        log_error "Checking server state..."
        log_info "=== Server log (last 30 lines) ==="
        tail -30 /tmp/dittofs-posix-server.log 2>/dev/null || true
        log_info "=== dmesg NFS errors ==="
        dmesg | grep -i nfs | tail -20 2>/dev/null || true
        log_info "=== rpcinfo ==="
        rpcinfo -p localhost 2>/dev/null || true
        log_info "=== NFS kernel modules ==="
        lsmod | grep nfs 2>/dev/null || true
        return 1
    fi

    log_info "NFS share mounted at $MOUNT_POINT (NFSv${NFS_VERSION})"
}

# Main
main() {
    log_info "Setting up POSIX tests with config type: $CONFIG_TYPE, NFS version: $NFS_VERSION"

    # Before the server starts, so a missing localstack leaves no server behind.
    case "$CONFIG_TYPE" in
        cache-s3|badger-s3) create_s3_bucket ;;
    esac

    cleanup_existing
    start_server
    configure_via_api

    if [[ "$NO_MOUNT" != true ]]; then
        mount_nfs
    fi

    echo ""
    log_info "Setup complete!"
    log_info ""
    if [[ "$NO_MOUNT" == true ]]; then
        log_info "Server:      localhost:${NFS_PORT}, share /export (not mounted)"
    else
        log_info "Mount point: $MOUNT_POINT"
    fi
    log_info "NFS version: NFSv${NFS_VERSION}"
    log_info "Server log:  /tmp/dittofs-posix-server.log"
    log_info "Data dir:    $DATA_DIR"
    log_info ""
    log_info "To clean up:"
    log_info "  sudo $SCRIPT_DIR/teardown-posix.sh"
}

main "$@"
