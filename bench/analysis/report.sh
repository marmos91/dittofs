#!/usr/bin/env bash
set -euo pipefail

# report.sh — Generate a Markdown comparison report from benchmark JSON results.
#
# Usage:
#   ./report.sh RESULTS_DIR
#   ./report.sh results/ > report.md
#
# Input:  Directory containing *.json result files from `dfsctl bench run --save`.
# Output: Markdown report on stdout.
#
# Features:
#   - Config summary (threads, file size, duration, etc.)
#   - Per-workload comparison tables with all systems side-by-side
#   - Best-performer highlighting per metric (bold)
#   - Percentage comparison relative to baseline (kernel-nfs, or first system)

usage() {
    cat >&2 <<EOF
Usage: $(basename "$0") RESULTS_DIR

Generate a Markdown benchmark comparison report.

Arguments:
  RESULTS_DIR   Directory containing *.json benchmark result files

Output (stdout):
  Markdown report with comparison tables and analysis

Options:
  --baseline SYSTEM   System name to use as comparison baseline
                      (default: kernel-nfs, falls back to first system)

Examples:
  $(basename "$0") results/
  $(basename "$0") results/ --baseline dittofs-badger-fs > report.md
EOF
    exit 1
}

# --- Dependency check ---
if ! command -v jq &>/dev/null; then
    echo "error: jq is required but not installed. Install it with: brew install jq (macOS) or apt install jq (Linux)" >&2
    exit 1
fi

# --- Argument parsing ---
BASELINE="kernel-nfs"
RESULTS_DIR=""

while [[ $# -gt 0 ]]; do
    case "$1" in
        --baseline)
            BASELINE="$2"
            shift 2
            ;;
        -h|--help)
            usage
            ;;
        -*)
            echo "error: unknown option: $1" >&2
            usage
            ;;
        *)
            if [[ -z "$RESULTS_DIR" ]]; then
                RESULTS_DIR="$1"
            else
                echo "error: unexpected argument: $1" >&2
                usage
            fi
            shift
            ;;
    esac
done

if [[ -z "$RESULTS_DIR" ]]; then
    usage
fi

if [[ ! -d "$RESULTS_DIR" ]]; then
    echo "error: not a directory: $RESULTS_DIR" >&2
    exit 1
fi

# Collect JSON files, sorted by name for deterministic output.
json_files=()
while IFS= read -r -d '' f; do
    json_files+=("$f")
done < <(find "$RESULTS_DIR" -maxdepth 1 -name '*.json' -print0 | sort -z)

