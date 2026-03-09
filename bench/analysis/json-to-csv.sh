#!/usr/bin/env bash
set -euo pipefail

# json-to-csv.sh — Convert DittoFS benchmark JSON result files to CSV.
#
# Usage:
#   ./json-to-csv.sh FILE [FILE...]
#   ./json-to-csv.sh results/*.json > summary.csv
#
# Input:  One or more JSON result files produced by `dfsctl bench run --save`.
# Output: CSV on stdout with one row per system+workload combination.
#
# Columns:
#   system, workload, throughput_mbps, iops, ops_per_sec,
#   p50_us, p95_us, p99_us, total_bytes, total_ops, duration_sec

usage() {
    cat >&2 <<EOF
Usage: $(basename "$0") FILE [FILE...]

Convert benchmark JSON result files to CSV format.

Arguments:
  FILE    One or more JSON result files from dfsctl bench run --save

Output (stdout):
  CSV with columns: system, workload, throughput_mbps, iops, ops_per_sec,
  p50_us, p95_us, p99_us, total_bytes, total_ops, duration_sec

Examples:
  $(basename "$0") results/dittofs.json results/kernel-nfs.json
  $(basename "$0") results/*.json > summary.csv
EOF
    exit 1
}

# --- Dependency check ---
if ! command -v jq &>/dev/null; then
    echo "error: jq is required but not installed. Install it with: brew install jq (macOS) or apt install jq (Linux)" >&2
    exit 1
fi

# --- Argument validation ---
if [[ $# -lt 1 ]]; then
    usage
fi

# Validate that all input files exist and are readable.
for f in "$@"; do
    if [[ ! -r "$f" ]]; then
        echo "error: cannot read file: $f" >&2
        exit 1
    fi
done

# --- Header ---
echo "system,workload,throughput_mbps,iops,ops_per_sec,p50_us,p95_us,p99_us,total_bytes,total_ops,duration_sec"

# --- Rows ---
# For each JSON file, extract the system label and iterate over every workload
# entry, emitting one CSV row per workload.
for f in "$@"; do
    jq -r '
        .system as $sys |
        .workloads | to_entries[] |
        [
            $sys,
            .value.workload,
            (.value.throughput_mbps // 0 | tostring),
            (.value.iops // 0 | tostring),
            (.value.ops_per_sec // 0 | tostring),
            (.value.latency_p50_us // 0 | tostring),
            (.value.latency_p95_us // 0 | tostring),
            (.value.latency_p99_us // 0 | tostring),
            (.value.total_bytes // 0 | tostring),
            (.value.total_ops // 0 | tostring),
            (.value.duration // 0 | if type == "number" then . / 1000000000 else 0 end | tostring)
        ] | @csv
    ' "$f"
done
