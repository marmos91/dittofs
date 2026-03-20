#!/usr/bin/env bash
set -euo pipefail

# =============================================================================
# DittoFS Edge Test Suite
#
# Validates cache persistence, offline reads/writes during S3 disconnection,
# and auto-sync on reconnect against real Scaleway infrastructure.
#
# Usage: ./edge-test.sh [flags] <command>
# Commands: persist, offline, sync, all
# =============================================================================

# ---------------------------------------------------------------------------
# Configuration (environment variables or flags)
# ---------------------------------------------------------------------------
SERVER_IP="${SERVER_IP:-}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_rsa}"
SSH_USER="${SSH_USER:-root}"
MOUNT_POINT="${MOUNT_POINT:-/mnt/dittofs-edge}"
NFS_PORT="${NFS_PORT:-12049}"
API_PORT="${API_PORT:-8080}"
ADMIN_PASSWORD="${ADMIN_PASSWORD:-dittofs-edge-admin-1234567890}"
PERSIST_DELAY="${PERSIST_DELAY:-300}"
OFFLINE_DURATION="${OFFLINE_DURATION:-120}"
SYNC_TIMEOUT="${SYNC_TIMEOUT:-300}"
SMALL_COUNT="${SMALL_COUNT:-10}"
MEDIUM_COUNT="${MEDIUM_COUNT:-5}"
LARGE_COUNT="${LARGE_COUNT:-2}"

COMMAND=""

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------
usage() {
    cat <<EOF
Usage: $(basename "$0") [flags] <command>

Commands:
  persist   Upload files, wait, verify reads persist (tests cache retention)
  offline   Block S3, verify cached reads/offline writes (tests offline mode)
  sync      Write while offline, restore S3, verify auto-sync (tests syncer recovery)
  all       Run all scenarios sequentially

Flags:
  --server IP              Server VM IP address (or set SERVER_IP env var)
  --ssh-key PATH           SSH private key (default: ~/.ssh/id_rsa)
  --delay SECONDS          Persistence test delay (default: 300)
  --offline-duration SECS  How long to keep S3 blocked (default: 120)
  --sync-timeout SECS      Max wait for sync completion (default: 300)
  -h, --help               Show this help

Environment:
  SERVER_IP                Server VM IP address
  SSH_KEY                  SSH private key path
  SSH_USER                 SSH user (default: root)
  ADMIN_PASSWORD           DittoFS admin password
  SMALL_COUNT              Number of 4KB test files (default: 10)
  MEDIUM_COUNT             Number of 1MB test files (default: 5)
  LARGE_COUNT              Number of 64MB test files (default: 2)

Examples:
  # Quick persistence test with short delay
  SERVER_IP=1.2.3.4 ./edge-test.sh --delay 60 persist

  # Full test suite
  ./edge-test.sh --server 1.2.3.4 all
EOF
}

# ---------------------------------------------------------------------------
# Parse command-line flags
# ---------------------------------------------------------------------------
while [[ $# -gt 0 ]]; do
    case "$1" in
        --server)     SERVER_IP="$2"; shift 2 ;;
        --ssh-key)    SSH_KEY="$2"; shift 2 ;;
        --delay)      PERSIST_DELAY="$2"; shift 2 ;;
        --offline-duration) OFFLINE_DURATION="$2"; shift 2 ;;
        --sync-timeout)     SYNC_TIMEOUT="$2"; shift 2 ;;
        --help|-h)    usage; exit 0 ;;
        -*)           echo "Unknown flag: $1"; usage; exit 1 ;;
        *)            COMMAND="$1"; shift ;;
    esac
done

: "${SERVER_IP:?SERVER_IP is required (set via --server or SERVER_IP env var)}"
: "${COMMAND:?Subcommand required: persist, offline, sync, all}"

# ---------------------------------------------------------------------------
# Helper functions
# ---------------------------------------------------------------------------
log() {
    echo "$(date '+%Y-%m-%d %H:%M:%S') $*"
}

PASS_COUNT=0
FAIL_COUNT=0