if [[ ${#json_files[@]} -eq 0 ]]; then
    echo "error: no *.json files found in $RESULTS_DIR" >&2
    exit 1
fi

# --- Collect system names ---
systems=()
for f in "${json_files[@]}"; do
    sys=$(jq -r '.system // "unknown"' "$f")
    systems+=("$sys")
done

# Determine baseline system index. Prefer the --baseline value if present,
# otherwise fall back to the first system.
baseline_idx=0
for i in "${!systems[@]}"; do
    if [[ "${systems[$i]}" == "$BASELINE" ]]; then
        baseline_idx=$i
        break
    fi
done
baseline_sys="${systems[$baseline_idx]}"

# --- Helper: format a number with appropriate precision ---
fmt_num() {
    local val="$1"
    if [[ "$val" == "0" ]] || [[ "$val" == "null" ]]; then
        echo "-"
        return
    fi
    # Use awk for cross-platform float formatting.
    awk "BEGIN { v=$val; if (v >= 1000) printf \"%.0f\", v; else if (v >= 10) printf \"%.1f\", v; else printf \"%.2f\", v }"
}

# --- Helper: format latency (microseconds) ---
fmt_latency() {
    local us="$1"
    if [[ "$us" == "0" ]] || [[ "$us" == "null" ]]; then
        echo "-"
        return
    fi
    awk "BEGIN { us=$us; if (us >= 1000) printf \"%.1f ms\", us/1000; else printf \"%.0f us\", us }"
}

# --- Helper: calculate percentage difference ---
pct_diff() {
    local baseline="$1"
    local value="$2"
    if [[ "$baseline" == "0" ]] || [[ "$value" == "0" ]] || [[ "$baseline" == "null" ]] || [[ "$value" == "null" ]]; then
        echo ""
        return
    fi
    awk "BEGIN { b=$baseline; v=$value; diff=((v-b)/b)*100; if (diff >= 0) printf \"+%.1f%%\", diff; else printf \"%.1f%%\", diff }"
}

# --- Helper: find best value index (highest = best for throughput/iops/ops, lowest = best for latency) ---
# Outputs the 0-based index of the best performer.
best_idx_max() {
    # $@ = values (space-separated)
    local values=("$@")
    local best_i=0
    local best_v=0
    for i in "${!values[@]}"; do
        if awk "BEGIN { exit !(${values[$i]} > $best_v) }"; then
            best_v="${values[$i]}"
            best_i=$i
        fi
    done
    echo "$best_i"
}

best_idx_min() {
    # $@ = values (space-separated), find minimum > 0
    local values=("$@")
    local best_i=-1
    local best_v=999999999
    for i in "${!values[@]}"; do
        local v="${values[$i]}"
        if [[ "$v" != "0" ]] && [[ "$v" != "null" ]] && awk "BEGIN { exit !($v < $best_v && $v > 0) }"; then
            best_v="$v"
            best_i=$i
        fi
    done
    if [[ "$best_i" == "-1" ]]; then
        echo "0"
    else
        echo "$best_i"
    fi
}

# ============================================================
# Report generation
# ============================================================

echo "# Benchmark Comparison Report"
echo ""
echo "_Generated: $(date -u '+%Y-%m-%d %H:%M:%S UTC')_"
echo ""

# --- Config summary ---
echo "## Configuration"
echo ""

# Use the first file for config info (all runs should share the same config).
config_file="${json_files[0]}"
threads=$(jq -r '.config.threads' "$config_file")
file_size=$(jq -r '.config.file_size' "$config_file")
block_size=$(jq -r '.config.block_size' "$config_file")
duration_ns=$(jq -r '.config.duration' "$config_file")
meta_files=$(jq -r '.config.meta_files' "$config_file")

# Convert sizes to human-readable.
file_size_hr=$(awk "BEGIN { s=$file_size; if (s >= 1073741824) printf \"%.0f GiB\", s/1073741824; else if (s >= 1048576) printf \"%.0f MiB\", s/1048576; else if (s >= 1024) printf \"%.0f KiB\", s/1024; else printf \"%d B\", s }")
block_size_hr=$(awk "BEGIN { s=$block_size; if (s >= 1048576) printf \"%.0f MiB\", s/1048576; else if (s >= 1024) printf \"%.0f KiB\", s/1024; else printf \"%d B\", s }")
duration_sec=$(awk "BEGIN { printf \"%.0f\", $duration_ns / 1000000000 }")

echo "| Parameter | Value |"
echo "|-----------|-------|"
echo "| Threads | $threads |"
echo "| File Size | $file_size_hr |"
echo "| Block Size | $block_size_hr |"
echo "| Duration | ${duration_sec}s |"
echo "| Metadata Files | $meta_files |"
echo "| Systems Tested | ${#systems[@]} |"
echo "| Baseline | $baseline_sys |"
echo ""

echo "### Systems"
echo ""
for i in "${!systems[@]}"; do
    ts=$(jq -r '.timestamp' "${json_files[$i]}")
    path=$(jq -r '.path' "${json_files[$i]}")
    echo "- **${systems[$i]}** -- path: \`$path\`, timestamp: $ts"
done
echo ""

# --- Workload tables ---
# Canonical workload order matching bench.AllWorkloads().
workloads=("seq-write" "seq-read" "rand-write" "rand-read" "metadata")
workload_labels=("Sequential Write" "Sequential Read" "Random Write" "Random Read" "Metadata")

for wi in "${!workloads[@]}"; do
    wl="${workloads[$wi]}"
    wl_label="${workload_labels[$wi]}"

    # Check if any system has data for this workload.
    has_data=false
    for f in "${json_files[@]}"; do
        if jq -e ".workloads[\"$wl\"]" "$f" &>/dev/null; then
            has_data=true
            break
        fi
    done
    if [[ "$has_data" == "false" ]]; then
        continue
    fi

    echo "## $wl_label (\`$wl\`)"
    echo ""

    # Determine which metrics are relevant for this workload type.
    # Sequential: throughput is primary. Random: IOPS is primary. Metadata: ops/sec is primary.
    # Latency is always relevant.
    case "$wl" in
        seq-write|seq-read)
            metrics=("throughput_mbps" "latency_p50_us" "latency_p95_us" "latency_p99_us" "total_bytes")
            metric_labels=("Throughput (MB/s)" "P50 Latency" "P95 Latency" "P99 Latency" "Total Bytes")
            metric_units=("higher" "lower" "lower" "lower" "higher")
            ;;
        rand-write|rand-read)
            metrics=("iops" "latency_p50_us" "latency_p95_us" "latency_p99_us" "total_ops")
            metric_labels=("IOPS" "P50 Latency" "P95 Latency" "P99 Latency" "Total Ops")
            metric_units=("higher" "lower" "lower" "lower" "higher")
            ;;
        metadata)
            metrics=("ops_per_sec" "latency_p50_us" "latency_p95_us" "latency_p99_us" "total_ops")
            metric_labels=("Ops/sec" "P50 Latency" "P95 Latency" "P99 Latency" "Total Ops")
            metric_units=("higher" "lower" "lower" "lower" "higher")
            ;;
    esac

    # Build the table header.
    header="| Metric |"
    separator="|--------|"
    for sys in "${systems[@]}"; do
        header="$header $sys |"
        separator="$separator--------|"
    done
    # Add a column for % vs baseline (only when more than 1 system).
    if [[ ${#systems[@]} -gt 1 ]]; then
        header="$header vs $baseline_sys |"
        separator="$separator--------|"
    fi
    echo "$header"
    echo "$separator"

    # Build each metric row.
    for mi in "${!metrics[@]}"; do
        metric="${metrics[$mi]}"
        label="${metric_labels[$mi]}"
        direction="${metric_units[$mi]}"

        # Collect raw values for all systems.
        raw_values=()
        for f in "${json_files[@]}"; do
            val=$(jq -r ".workloads[\"$wl\"].$metric // 0" "$f")
            raw_values+=("$val")
        done

        # Find best performer.
        if [[ "$direction" == "higher" ]]; then
            best=$(best_idx_max "${raw_values[@]}")
        else
            best=$(best_idx_min "${raw_values[@]}")
        fi

        # Format the row.
        row="| $label |"
        for i in "${!raw_values[@]}"; do
            val="${raw_values[$i]}"
            # Format based on metric type.
            case "$metric" in
                throughput_mbps)
                    formatted=$(fmt_num "$val")
                    if [[ "$formatted" != "-" ]]; then formatted="${formatted} MB/s"; fi
                    ;;
                iops|ops_per_sec|total_ops)
                    formatted=$(fmt_num "$val")
                    ;;
                total_bytes)
                    if [[ "$val" == "0" ]] || [[ "$val" == "null" ]]; then
                        formatted="-"
                    else
                        formatted=$(awk "BEGIN { b=$val; if (b >= 1073741824) printf \"%.1f GiB\", b/1073741824; else if (b >= 1048576) printf \"%.1f MiB\", b/1048576; else if (b >= 1024) printf \"%.1f KiB\", b/1024; else printf \"%d B\", b }")
                    fi
                    ;;
                latency_p50_us|latency_p95_us|latency_p99_us)
                    formatted=$(fmt_latency "$val")
                    ;;
                *)
                    formatted=$(fmt_num "$val")
                    ;;
            esac

            # Bold the best performer.
            if [[ "$i" == "$best" ]] && [[ "$formatted" != "-" ]] && [[ ${#systems[@]} -gt 1 ]]; then
                formatted="**$formatted**"
            fi
            row="$row $formatted |"
        done

        # Percentage vs baseline column.
        if [[ ${#systems[@]} -gt 1 ]]; then
            baseline_val="${raw_values[$baseline_idx]}"
            pct_col=""
            # Show pct for each non-baseline system (use the last non-baseline for the column,
            # or better: show all). For simplicity, if there are exactly 2 systems, show the
            # non-baseline pct. For >2, skip the column detail and just use the table.
            # Actually, let's show the range of pcts.
            pct_parts=()
            for i in "${!raw_values[@]}"; do
                if [[ "$i" == "$baseline_idx" ]]; then
                    continue
                fi
                p=$(pct_diff "$baseline_val" "${raw_values[$i]}")
                if [[ -n "$p" ]]; then
                    if [[ ${#systems[@]} -gt 2 ]]; then
                        pct_parts+=("${systems[$i]}: $p")
                    else
                        pct_parts+=("$p")
                    fi
                fi
            done
            if [[ ${#pct_parts[@]} -gt 0 ]]; then
                pct_col=$(IFS=', '; echo "${pct_parts[*]}")
            else
                pct_col="-"
            fi
            row="$row $pct_col |"
        fi

        echo "$row"
    done

    echo ""
done

# --- Summary / Winners ---
echo "## Summary"
echo ""
echo "Best performers per workload:"
echo ""

for wi in "${!workloads[@]}"; do
    wl="${workloads[$wi]}"
    wl_label="${workload_labels[$wi]}"

    # Determine primary metric for this workload.
    case "$wl" in
        seq-write|seq-read)   primary_metric="throughput_mbps" ;;
        rand-write|rand-read) primary_metric="iops" ;;
        metadata)             primary_metric="ops_per_sec" ;;
    esac

    raw_values=()
    has_any=false
    for f in "${json_files[@]}"; do
        val=$(jq -r ".workloads[\"$wl\"].$primary_metric // 0" "$f")
        raw_values+=("$val")
        if [[ "$val" != "0" ]] && [[ "$val" != "null" ]]; then
            has_any=true
        fi
    done

    if [[ "$has_any" == "false" ]]; then
        continue
    fi

    best=$(best_idx_max "${raw_values[@]}")
    best_sys="${systems[$best]}"
    best_val=$(fmt_num "${raw_values[$best]}")

    case "$primary_metric" in
        throughput_mbps) unit="MB/s" ;;
        iops)            unit="IOPS" ;;
        ops_per_sec)     unit="ops/sec" ;;
    esac

    echo "- **$wl_label**: $best_sys ($best_val $unit)"
done

echo ""
echo "---"
echo "_Baseline: $baseline_sys. Positive percentages = better than baseline (for throughput/IOPS/ops metrics); for latency, lower is better._"
