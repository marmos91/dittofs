#!/usr/bin/env bash
# run-all.sh — Benchmark orchestrator for DittoFS competitor comparison
#
# Runs from the CLIENT VM. For each competitor system it:
#   1. SSHs into the server VM to install/start the service
#   2. Waits for the service port to be reachable
#   3. Mounts the remote share on the client (NFS or CIFS)
#   4. Runs dfsctl bench run against the mount
#   5. Unmounts and cleans up
#   6. SSHs into the server to stop/remove the service
#   7. Drops client caches between runs
#
# Usage:
#   ./run-all.sh                             # Benchmark all systems
#   ./run-all.sh --dry-run                   # Show what would be done
#   SYSTEMS=kernel-nfs,samba ./run-all.sh    # Benchmark specific systems
#
# See BENCHMARK-PLAN.md Phase E for the full design.

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration (override via environment variables)
# ---------------------------------------------------------------------------
SERVER_IP="${SERVER_IP:?SERVER_IP is required (IP of the server VM)}"
SSH_KEY="${SSH_KEY:-$HOME/.ssh/id_rsa}"
SSH_USER="${SSH_USER:-root}"
BENCH_THREADS="${BENCH_THREADS:-4}"
BENCH_FILE_SIZE="${BENCH_FILE_SIZE:-1GiB}"
BENCH_BLOCK_SIZE="${BENCH_BLOCK_SIZE:-4KiB}"
BENCH_DURATION="${BENCH_DURATION:-60s}"
BENCH_META_FILES="${BENCH_META_FILES:-1000}"
RESULTS_DIR="${RESULTS_DIR:-./results}"
MOUNT_POINT="${MOUNT_POINT:-/mnt/bench}"
SCRIPTS_DIR="${SCRIPTS_DIR:-/opt/dittofs/bench/infra/scripts}"

# Comma-separated list of systems to benchmark (empty = all).
SYSTEMS="${SYSTEMS:-}"

# ---------------------------------------------------------------------------
# Flags
# ---------------------------------------------------------------------------
DRY_RUN=false

for arg in "$@"; do
    case "$arg" in
        --dry-run) DRY_RUN=true ;;
        --help|-h)
            cat <<'USAGE'
Usage: run-all.sh [OPTIONS]

Orchestrates DittoFS benchmarks against all competitor systems.
Must be run from the client VM with SSH access to the server VM.

Options:
  --dry-run   Show what would be done without executing anything
  --help      Show this help message

Environment Variables:
  SERVER_IP        Server VM IP address (required)
  SSH_KEY          Path to SSH private key (default: ~/.ssh/id_rsa)
  SSH_USER         SSH user on server VM (default: root)
  BENCH_THREADS    Number of concurrent I/O workers (default: 4)
  BENCH_FILE_SIZE  Test file size (default: 1GiB)
  BENCH_BLOCK_SIZE I/O block size for random workloads (default: 4KiB)
  BENCH_DURATION   Duration for time-based workloads (default: 60s)
  BENCH_META_FILES Number of files for metadata workload (default: 1000)
  RESULTS_DIR      Directory for JSON result files (default: ./results)
  MOUNT_POINT      Local mount point (default: /mnt/bench)
  SCRIPTS_DIR      Path to install scripts on server (default: /opt/dittofs/bench/infra/scripts)
  SYSTEMS          Comma-separated systems to benchmark (default: all)
                   DittoFS (badger+fs): dittofs-badger-fs-nfs3, dittofs-badger-fs-nfs4,
                     dittofs-badger-fs-smb2, dittofs-badger-fs-smb3
                   DittoFS (badger+s3): dittofs-badger-s3-nfs3, dittofs-badger-s3-nfs4,
                     dittofs-badger-s3-smb2, dittofs-badger-s3-smb3
                   Competitors (local): kernel-nfs, ganesha, rclone, samba, juicefs
                   Competitors (S3): rclone-s3, juicefs-s3, s3ql
USAGE
            exit 0
            ;;
        *)
            echo "Unknown option: $arg (use --help)" >&2
            exit 1
            ;;
    esac
done

# ---------------------------------------------------------------------------
# System definitions (mirrors bench/infra/systems.go)
# ---------------------------------------------------------------------------
# Format: name|protocol|port|mount_opts|install_script|export_path
#
# protocol: nfs, smb, fuse
# port: service port on the server (0 for FUSE-based systems)
# mount_opts: additional mount -o options
# install_script: filename in $SCRIPTS_DIR on the server
# export_path: remote export/share path
#
# DittoFS admin password used for SMB mounts (set in install scripts)
DITTOFS_ADMIN_PASSWORD="dittofs-bench-admin-1234567890"

