#!/usr/bin/env bash
# s3-baseline.sh — Raw S3 PUT/GET throughput baseline
#
# Measures direct S3 performance without any filesystem layer.
# Runs from the server VM using rclone as the S3 client.
#
# Tests:
#   1. Sequential PUT (upload N files of given size)
#   2. Sequential GET (download those files)
#   3. Single large PUT/GET (sustained throughput)
#   4. Metadata ops (HEAD/LIST latency)
#
# Requires: rclone configured with [scw] remote pointing to Scaleway S3.
#
# Usage:
#   S3_BUCKET=dittofs-bench S3_REGION=fr-par \
#   AWS_ACCESS_KEY_ID=... AWS_SECRET_ACCESS_KEY=... \
#   bash s3-baseline.sh

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
: "${S3_BUCKET:?S3_BUCKET is required}"
: "${S3_REGION:?S3_REGION is required}"
: "${AWS_ACCESS_KEY_ID:?AWS_ACCESS_KEY_ID is required}"
: "${AWS_SECRET_ACCESS_KEY:?AWS_SECRET_ACCESS_KEY is required}"
S3_ENDPOINT="${S3_ENDPOINT:-s3.${S3_REGION}.scw.cloud}"

SMALL_FILE_SIZE="${SMALL_FILE_SIZE:-4M}"       # Size for multi-file tests
SMALL_FILE_COUNT="${SMALL_FILE_COUNT:-50}"      # Number of small files
LARGE_FILE_SIZE="${LARGE_FILE_SIZE:-256M}"      # Size for single-file throughput
S3_PREFIX="s3-baseline"
LOCAL_DIR="/tmp/s3-baseline"
RESULTS_FILE="${RESULTS_FILE:-/tmp/s3-baseline-results.json}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------
log() { echo "[s3-baseline] $(date '+%H:%M:%S') $*"; }

ensure_rclone_config() {
    mkdir -p /root/.config/rclone
    cat > /root/.config/rclone/rclone.conf <<CONF
[scw]
type = s3
provider = Scaleway
access_key_id = ${AWS_ACCESS_KEY_ID}
secret_access_key = ${AWS_SECRET_ACCESS_KEY}
region = ${S3_REGION}
endpoint = ${S3_ENDPOINT}
acl = private
CONF
}

# Generate a random file of given size.
generate_file() {
    local path="$1"
    local size="$2"
    dd if=/dev/urandom of="$path" bs=1M count="$(echo "$size" | sed 's/M//')" 2>/dev/null
}

# Measure wall-clock time of a command in milliseconds.
time_ms() {
    local start end
    start=$(date +%s%N)
    "$@"
    end=$(date +%s%N)
    echo $(( (end - start) / 1000000 ))
}

cleanup_s3() {
    rclone purge "scw:${S3_BUCKET}/${S3_PREFIX}/" 2>/dev/null || true
}

cleanup_local() {
    rm -rf "${LOCAL_DIR}"
}

# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

# Test 1: Multi-file PUT (upload N small files)
test_multi_put() {
    local count="$1"
    local size="$2"
    local size_bytes
    size_bytes=$(( $(echo "$size" | sed 's/M//') * 1048576 ))

    log "TEST: Multi-file PUT — ${count} x ${size} files..."
    cleanup_s3

    # Generate files
    mkdir -p "${LOCAL_DIR}/upload"
    for i in $(seq 1 "$count"); do
        generate_file "${LOCAL_DIR}/upload/file-${i}.bin" "$size"
    done

    # Upload all files
    local start end elapsed_ms
    start=$(date +%s%N)
    rclone copy "${LOCAL_DIR}/upload/" "scw:${S3_BUCKET}/${S3_PREFIX}/multi/" \
        --transfers 4 --no-check-dest -q
    end=$(date +%s%N)
    elapsed_ms=$(( (end - start) / 1000000 ))

    local total_bytes=$(( size_bytes * count ))
    local throughput_mbps
    throughput_mbps=$(echo "scale=2; ${total_bytes} / 1048576 / (${elapsed_ms} / 1000)" | bc)
    local avg_latency_ms=$(( elapsed_ms / count ))

    log "  Files: ${count}, Total: $(( total_bytes / 1048576 )) MB"
    log "  Time: ${elapsed_ms} ms, Throughput: ${throughput_mbps} MB/s"
    log "  Avg latency per file: ${avg_latency_ms} ms"

    echo "{\"test\":\"multi-put\",\"files\":${count},\"file_size_bytes\":${size_bytes},\"total_bytes\":${total_bytes},\"elapsed_ms\":${elapsed_ms},\"throughput_mbps\":${throughput_mbps},\"avg_latency_ms\":${avg_latency_ms}}"
}