assert() {
    local description="$1"
    shift
    if "$@"; then
        log "[PASS] ${description}"
        PASS_COUNT=$((PASS_COUNT + 1))
    else
        log "[FAIL] ${description}"
        FAIL_COUNT=$((FAIL_COUNT + 1))
    fi
}

summary() {
    echo ""
    log "========================================="
    log "RESULTS: ${PASS_COUNT} passed, ${FAIL_COUNT} failed"
    log "========================================="
    [ "${FAIL_COUNT}" -eq 0 ]
}

ssh_server() {
    ssh -o StrictHostKeyChecking=no -o ConnectTimeout=10 -i "${SSH_KEY}" "${SSH_USER}@${SERVER_IP}" "$@"
}

# ---------------------------------------------------------------------------
# File generation and verification
# ---------------------------------------------------------------------------
generate_file() {
    local path="$1"
    local size="$2"
    local seed
    seed=$(echo -n "$(basename "$path")" | sha256sum | awk '{print $1}')
    dd if=/dev/zero bs="$size" count=1 2>/dev/null | \
        openssl enc -aes-256-ctr -nosalt -K "${seed}" -iv "00000000000000000000000000000000" \
        > "${path}" 2>/dev/null
}

checksum_file() {
    sha256sum "$1" | awk '{print $1}'
}

generate_test_files() {
    local prefix="$1"
    local dir="${MOUNT_POINT}/${prefix}"
    mkdir -p "${dir}"

    log "Generating test files in ${dir}..."

    # Small files (4KB)
    for i in $(seq 1 "${SMALL_COUNT}"); do
        generate_file "${dir}/small-${i}.dat" 4096
    done
    log "  Created ${SMALL_COUNT} small files (4KB each)"

    # Medium files (1MB)
    for i in $(seq 1 "${MEDIUM_COUNT}"); do
        generate_file "${dir}/medium-${i}.dat" 1048576
    done
    log "  Created ${MEDIUM_COUNT} medium files (1MB each)"

    # Large files (64MB)
    for i in $(seq 1 "${LARGE_COUNT}"); do
        generate_file "${dir}/large-${i}.dat" 67108864
    done
    log "  Created ${LARGE_COUNT} large files (64MB each)"
}

collect_checksums() {
    local dir="${MOUNT_POINT}/$1"
    find "${dir}" -type f -name "*.dat" | sort | while read -r f; do
        echo "$(checksum_file "$f") $(basename "$f")"
    done
}

verify_checksums() {
    local dir="${MOUNT_POINT}/$1"
    local saved_sums="$2"
    local all_pass=true
    while IFS=' ' read -r expected_sum fname; do
        local actual_sum
        actual_sum=$(checksum_file "${dir}/${fname}")
        if [ "$actual_sum" = "$expected_sum" ]; then
            log "  [OK] ${fname}"
        else
            log "  [MISMATCH] ${fname}: expected=${expected_sum}, got=${actual_sum}"
            all_pass=false
        fi
    done < "${saved_sums}"
    $all_pass
}

# ---------------------------------------------------------------------------
# Health endpoint functions
# ---------------------------------------------------------------------------
check_health_status() {
    ssh_server "curl -sf http://localhost:${API_PORT}/health" | jq -r '.status'
}

get_pending_uploads() {
    ssh_server "curl -sf http://localhost:${API_PORT}/health" | jq -r '.data.storage_health.total_pending'
}

wait_for_degraded() {
    local timeout="${1:-90}"
    log "Waiting for health endpoint to show 'degraded'..."
    local elapsed=0
    while [ "$elapsed" -lt "$timeout" ]; do
        local status
        status=$(check_health_status 2>/dev/null || echo "unknown")
        if [ "$status" = "degraded" ]; then
            return 0
        fi
        sleep 5
        elapsed=$((elapsed + 5))
    done
    return 1
}