ALL_SYSTEMS=(
    # ===== DittoFS: BadgerDB metadata + filesystem payload =====
    "dittofs-badger-fs-nfs3|nfs|12049|tcp,port=12049,mountport=12049,nfsvers=3|dittofs-badger-fs.sh|/export"
    "dittofs-badger-fs-nfs4|nfs|12049|tcp,port=12049,nfsvers=4.1|dittofs-badger-fs.sh|/export"
    "dittofs-badger-fs-smb2|smb|1445|username=admin,password=${DITTOFS_ADMIN_PASSWORD},port=1445,vers=2.1|dittofs-badger-fs.sh|/export"
    "dittofs-badger-fs-smb3|smb|1445|username=admin,password=${DITTOFS_ADMIN_PASSWORD},port=1445,vers=3.0|dittofs-badger-fs.sh|/export"

    # ===== DittoFS: BadgerDB metadata + S3 payload =====
    # Requires S3_BUCKET, S3_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY env vars
    "dittofs-badger-s3-nfs3|nfs|12049|tcp,port=12049,mountport=12049,nfsvers=3|dittofs-badger-s3.sh|/export"
    "dittofs-badger-s3-nfs4|nfs|12049|tcp,port=12049,nfsvers=4.1|dittofs-badger-s3.sh|/export"
    "dittofs-badger-s3-smb2|smb|1445|username=admin,password=${DITTOFS_ADMIN_PASSWORD},port=1445,vers=2.1|dittofs-badger-s3.sh|/export"
    "dittofs-badger-s3-smb3|smb|1445|username=admin,password=${DITTOFS_ADMIN_PASSWORD},port=1445,vers=3.0|dittofs-badger-s3.sh|/export"

    # ===== Competitors (local storage) =====
    "kernel-nfs|nfs|2049|tcp|kernel-nfs.sh|/export"
    "ganesha|nfs|2049|tcp|ganesha.sh|/export"
    "rclone|nfs|2049|tcp,port=2049,mountport=2049,nfsvers=3|rclone.sh|/export"
    "samba|smb|445|username=bench,password=bench|samba.sh|/bench"
    "juicefs|nfs|2049|tcp|juicefs.sh|/export"

    # ===== Competitors (S3-backed) =====
    # Requires S3_BUCKET, S3_REGION, AWS_ACCESS_KEY_ID, AWS_SECRET_ACCESS_KEY env vars
    "rclone-s3|nfs|2049|tcp,port=2049,mountport=2049,nfsvers=3|rclone-s3.sh|/export"
    "juicefs-s3|nfs|2049|tcp|juicefs-s3.sh|/export"
    "s3ql|nfs|2049|tcp|s3ql.sh|/export"
)

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
LOG_FILE="${RESULTS_DIR}/run-all.log"

log() {
    local ts
    ts="$(date '+%Y-%m-%d %H:%M:%S')"
    echo "[${ts}] $*" | tee -a "$LOG_FILE" >&2
}

