#!/usr/bin/env bash
#
# Comprehensive DittoFS benchmark suite — all systems, all workloads, all NFS versions.
#
# Runs on the LOCAL machine (Mac), SSHs into server and client VMs.
#
# What this benchmarks:
#   - DittoFS: FS backend (NFSv3 + NFSv4.1)
#   - DittoFS: S3 backend (NFSv3 + NFSv4.1) + cold S3 reads
#   - Competitors: kernel-nfs, ganesha, rclone, juicefs, samba
#
# Workloads: seq-write, seq-read, rand-write, rand-read, metadata, small-files
#
# Usage:
#   ./scripts/run-full-bench.sh [round-name]

set -euo pipefail

SERVER=51.15.211.189
CLIENT=51.15.199.235
SSH="ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR -o ConnectTimeout=10"
SCP="scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o LogLevel=ERROR"

# Use direct SSH key, bypass 1Password agent
export SSH_AUTH_SOCK=""
SSH="$SSH -o IdentityAgent=none -i $HOME/.ssh/id_rsa"
SCP="$SCP -o IdentityAgent=none -i $HOME/.ssh/id_rsa"

ROUND=${1:-"round-$(date +%Y%m%d-%H%M)"}
RESULTS_DIR="results/$ROUND"
SCRIPTS_DIR="/opt/dittofs/bench/infra/scripts"

# Benchmark parameters (same for all systems)
DURATION=60s
THREADS=4
FILE_SIZE=1GiB
BLOCK_SIZE=4KiB
META_FILES=1000
SMALL_FILE_COUNT=10000
MOUNT_POINT=/mnt/bench

# DittoFS config
DITTOFS_ADMIN_PASSWORD="benchadmin123"
DITTOFS_SECRET="dittofs-bench-secret-key-for-jwt-1234567890"
S3_CONFIG='{"region":"fr-par","bucket":"dittofs-bench-payload","endpoint":"https://s3.fr-par.scw.cloud","access_key":"SCW8SK6RJTJEHPJXNC36","secret_key":"81bf6d6c-fc05-4cd6-a84d-9336c2f5eb80","force_path_style":true}'

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
log() {
    local ts
    ts="$(date '+%H:%M:%S')"
    echo "[${ts}] $*"
}

log_section() {
    echo ""
    echo "========================================================================"
    log "$*"
    echo "========================================================================"
}

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
ssh_server() { $SSH root@$SERVER "$@"; }
ssh_client() { $SSH root@$CLIENT "$@"; }

wait_for_port() {
    local host="$1" port="$2" timeout="${3:-60}"
    log "Waiting for ${host}:${port}..."
    for i in $(seq 1 "$timeout"); do
        if ssh_client "nc -z -w 2 $host $port 2>/dev/null"; then
            log "Port ${port} ready after ${i}s"
            return 0
        fi
        sleep 1
    done
    log "ERROR: Port ${port} not ready after ${timeout}s"
    return 1
}

drop_caches() {
    log "Dropping caches on client and server..."
    ssh_client 'sync 2>/dev/null; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null; true'
    ssh_server 'sync 2>/dev/null; echo 3 > /proc/sys/vm/drop_caches 2>/dev/null; true'
}

unmount_all() {
    ssh_client "umount -f $MOUNT_POINT 2>/dev/null; umount -l $MOUNT_POINT 2>/dev/null; rm -rf $MOUNT_POINT; mkdir -p $MOUNT_POINT; true"
    sleep 1
}

cleanup_server() {
    ssh_server 'killall -9 dfs 2>/dev/null; true'
    ssh_server 'systemctl stop nfs-kernel-server 2>/dev/null; true'
    ssh_server 'systemctl stop nfs-ganesha 2>/dev/null; true'
    ssh_server 'systemctl stop rclone-nfs 2>/dev/null; true'
    ssh_server 'killall -9 juicefs 2>/dev/null; true'
    ssh_server 'systemctl stop smbd 2>/dev/null; true'
    ssh_server 'fuser -k 2049/tcp 2>/dev/null; fuser -k 12049/tcp 2>/dev/null; fuser -k 445/tcp 2>/dev/null; true'
    sleep 2
}