wait_for_sync() {
    local timeout="${1:-${SYNC_TIMEOUT}}"
    local start
    start=$(date +%s)
    while true; do
        local status
        status=$(check_health_status 2>/dev/null || echo "unknown")
        local pending
        pending=$(get_pending_uploads 2>/dev/null || echo "-1")
        log "  Health: status=${status}, pending=${pending}"
        if [ "$status" = "healthy" ] && [ "$pending" = "0" ]; then
            return 0
        fi
        local elapsed=$(( $(date +%s) - start ))
        if [ "$elapsed" -ge "$timeout" ]; then
            log "  Timeout after ${timeout}s waiting for sync"
            return 1
        fi
        sleep 5
    done
}

# ---------------------------------------------------------------------------
# NFS mount management
# ---------------------------------------------------------------------------
setup_nfs_mount() {
    log "Mounting NFS export from ${SERVER_IP}..."
    sudo mkdir -p "${MOUNT_POINT}"
    sudo mount -t nfs -o "tcp,port=${NFS_PORT},mountport=${NFS_PORT},noac" \
        "${SERVER_IP}:/export" "${MOUNT_POINT}"
    log "NFS mounted at ${MOUNT_POINT}"
}

teardown_nfs_mount() {
    log "Unmounting NFS..."
    sudo umount "${MOUNT_POINT}" 2>/dev/null || sudo umount -f "${MOUNT_POINT}" 2>/dev/null || true
}

# ---------------------------------------------------------------------------
# Server management
# ---------------------------------------------------------------------------
clean_server_data() {
    log "Cleaning server test data..."
    ssh_server "rm -rf /export/edge-test-* 2>/dev/null || true"
}

dfsctl_server() {
    ssh_server "dfsctl $*"
}

login_server() {
    ssh_server "dfsctl login --server http://localhost:${API_PORT} --username admin --password '${ADMIN_PASSWORD}'"
}

# ---------------------------------------------------------------------------
# iptables functions (S3 blocking)
# ---------------------------------------------------------------------------
resolve_s3_ips() {
    ssh_server "dig +short s3.fr-par.scw.cloud | grep -E '^[0-9]'"
}

block_s3() {
    local s3_ips="$1"
    log "Blocking S3 traffic on server..."
    ssh_server "iptables -N DITTOFS_EDGE_TEST 2>/dev/null || true"
    if ! ssh_server "iptables -I OUTPUT -j DITTOFS_EDGE_TEST"; then
        log "ERROR: Failed to insert DITTOFS_EDGE_TEST chain into OUTPUT"
        return 1
    fi
    for ip in ${s3_ips}; do
        ssh_server "iptables -A DITTOFS_EDGE_TEST -d ${ip} -j DROP"
        log "  Blocked ${ip}"
    done
}

restore_s3() {
    log "Restoring S3 connectivity on server..."
    ssh_server "iptables -D OUTPUT -j DITTOFS_EDGE_TEST 2>/dev/null || true"
    ssh_server "iptables -F DITTOFS_EDGE_TEST 2>/dev/null || true"
    ssh_server "iptables -X DITTOFS_EDGE_TEST 2>/dev/null || true"
    log "S3 connectivity restored"
}

verify_s3_blocked() {
    log "Verifying S3 is unreachable from server..."
    if ssh_server "curl -sf --connect-timeout 5 https://s3.fr-par.scw.cloud/ 2>/dev/null"; then
        log "ERROR: S3 is still reachable!"
        return 1
    fi
    log "  S3 confirmed unreachable"
    return 0
}