log_separator() {
    log "========================================================================"
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# Run a command on the server VM via SSH.
ssh_server() {
    ssh -o StrictHostKeyChecking=no \
        -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 \
        -o LogLevel=ERROR \
        -i "$SSH_KEY" \
        "${SSH_USER}@${SERVER_IP}" \
        "$@"
}

# Run a command, or just print it in dry-run mode.
run() {
    if $DRY_RUN; then
        log "[DRY-RUN] $*"
    else
        "$@"
    fi
}

# Wait for a TCP port to become reachable on the server. Times out after 120s.
wait_for_port() {
    local port="$1"
    local timeout=120
    local elapsed=0
    local interval=2

    log "Waiting for ${SERVER_IP}:${port} to become reachable (timeout ${timeout}s)..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would wait for port ${port}"
        return 0
    fi

    while [ "$elapsed" -lt "$timeout" ]; do
        if nc -z -w 2 "$SERVER_IP" "$port" 2>/dev/null; then
            log "Port ${port} is reachable after ${elapsed}s."
            return 0
        fi
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    log "ERROR: Port ${port} not reachable after ${timeout}s."
    return 1
}

# Wait for the server to be reachable via SSH. Times out after 120s.
wait_for_ssh() {
    local timeout=120
    local elapsed=0
    local interval=5

    log "Waiting for SSH on ${SERVER_IP}..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would wait for SSH"
        return 0
    fi

    while [ "$elapsed" -lt "$timeout" ]; do
        if ssh_server "true" 2>/dev/null; then
            log "SSH is reachable after ${elapsed}s."
            return 0
        fi
        sleep "$interval"
        elapsed=$((elapsed + interval))
    done

    log "ERROR: SSH not reachable after ${timeout}s."
    return 1
}

# Drop filesystem caches on the local client.
drop_client_caches() {
    log "Dropping client OS caches..."
    if $DRY_RUN; then
        log "[DRY-RUN] Would run: sync && echo 3 > /proc/sys/vm/drop_caches"
        return 0
    fi
    timeout 10 sync 2>/dev/null || log "WARN: sync timed out after 10s (NFS mount may be slow)"
    echo 3 > /proc/sys/vm/drop_caches 2>/dev/null || log "WARN: Could not drop caches (not root?)"
}

# Drop filesystem caches on the remote server.
drop_server_caches() {
    log "Dropping server OS caches..."
    if $DRY_RUN; then
        log "[DRY-RUN] Would SSH to server and run: sync && echo 3 > /proc/sys/vm/drop_caches"
        return 0
    fi
    ssh_server "sync && echo 3 > /proc/sys/vm/drop_caches" 2>/dev/null \
        || log "WARN: Could not drop server caches"
}

# Evict DittoFS server-side caches (L1 read cache + local disk cache).
# Only meaningful for DittoFS systems; no-op for competitors.
evict_dittofs_cache() {
    local name="$1"

    # Only applies to DittoFS systems.
    case "$name" in
        dittofs-*) ;;
        *) return 0 ;;
    esac

    log "[${name}] Evicting DittoFS server-side cache via dfsctl..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would SSH to server and run: dfsctl cache evict"
        return 0
    fi

    if ! ssh_server "dfsctl cache evict -v" 2>&1; then
        log "WARN: dfsctl cache evict failed (server cache may still be warm)"
    fi
}

# Drop all caches: DittoFS application cache (if applicable), server OS, client OS.
drop_all_caches() {
    local name="$1"
    evict_dittofs_cache "$name"
    drop_server_caches
    drop_client_caches
}

# Mount a remote share on the client.
mount_share() {
    local protocol="$1"
    local port="$2"
    local mount_opts="$3"
    local export_path="$4"

    log "Mounting ${protocol}://${SERVER_IP}${export_path} at ${MOUNT_POINT}..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would mount ${protocol} share"
        return 0
    fi

    mkdir -p "$MOUNT_POINT"

    case "$protocol" in
        nfs)
            mount -t nfs -o "${mount_opts}" \
                "${SERVER_IP}:${export_path}" "$MOUNT_POINT"
            ;;
        smb)
            mount -t cifs -o "${mount_opts}" \
                "//${SERVER_IP}${export_path}" "$MOUNT_POINT"
            ;;
        *)
            log "ERROR: Unsupported protocol '${protocol}'"
            return 1
            ;;
    esac

    # Verify mount succeeded.
    if mountpoint -q "$MOUNT_POINT"; then
        log "Mount successful."
    else
        log "ERROR: ${MOUNT_POINT} is not a mount point after mount command."
        return 1
    fi
}

# Unmount the benchmark share.
unmount_share() {
    log "Unmounting ${MOUNT_POINT}..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would unmount ${MOUNT_POINT}"
        return 0
    fi

    if mountpoint -q "$MOUNT_POINT" 2>/dev/null; then
        # Give any lingering I/O a moment to settle.
        timeout 10 sync 2>/dev/null || true
        sleep 1
        umount -f "$MOUNT_POINT" 2>/dev/null || umount -l "$MOUNT_POINT" 2>/dev/null || true
    fi
}

# Install and start a competitor service on the server via SSH.
install_competitor() {
    local name="$1"
    local script="$2"

    log "Installing '${name}' on server via ${SCRIPTS_DIR}/${script}..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would SSH to server and run ${SCRIPTS_DIR}/${script}"
        return 0
    fi

    ssh_server "DITTOFS_BRANCH=${DITTOFS_BRANCH:-main} \
        S3_BUCKET=${S3_BUCKET:-} S3_REGION=${S3_REGION:-} \
        S3_ENDPOINT=${S3_ENDPOINT:-} \
        AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-} \
        AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-} \
        bash ${SCRIPTS_DIR}/${script}"
}