# Test 2: Multi-file GET (download N small files)
test_multi_get() {
    local count="$1"
    local size="$2"
    local size_bytes
    size_bytes=$(( $(echo "$size" | sed 's/M//') * 1048576 ))

    log "TEST: Multi-file GET — ${count} x ${size} files..."
    mkdir -p "${LOCAL_DIR}/download"

    local start end elapsed_ms
    start=$(date +%s%N)
    rclone copy "scw:${S3_BUCKET}/${S3_PREFIX}/multi/" "${LOCAL_DIR}/download/" \
        --transfers 4 -q
    end=$(date +%s%N)
    elapsed_ms=$(( (end - start) / 1000000 ))

    local total_bytes=$(( size_bytes * count ))
    local throughput_mbps
    throughput_mbps=$(echo "scale=2; ${total_bytes} / 1048576 / (${elapsed_ms} / 1000)" | bc)
    local avg_latency_ms=$(( elapsed_ms / count ))

    log "  Files: ${count}, Total: $(( total_bytes / 1048576 )) MB"
    log "  Time: ${elapsed_ms} ms, Throughput: ${throughput_mbps} MB/s"
    log "  Avg latency per file: ${avg_latency_ms} ms"

    echo "{\"test\":\"multi-get\",\"files\":${count},\"file_size_bytes\":${size_bytes},\"total_bytes\":${total_bytes},\"elapsed_ms\":${elapsed_ms},\"throughput_mbps\":${throughput_mbps},\"avg_latency_ms\":${avg_latency_ms}}"
}

# Test 3: Single large file PUT
test_large_put() {
    local size="$1"
    local size_bytes
    size_bytes=$(( $(echo "$size" | sed 's/M//') * 1048576 ))

    log "TEST: Large file PUT — 1 x ${size}..."
    cleanup_s3
    mkdir -p "${LOCAL_DIR}/large"
    generate_file "${LOCAL_DIR}/large/bigfile.bin" "$size"

    local start end elapsed_ms
    start=$(date +%s%N)
    rclone copyto "${LOCAL_DIR}/large/bigfile.bin" "scw:${S3_BUCKET}/${S3_PREFIX}/large/bigfile.bin" -q
    end=$(date +%s%N)
    elapsed_ms=$(( (end - start) / 1000000 ))

    local throughput_mbps
    throughput_mbps=$(echo "scale=2; ${size_bytes} / 1048576 / (${elapsed_ms} / 1000)" | bc)

    log "  Size: $(( size_bytes / 1048576 )) MB"
    log "  Time: ${elapsed_ms} ms, Throughput: ${throughput_mbps} MB/s"

    echo "{\"test\":\"large-put\",\"file_size_bytes\":${size_bytes},\"elapsed_ms\":${elapsed_ms},\"throughput_mbps\":${throughput_mbps}}"
}

# Test 4: Single large file GET
test_large_get() {
    local size="$1"
    local size_bytes
    size_bytes=$(( $(echo "$size" | sed 's/M//') * 1048576 ))

    log "TEST: Large file GET — 1 x ${size}..."
    mkdir -p "${LOCAL_DIR}/large-get"

    local start end elapsed_ms
    start=$(date +%s%N)
    rclone copyto "scw:${S3_BUCKET}/${S3_PREFIX}/large/bigfile.bin" "${LOCAL_DIR}/large-get/bigfile.bin" -q
    end=$(date +%s%N)
    elapsed_ms=$(( (end - start) / 1000000 ))

    local throughput_mbps
    throughput_mbps=$(echo "scale=2; ${size_bytes} / 1048576 / (${elapsed_ms} / 1000)" | bc)

    log "  Size: $(( size_bytes / 1048576 )) MB"
    log "  Time: ${elapsed_ms} ms, Throughput: ${throughput_mbps} MB/s"

    echo "{\"test\":\"large-get\",\"file_size_bytes\":${size_bytes},\"elapsed_ms\":${elapsed_ms},\"throughput_mbps\":${throughput_mbps}}"
}