# =============================================================================
# Scenario: persist
# =============================================================================
run_persist() {
    log "=== SCENARIO: Cache Persistence ==="
    log "Testing that files remain readable after ${PERSIST_DELAY}s delay"

    # Setup
    setup_nfs_mount
    trap 'teardown_nfs_mount' EXIT
    clean_server_data
    login_server

    # Set retention to pin (per user decision: exercise Phase 63 features)
    log "Setting retention to 'pin'..."
    dfsctl_server "share edit /export --retention pin"

    # Generate and checksum
    generate_test_files "edge-test-persist"
    local checksum_tmp
    checksum_tmp=$(mktemp)
    collect_checksums "edge-test-persist" > "${checksum_tmp}"
    local file_count
    file_count=$(wc -l < "${checksum_tmp}")
    log "Generated ${file_count} files, checksums saved"

    # Wait for initial sync to complete
    log "Waiting for initial sync to S3..."
    wait_for_sync "${SYNC_TIMEOUT}"
    assert "Initial sync completed" [ "$(get_pending_uploads)" = "0" ]

    # Wait the configured delay
    log "Waiting ${PERSIST_DELAY}s for cache persistence test..."
    sleep "${PERSIST_DELAY}"

    # Drop NFS client caches to force server-side reads
    log "Dropping NFS client caches..."
    sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches' 2>/dev/null || true

    # Verify all files still readable with correct checksums
    log "Verifying files after delay..."
    assert "All files readable with correct checksums after ${PERSIST_DELAY}s delay" \
        verify_checksums "edge-test-persist" "${checksum_tmp}"

    # Test with TTL retention mode
    log "Switching retention to 'ttl' (1h)..."
    dfsctl_server "share edit /export --retention ttl --retention-ttl 1h"

    # Verify reads still work under TTL
    assert "Files readable under TTL retention" \
        verify_checksums "edge-test-persist" "${checksum_tmp}"

    # Switch back to LRU
    log "Switching retention to 'lru'..."
    dfsctl_server "share edit /export --retention lru"

    assert "Files readable under LRU retention" \
        verify_checksums "edge-test-persist" "${checksum_tmp}"

    # Cleanup
    rm -f "${checksum_tmp}"
    teardown_nfs_mount
    trap - EXIT

    log "=== PERSIST scenario complete ==="
}

# =============================================================================
# Scenario: offline
# =============================================================================
run_offline() {
    log "=== SCENARIO: Offline Operation ==="
    log "Testing reads/writes while S3 is blocked for ${OFFLINE_DURATION}s"

    # Setup
    setup_nfs_mount
    trap 'restore_s3; teardown_nfs_mount' EXIT
    clean_server_data
    login_server

    # Ensure pin retention for offline test
    dfsctl_server "share edit /export --retention pin"

    # Pre-populate files while online
    generate_test_files "edge-test-offline"
    local checksum_tmp
    checksum_tmp=$(mktemp)
    collect_checksums "edge-test-offline" > "${checksum_tmp}"
    log "Pre-populated files, waiting for sync..."
    wait_for_sync "${SYNC_TIMEOUT}"
    assert "Pre-sync completed before offline test" [ "$(get_pending_uploads)" = "0" ]

    # Resolve S3 IPs before blocking
    local s3_ips
    s3_ips=$(resolve_s3_ips)
    log "Resolved S3 IPs: ${s3_ips}"

    # Block S3
    block_s3 "${s3_ips}"
    assert "S3 connectivity blocked" verify_s3_blocked

    # Wait for health to show degraded (health check interval ~30s)
    wait_for_degraded
    assert "Health endpoint shows 'degraded'" [ "$(check_health_status)" = "degraded" ]

    # Drop NFS client caches
    sudo sh -c 'echo 3 > /proc/sys/vm/drop_caches' 2>/dev/null || true

    # Test 1: Read cached files offline
    log "Testing offline reads of cached files..."
    assert "Cached files readable while S3 is blocked" \
        verify_checksums "edge-test-offline" "${checksum_tmp}"

    # Test 2: Write new files offline
    log "Testing offline writes..."
    local offline_dir="${MOUNT_POINT}/edge-test-offline-writes"
    mkdir -p "${offline_dir}"
    generate_file "${offline_dir}/offline-write-1.dat" 4096
    generate_file "${offline_dir}/offline-write-2.dat" 1048576
    assert "Can write small file while offline" [ -f "${offline_dir}/offline-write-1.dat" ]
    assert "Can write medium file while offline" [ -f "${offline_dir}/offline-write-2.dat" ]

    # Save checksums of offline-written files
    local offline_checksums
    offline_checksums=$(mktemp)
    collect_checksums "edge-test-offline-writes" > "${offline_checksums}"

    # Test 3: Directory listing works offline
    local ls_count
    ls_count=$(ls "${MOUNT_POINT}/edge-test-offline/" | wc -l)
    assert "Directory listing works while offline (${ls_count} entries)" [ "${ls_count}" -gt 0 ]

    # Wait the offline duration
    log "S3 blocked for ${OFFLINE_DURATION}s..."
    sleep "${OFFLINE_DURATION}"

    # Verify files still readable after extended offline period
    assert "Files still readable after ${OFFLINE_DURATION}s offline" \
        verify_checksums "edge-test-offline" "${checksum_tmp}"

    # Restore S3
    restore_s3

    # Verify offline-written files survived reconnect
    assert "Offline-written files readable after S3 restore" \
        verify_checksums "edge-test-offline-writes" "${offline_checksums}"

    # Cleanup
    rm -f "${checksum_tmp}" "${offline_checksums}"
    teardown_nfs_mount
    trap - EXIT

    log "=== OFFLINE scenario complete ==="
}