# Stop and clean up a competitor service on the server.
cleanup_competitor() {
    local name="$1"
    local script="$2"

    log "Stopping '${name}' on server..."

    if $DRY_RUN; then
        log "[DRY-RUN] Would SSH to server and run ${SCRIPTS_DIR}/${script} stop"
        return 0
    fi

    # Convention: each install script supports a "stop" argument to tear down.
    # Forward S3 env vars for S3-backed scripts that need them during cleanup.
    ssh_server "S3_BUCKET=${S3_BUCKET:-} S3_REGION=${S3_REGION:-} \
        S3_ENDPOINT=${S3_ENDPOINT:-} \
        AWS_ACCESS_KEY_ID=${AWS_ACCESS_KEY_ID:-} \
        AWS_SECRET_ACCESS_KEY=${AWS_SECRET_ACCESS_KEY:-} \
        bash ${SCRIPTS_DIR}/${script} stop" 2>/dev/null || true

    # Belt-and-suspenders: kill anything still on common service ports.
    ssh_server "fuser -k 2049/tcp 2>/dev/null; \
        systemctl stop nfs-kernel-server 2>/dev/null; \
        systemctl stop rclone-nfs 2>/dev/null; \
        systemctl stop juicefs-mount 2>/dev/null; \
        umount.s3ql /export 2>/dev/null; \
        umount /export 2>/dev/null; \
        true" 2>/dev/null || true
}

# Parse a system definition string into individual variables.
# Sets: SYS_NAME, SYS_PROTOCOL, SYS_PORT, SYS_MOUNT_OPTS, SYS_SCRIPT, SYS_EXPORT
parse_system() {
    local def="$1"
    IFS='|' read -r SYS_NAME SYS_PROTOCOL SYS_PORT SYS_MOUNT_OPTS SYS_SCRIPT SYS_EXPORT <<< "$def"
}

# Check whether a system name should be benchmarked.
should_run() {
    local name="$1"
    if [ -z "$SYSTEMS" ]; then
        return 0  # Run all if no filter specified.
    fi
    echo ",$SYSTEMS," | grep -q ",${name},"
}

# Format elapsed seconds as Xm Ys.
format_duration() {
    local secs="$1"
    local mins=$((secs / 60))
    local rem=$((secs % 60))
    if [ "$mins" -gt 0 ]; then
        echo "${mins}m ${rem}s"
    else
        echo "${rem}s"
    fi
}

# ---------------------------------------------------------------------------
# Pre-flight checks
# ---------------------------------------------------------------------------
preflight() {
    log "Running pre-flight checks..."

    local errors=0

    # Check required tools on the client.
    for tool in ssh nc mount umount mountpoint dfsctl jq; do
        if ! command -v "$tool" &>/dev/null; then
            log "ERROR: Required tool '${tool}' not found in PATH."
            errors=$((errors + 1))
        fi
    done

    # Check SSH key exists.
    if [ ! -f "$SSH_KEY" ]; then
        log "ERROR: SSH key not found at ${SSH_KEY}"
        errors=$((errors + 1))
    fi

    # Check we can reach the server via SSH.
    if ! $DRY_RUN; then
        if ! ssh_server "true" 2>/dev/null; then
            log "ERROR: Cannot SSH to ${SSH_USER}@${SERVER_IP}"
            errors=$((errors + 1))
        fi
    fi

    # Check CIFS utilities are available if samba is in the system list.
    if should_run "samba"; then
        if ! command -v mount.cifs &>/dev/null && [ ! -f /sbin/mount.cifs ]; then
            log "WARN: mount.cifs not found; samba benchmarks may fail. Install cifs-utils."
        fi
    fi

    if [ "$errors" -gt 0 ]; then
        log "Pre-flight checks failed with ${errors} error(s). Aborting."
        exit 1
    fi

    log "Pre-flight checks passed."
}