run_bench() {
    local system="$1"
    local result_file="$2"

    log "Running benchmark: $system"
    ssh_client "dfsctl bench run $MOUNT_POINT \
        --system $system \
        --duration $DURATION \
        --threads $THREADS \
        --file-size $FILE_SIZE \
        --block-size $BLOCK_SIZE \
        --meta-files $META_FILES \
        --small-file-count $SMALL_FILE_COUNT \
        --save /tmp/${system}.json" 2>&1 | grep -E '(WORKLOAD|seq-|rand-|metadata|small-|Benchmarking)' | grep -v '%' || true

    $SCP root@$CLIENT:/tmp/${system}.json "$result_file" 2>/dev/null || {
        log "WARN: Failed to copy results for $system"
        return 0
    }
    log "Results saved: $result_file"
}

# ---------------------------------------------------------------------------
# DittoFS benchmark functions
# ---------------------------------------------------------------------------
start_dittofs() {
    log "Starting fresh DittoFS..."
    ssh_server "killall -9 dfs 2>/dev/null; sleep 2; \
        fuser -k 12049/tcp 2>/dev/null; fuser -k 8080/tcp 2>/dev/null; sleep 1; \
        rm -rf /root/.config/dittofs/controlplane.db /data/cache/* /data/metadata/* /data/payload/* /export/*; \
        DITTOFS_CONTROLPLANE_SECRET='$DITTOFS_SECRET' \
        DITTOFS_ADMIN_INITIAL_PASSWORD='$DITTOFS_ADMIN_PASSWORD' \
        nohup /usr/local/bin/dfs start --foreground --config /etc/dfs/config.yaml > /tmp/dfs.log 2>&1 &"
    sleep 6
    # Verify DFS is responding before continuing
    wait_for_port $SERVER 8080 30
}

configure_dittofs_fs() {
    log "Configuring DittoFS (FS backend)..."
    ssh_server "dfsctl login --server http://localhost:8080 --username admin --password '$DITTOFS_ADMIN_PASSWORD' && \
        dfsctl store metadata add --name badger-meta --type badger --db-path /data/metadata/badger && \
        dfsctl store block local add --name fs-payload --type filesystem --path /data/payload && \
        dfsctl share create --name /export --metadata badger-meta --local fs-payload"
}

configure_dittofs_s3() {
    log "Configuring DittoFS (S3 backend)..."
    ssh_server "dfsctl login --server http://localhost:8080 --username admin --password '$DITTOFS_ADMIN_PASSWORD' && \
        dfsctl store metadata add --name badger-meta --type badger --db-path /data/metadata/badger && \
        dfsctl store block local add --name local-payload --type memory && \
        dfsctl store block remote add --name s3-payload --type s3 --config '$S3_CONFIG' && \
        dfsctl share create --name /export --metadata badger-meta --local local-payload --remote s3-payload"
}

mount_nfs3() {
    local port="${1:-12049}"
    log "Mounting NFSv3 (port $port)..."
    ssh_client "mkdir -p $MOUNT_POINT && \
        mount -t nfs -o tcp,port=$port,mountport=$port,hard,vers=3,rsize=1048576,wsize=1048576 $SERVER:/export $MOUNT_POINT"
}

mount_nfs41() {
    local port="${1:-12049}"
    log "Mounting NFSv4.1 (port $port)..."
    ssh_client "mkdir -p $MOUNT_POINT && \
        mount -t nfs -o tcp,port=$port,nfsvers=4.1 $SERVER:/export $MOUNT_POINT"
}