# =============================================================================
# Scenario: sync
# =============================================================================
run_sync() {
    log "=== SCENARIO: Auto-Sync on Reconnect ==="
    log "Testing that offline writes are synced after S3 returns"

    # Setup
    setup_nfs_mount
    trap 'restore_s3; teardown_nfs_mount' EXIT
    clean_server_data
    login_server

    dfsctl_server "share edit /export --retention pin"

    # Resolve S3 IPs
    local s3_ips
    s3_ips=$(resolve_s3_ips)

    # Block S3
    block_s3 "${s3_ips}"
    assert "S3 blocked for sync test" verify_s3_blocked

    # Wait for health to show degraded
    wait_for_degraded
    assert "Health shows degraded" [ "$(check_health_status)" = "degraded" ]

    # Write files while offline
    log "Writing files while S3 is blocked..."
    generate_test_files "edge-test-sync"
    local checksum_tmp
    checksum_tmp=$(mktemp)
    collect_checksums "edge-test-sync" > "${checksum_tmp}"
    local file_count
    file_count=$(wc -l < "${checksum_tmp}")
    log "Wrote ${file_count} files while offline"

    # Check pending uploads > 0
    local pending
    pending=$(get_pending_uploads 2>/dev/null || echo "0")
    log "Pending uploads while offline: ${pending}"
    assert "Pending uploads > 0 while S3 blocked" [ "${pending}" -gt 0 ]

    # Restore S3
    log "Restoring S3 connectivity..."
    restore_s3

    # Wait for sync completion
    log "Waiting for auto-sync to complete (timeout: ${SYNC_TIMEOUT}s)..."
    assert "Auto-sync completed within ${SYNC_TIMEOUT}s" wait_for_sync "${SYNC_TIMEOUT}"

    # Verify health endpoint back to healthy
    assert "Health endpoint shows 'healthy' after sync" \
        [ "$(check_health_status)" = "healthy" ]

    # Verify zero pending uploads
    assert "Zero pending uploads after sync" \
        [ "$(get_pending_uploads)" = "0" ]

    # Verify files still readable
    assert "All synced files readable with correct checksums" \
        verify_checksums "edge-test-sync" "${checksum_tmp}"

    # Cleanup
    rm -f "${checksum_tmp}"
    teardown_nfs_mount
    trap - EXIT

    log "=== SYNC scenario complete ==="
}

# =============================================================================
# Main dispatch
# =============================================================================

# Pre-flight: clean any stale iptables rules from a previous failed run
log "Pre-flight: cleaning stale iptables rules..."
restore_s3 2>/dev/null || true

case "${COMMAND}" in
    persist)  run_persist ;;
    offline)  run_offline ;;
    sync)     run_sync ;;
    all)
        run_persist
        run_offline
        run_sync
        ;;
    *)
        echo "Unknown command: ${COMMAND}"
        usage
        exit 1
        ;;
esac

summary