# Test 5: Metadata operations (HEAD / LIST latency)
test_metadata() {
    local iterations="${1:-100}"

    log "TEST: Metadata ops — ${iterations} iterations..."

    # HEAD (stat) latency — use rclone lsf on a known single file
    local head_total_ms
    head_total_ms=0
    for i in $(seq 1 "$iterations"); do
        local start end
        start=$(date +%s%N)
        rclone lsf "scw:${S3_BUCKET}/${S3_PREFIX}/large/bigfile.bin" -q 2>/dev/null || true
        end=$(date +%s%N)
        head_total_ms=$(( head_total_ms + (end - start) / 1000000 ))
    done
    local head_avg_ms=$(( head_total_ms / iterations ))

    # LIST latency — list the multi/ prefix
    local list_total_ms
    list_total_ms=0
    for i in $(seq 1 "$iterations"); do
        local start end
        start=$(date +%s%N)
        rclone lsf "scw:${S3_BUCKET}/${S3_PREFIX}/multi/" -q 2>/dev/null | wc -l > /dev/null
        end=$(date +%s%N)
        list_total_ms=$(( list_total_ms + (end - start) / 1000000 ))
    done
    local list_avg_ms=$(( list_total_ms / iterations ))

    log "  HEAD avg: ${head_avg_ms} ms (${iterations} iterations)"
    log "  LIST avg: ${list_avg_ms} ms (${iterations} iterations)"

    echo "{\"test\":\"metadata\",\"iterations\":${iterations},\"head_avg_ms\":${head_avg_ms},\"list_avg_ms\":${list_avg_ms}}"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    log "========================================"
    log "S3 Baseline Benchmark"
    log "========================================"
    log "Bucket:     ${S3_BUCKET}"
    log "Region:     ${S3_REGION}"
    log "Endpoint:   ${S3_ENDPOINT}"
    log "Small file: ${SMALL_FILE_COUNT} x ${SMALL_FILE_SIZE}"
    log "Large file: 1 x ${LARGE_FILE_SIZE}"
    log ""

    ensure_rclone_config
    cleanup_s3
    cleanup_local
    mkdir -p "${LOCAL_DIR}"

    local results=()

    # Run tests
    results+=("$(test_multi_put "$SMALL_FILE_COUNT" "$SMALL_FILE_SIZE")")
    cleanup_local
    results+=("$(test_multi_get "$SMALL_FILE_COUNT" "$SMALL_FILE_SIZE")")
    cleanup_local
    results+=("$(test_large_put "$LARGE_FILE_SIZE")")
    cleanup_local
    results+=("$(test_large_get "$LARGE_FILE_SIZE")")

    # For metadata test, re-upload multi files first (they were cleaned by large_put)
    cleanup_s3
    mkdir -p "${LOCAL_DIR}/upload"
    for i in $(seq 1 10); do
        generate_file "${LOCAL_DIR}/upload/file-${i}.bin" "$SMALL_FILE_SIZE"
    done
    rclone copy "${LOCAL_DIR}/upload/" "scw:${S3_BUCKET}/${S3_PREFIX}/multi/" --transfers 4 -q
    # Also upload the large file for HEAD test
    generate_file "${LOCAL_DIR}/bigfile.bin" "64M"
    rclone copyto "${LOCAL_DIR}/bigfile.bin" "scw:${S3_BUCKET}/${S3_PREFIX}/large/bigfile.bin" -q

    results+=("$(test_metadata 50)")

    # Write JSON results
    local json="["
    local first=true
    for r in "${results[@]}"; do
        if $first; then
            first=false
        else
            json+=","
        fi
        json+="$r"
    done
    json+="]"

    echo "$json" | python3 -m json.tool > "${RESULTS_FILE}"

    log ""
    log "========================================"
    log "Results saved to ${RESULTS_FILE}"
    log "========================================"

    # Print summary
    log ""
    log "SUMMARY:"
    for r in "${results[@]}"; do
        local test_name throughput
        test_name=$(echo "$r" | python3 -c "import sys,json; print(json.load(sys.stdin)['test'])")
        case "$test_name" in
            multi-put|multi-get|large-put|large-get)
                throughput=$(echo "$r" | python3 -c "import sys,json; print(json.load(sys.stdin)['throughput_mbps'])")
                log "  ${test_name}: ${throughput} MB/s"
                ;;
            metadata)
                head=$(echo "$r" | python3 -c "import sys,json; print(json.load(sys.stdin)['head_avg_ms'])")
                list=$(echo "$r" | python3 -c "import sys,json; print(json.load(sys.stdin)['list_avg_ms'])")
                log "  HEAD latency: ${head} ms avg"
                log "  LIST latency: ${list} ms avg"
                ;;
        esac
    done

    # Cleanup S3
    cleanup_s3
    cleanup_local
}

main