# ---------------------------------------------------------------------------
# Cold S3 read benchmark
# ---------------------------------------------------------------------------
run_cold_s3_bench() {
    local system="$1"
    local result_file="$2"

    log "Running COLD S3 read benchmark: $system"

    # Step 1: Write data first (seq-write + rand-write to create test files)
    log "  Phase 1: Writing test data..."
    ssh_client "dfsctl bench run $MOUNT_POINT \
        --system ${system}-warmup \
        --duration $DURATION \
        --threads $THREADS \
        --file-size $FILE_SIZE \
        --block-size $BLOCK_SIZE \
        --workload seq-write,rand-write \
        --save /tmp/${system}-warmup.json" 2>&1 | grep -E '(seq-write|rand-write)' || true

    # Step 2: Wait for S3 uploads to complete (give 30s for async uploads)
    log "  Phase 2: Waiting for S3 uploads to complete (30s)..."
    sleep 30

    # Step 3: Clear DittoFS block cache on server (force S3 downloads on read)
    log "  Phase 3: Clearing DittoFS cache..."
    ssh_server 'rm -rf /data/cache/blocks/* 2>/dev/null; true'
    drop_caches

    # Step 4: Read the data back (this triggers S3 downloads)
    log "  Phase 4: Running cold reads..."
    ssh_client "dfsctl bench run $MOUNT_POINT \
        --system $system \
        --duration $DURATION \
        --threads $THREADS \
        --file-size $FILE_SIZE \
        --block-size $BLOCK_SIZE \
        --workload seq-read,rand-read \
        --save /tmp/${system}.json" 2>&1 | grep -E '(seq-read|rand-read|Benchmarking)' || true

    $SCP root@$CLIENT:/tmp/${system}.json "$result_file" 2>/dev/null || {
        log "WARN: Failed to copy cold read results for $system"
        return 0
    }
    log "Cold read results saved: $result_file"
}