# ---------------------------------------------------------------------------
# Main benchmark loop
# ---------------------------------------------------------------------------
main() {
    local start_time
    start_time="$(date +%s)"

    log_separator
    log "DittoFS Benchmark Orchestrator"
    log_separator
    log ""
    log "Server:      ${SERVER_IP}"
    log "SSH user:    ${SSH_USER}"
    log "Threads:     ${BENCH_THREADS}"
    log "File size:   ${BENCH_FILE_SIZE}"
    log "Block size:  ${BENCH_BLOCK_SIZE}"
    log "Duration:    ${BENCH_DURATION}"
    log "Meta files:  ${BENCH_META_FILES}"
    log "Mount point: ${MOUNT_POINT}"
    log "Results dir: ${RESULTS_DIR}"
    if [ -n "$SYSTEMS" ]; then
        log "Systems:     ${SYSTEMS}"
    else
        log "Systems:     all"
    fi
    if $DRY_RUN; then
        log "Mode:        DRY RUN"
    fi
    log ""

    # Create results directory and log file.
    mkdir -p "$RESULTS_DIR"
    : > "$LOG_FILE"  # Truncate log file for this run.

    # Pre-flight checks.
    preflight

    # Track which systems succeeded and failed.
    local succeeded=()
    local failed=()
    local skipped=()

    # -----------------------------------------------------------------------
    # Iterate over each competitor system.
    # -----------------------------------------------------------------------
    for sys_def in "${ALL_SYSTEMS[@]}"; do
        parse_system "$sys_def"

        if ! should_run "$SYS_NAME"; then
            skipped+=("$SYS_NAME")
            continue
        fi

        log_separator
        log "SYSTEM: ${SYS_NAME} (${SYS_PROTOCOL}, port ${SYS_PORT})"
        log_separator

        local sys_start
        sys_start="$(date +%s)"
        local result_file="${RESULTS_DIR}/${SYS_NAME}.json"

        # Use a subshell-like trap so failures don't abort the entire run.
        if benchmark_system "$SYS_NAME" "$SYS_PROTOCOL" "$SYS_PORT" \
                            "$SYS_MOUNT_OPTS" "$SYS_SCRIPT" "$SYS_EXPORT" \
                            "$result_file"; then
            local sys_end
            sys_end="$(date +%s)"
            local sys_elapsed=$((sys_end - sys_start))
            log "SUCCESS: ${SYS_NAME} completed in $(format_duration $sys_elapsed)"
            succeeded+=("$SYS_NAME")
        else
            local sys_end
            sys_end="$(date +%s)"
            local sys_elapsed=$((sys_end - sys_start))
            log "FAILED: ${SYS_NAME} failed after $(format_duration $sys_elapsed)"
            failed+=("$SYS_NAME")
        fi

        # Always clean up between systems.
        unmount_share
        cleanup_competitor "$SYS_NAME" "$SYS_SCRIPT"
        drop_client_caches

        log ""
    done

    # -----------------------------------------------------------------------
    # Summary
    # -----------------------------------------------------------------------
    local end_time
    end_time="$(date +%s)"
    local total_elapsed=$((end_time - start_time))

    log_separator
    log "BENCHMARK SUMMARY"
    log_separator
    log ""
    log "Total time: $(format_duration $total_elapsed)"
    log ""

    if [ "${#succeeded[@]}" -gt 0 ]; then
        log "Succeeded (${#succeeded[@]}):"
        for name in "${succeeded[@]}"; do
            local rf="${RESULTS_DIR}/${name}.json"
            log "  - ${name} -> ${rf}"
        done
    fi

    if [ "${#failed[@]}" -gt 0 ]; then
        log "Failed (${#failed[@]}):"
        for name in "${failed[@]}"; do
            log "  - ${name}"
        done
    fi

    if [ "${#skipped[@]}" -gt 0 ]; then
        log "Skipped (${#skipped[@]}):"
        for name in "${skipped[@]}"; do
            log "  - ${name}"
        done
    fi

    log ""

    # -----------------------------------------------------------------------
    # Run comparison if we have 2+ successful results.
    # -----------------------------------------------------------------------
    if [ "${#succeeded[@]}" -ge 2 ]; then
        log "Running dfsctl bench compare across successful results..."

        local compare_files=()
        for name in "${succeeded[@]}"; do
            compare_files+=("${RESULTS_DIR}/${name}.json")
        done

        if $DRY_RUN; then
            log "[DRY-RUN] dfsctl bench compare ${compare_files[*]}"
        else
            dfsctl bench compare "${compare_files[@]}" 2>&1 | tee -a "$LOG_FILE" || \
                log "WARN: dfsctl bench compare returned non-zero exit code."
        fi
    elif [ "${#succeeded[@]}" -eq 1 ]; then
        log "Only one system succeeded; skipping comparison (need 2+)."
    else
        log "No systems succeeded; nothing to compare."
    fi

    log ""
    log "Log file: ${LOG_FILE}"
    log_separator

    # Exit with failure if any systems failed.
    if [ "${#failed[@]}" -gt 0 ]; then
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# benchmark_system — Run the full benchmark cycle for one competitor.
#
# This function is called from the main loop. It returns 0 on success, 1 on
# failure. The caller handles cleanup regardless of the return code.
# ---------------------------------------------------------------------------
benchmark_system() {
    local name="$1"
    local protocol="$2"
    local port="$3"
    local mount_opts="$4"
    local script="$5"
    local export_path="$6"
    local result_file="$7"

    # Step 1: Install and start the competitor on the server.
    log "[${name}] Step 1/7: Installing competitor on server..."
    if ! install_competitor "$name" "$script"; then
        log "[${name}] ERROR: Install script failed."
        return 1
    fi

    # Step 2: Wait for the service to be ready.
    if [ "$port" -gt 0 ]; then
        log "[${name}] Step 2/7: Waiting for service on port ${port}..."
        if ! wait_for_port "$port"; then
            log "[${name}] ERROR: Service did not become ready."
            return 1
        fi
    else
        # FUSE-based systems (like juicefs) expose an NFS gateway or mount
        # directly. Give the service a few seconds to stabilize.
        log "[${name}] Step 2/7: No port to check (FUSE); waiting 10s for startup..."
        if ! $DRY_RUN; then
            sleep 10
        fi
    fi

    # Step 3: Mount the remote share on the client.
    log "[${name}] Step 3/7: Mounting share..."
    if ! mount_share "$protocol" "$port" "$mount_opts" "$export_path"; then
        log "[${name}] ERROR: Mount failed."
        return 1
    fi

    # Step 4: Drop all caches before benchmarking.
    log "[${name}] Step 4/7: Dropping caches before benchmark..."
    drop_all_caches "$name"

    # Step 5: Run full benchmark (writes + warm reads).
    # Reads immediately follow writes, so data is warm in cache.
    # Files are kept by default for the cold read pass in step 7.
    log "[${name}] Step 5/7: Running full benchmark (warm reads)..."

    local bench_cmd=(
        dfsctl bench run "$MOUNT_POINT"
        --system "$name"
        --threads "$BENCH_THREADS"
        --file-size "$BENCH_FILE_SIZE"
        --block-size "$BENCH_BLOCK_SIZE"
        --duration "$BENCH_DURATION"
        --meta-files "$BENCH_META_FILES"
        --save "$result_file"
    )

    if $DRY_RUN; then
        log "[DRY-RUN] ${bench_cmd[*]}"
    else
        log "[${name}] Running: ${bench_cmd[*]}"
        if ! "${bench_cmd[@]}" 2>&1 | tee -a "$LOG_FILE"; then
            log "[${name}] ERROR: dfsctl bench run failed."
            return 1
        fi
    fi

    log "[${name}] Warm benchmark complete. Results: ${result_file}"

    # Step 6: Evict all caches for cold read test.
    # For DittoFS: evicts L1 read cache + local disk cache via dfsctl.
    # For all systems: drops server and client OS page caches.
    log "[${name}] Step 6/7: Evicting all caches for cold read test..."
    drop_all_caches "$name"

    # Step 7: Run cold read benchmark (seq-read + rand-read only).
    # All server and client caches are cold — measures true read-from-storage perf.
    # Test files from step 5 are still on disk (--keep-files).
    local cold_result_file="${result_file%.json}-cold.json"
    log "[${name}] Step 7/7: Running cold read benchmark..."

    local cold_bench_cmd=(
        dfsctl bench run "$MOUNT_POINT"
        --system "${name}-cold"
        --threads "$BENCH_THREADS"
        --file-size "$BENCH_FILE_SIZE"
        --block-size "$BENCH_BLOCK_SIZE"
        --duration "$BENCH_DURATION"
        --workload "seq-read,rand-read"
        --clean
        --save "$cold_result_file"
    )

    if $DRY_RUN; then
        log "[DRY-RUN] ${cold_bench_cmd[*]}"
    else
        log "[${name}] Running: ${cold_bench_cmd[*]}"
        if ! "${cold_bench_cmd[@]}" 2>&1 | tee -a "$LOG_FILE"; then
            log "[${name}] WARN: Cold read benchmark failed (warm results still valid)."
        else
            log "[${name}] Cold read benchmark complete. Results: ${cold_result_file}"
        fi
    fi

    return 0
}

# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------
main