# ===========================================================================
# MAIN
# ===========================================================================
main() {
    local start_time
    start_time=$(date +%s)

    log_section "DittoFS Comprehensive Benchmark Suite"
    log "Round:      $ROUND"
    log "Duration:   $DURATION per workload"
    log "Threads:    $THREADS"
    log "File size:  $FILE_SIZE"
    log "Server:     $SERVER"
    log "Client:     $CLIENT"

    mkdir -p "$RESULTS_DIR"

    # Clean up any stale mounts from prior runs
    unmount_all

    # -------------------------------------------------------------------
    # Phase 1: DittoFS — FS backend, NFSv3
    # -------------------------------------------------------------------
    log_section "DittoFS — FS backend, NFSv3"
    cleanup_server
    start_dittofs
    configure_dittofs_fs
    mount_nfs3 12049
    drop_caches
    run_bench "dittofs-fs-nfs3" "$RESULTS_DIR/dittofs-fs-nfs3.json"
    unmount_all

    # -------------------------------------------------------------------
    # Phase 2: DittoFS — FS backend, NFSv4.1
    # -------------------------------------------------------------------
    log_section "DittoFS — FS backend, NFSv4.1"
    unmount_all
    mount_nfs41 12049
    drop_caches
    run_bench "dittofs-fs-nfs41" "$RESULTS_DIR/dittofs-fs-nfs41.json"
    unmount_all

    # -------------------------------------------------------------------
    # Phase 3: DittoFS — S3 backend, NFSv3 (warm)
    # -------------------------------------------------------------------
    log_section "DittoFS — S3 backend, NFSv3 (warm)"
    cleanup_server
    start_dittofs
    configure_dittofs_s3
    mount_nfs3 12049
    drop_caches
    run_bench "dittofs-s3-nfs3" "$RESULTS_DIR/dittofs-s3-nfs3.json"

    # -------------------------------------------------------------------
    # Phase 4: DittoFS — S3 cold reads (NFSv3)
    # -------------------------------------------------------------------
    log_section "DittoFS — S3 cold reads, NFSv3"
    # Reuse the same DittoFS instance — just clear cache and re-read
    run_cold_s3_bench "dittofs-s3-cold-nfs3" "$RESULTS_DIR/dittofs-s3-cold-nfs3.json"
    unmount_all

    # -------------------------------------------------------------------
    # Phase 5: DittoFS — S3 backend, NFSv4.1 (warm)
    # -------------------------------------------------------------------
    log_section "DittoFS — S3 backend, NFSv4.1 (warm)"
    cleanup_server
    start_dittofs
    configure_dittofs_s3
    mount_nfs41 12049
    drop_caches
    run_bench "dittofs-s3-nfs41" "$RESULTS_DIR/dittofs-s3-nfs41.json"

    # -------------------------------------------------------------------
    # Phase 6: DittoFS — S3 cold reads (NFSv4.1)
    # -------------------------------------------------------------------
    log_section "DittoFS — S3 cold reads, NFSv4.1"
    run_cold_s3_bench "dittofs-s3-cold-nfs41" "$RESULTS_DIR/dittofs-s3-cold-nfs41.json"
    unmount_all

    # -------------------------------------------------------------------
    # Phase 7: kernel-nfs
    # -------------------------------------------------------------------
    log_section "kernel-nfs"
    cleanup_server
    ssh_server "bash $SCRIPTS_DIR/kernel-nfs.sh"
    wait_for_port $SERVER 2049
    mount_nfs3 2049
    drop_caches
    run_bench "kernel-nfs" "$RESULTS_DIR/kernel-nfs.json"
    unmount_all
    ssh_server "bash $SCRIPTS_DIR/kernel-nfs.sh stop" 2>/dev/null || true

    # -------------------------------------------------------------------
    # Phase 8: NFS-Ganesha
    # -------------------------------------------------------------------
    log_section "NFS-Ganesha"
    cleanup_server
    ssh_server "bash $SCRIPTS_DIR/ganesha.sh"
    wait_for_port $SERVER 2049
    mount_nfs3 2049
    drop_caches
    run_bench "ganesha" "$RESULTS_DIR/ganesha.json"
    unmount_all
    ssh_server "bash $SCRIPTS_DIR/ganesha.sh stop 2>/dev/null; systemctl stop nfs-ganesha 2>/dev/null; true"

    # -------------------------------------------------------------------
    # Phase 9: Rclone serve NFS
    # -------------------------------------------------------------------
    log_section "Rclone"
    cleanup_server
    ssh_server "bash $SCRIPTS_DIR/rclone.sh"
    wait_for_port $SERVER 2049
    ssh_client "mount -t nfs -o tcp,port=2049,mountport=2049,hard,vers=3,rsize=1048576,wsize=1048576 $SERVER:/export $MOUNT_POINT"
    drop_caches
    run_bench "rclone" "$RESULTS_DIR/rclone.json"
    unmount_all
    ssh_server "bash $SCRIPTS_DIR/rclone.sh stop" 2>/dev/null || true

    # -------------------------------------------------------------------
    # Phase 10: JuiceFS
    # -------------------------------------------------------------------
    log_section "JuiceFS"
    cleanup_server
    ssh_server "bash $SCRIPTS_DIR/juicefs.sh"
    wait_for_port $SERVER 2049 120
    mount_nfs3 2049
    drop_caches
    run_bench "juicefs" "$RESULTS_DIR/juicefs.json"
    unmount_all
    ssh_server "bash $SCRIPTS_DIR/juicefs.sh stop" 2>/dev/null || true

    # -------------------------------------------------------------------
    # Phase 11: Samba
    # -------------------------------------------------------------------
    log_section "Samba"
    cleanup_server
    ssh_server "bash $SCRIPTS_DIR/samba.sh"
    wait_for_port $SERVER 445
    ssh_client "mkdir -p $MOUNT_POINT && mount -t cifs -o username=bench,password=bench //${SERVER}/bench $MOUNT_POINT"
    drop_caches
    run_bench "samba" "$RESULTS_DIR/samba.json"
    unmount_all
    ssh_server "bash $SCRIPTS_DIR/samba.sh stop" 2>/dev/null || true

    # -------------------------------------------------------------------
    # Summary
    # -------------------------------------------------------------------
    local end_time
    end_time=$(date +%s)
    local total=$((end_time - start_time))
    local mins=$((total / 60))
    local secs=$((total % 60))

    log_section "BENCHMARK COMPLETE — ${mins}m ${secs}s total"
    log "Results directory: $RESULTS_DIR/"
    echo ""

    # List all result files
    ls -la "$RESULTS_DIR/"*.json 2>/dev/null || true
    echo ""

    # Run comparison if we have multiple results
    local result_files=()
    for f in "$RESULTS_DIR"/*.json; do
        [ -f "$f" ] && result_files+=("$f")
    done

    if [ "${#result_files[@]}" -ge 2 ]; then
        log "Running comparison..."
        dfsctl bench compare "${result_files[@]}" 2>&1 || true
    fi
}

main "$@"
