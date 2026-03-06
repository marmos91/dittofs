#!/usr/bin/env bash
set -euo pipefail

# analyze.sh — Main analysis pipeline for DittoFS benchmark results.
#
# Usage:
#   ./analyze.sh RESULTS_DIR [OPTIONS]
#
# Takes a results directory containing *.json benchmark files and:
#   1. Runs `dfsctl bench compare` on all files for a table view
#   2. Generates a CSV summary via json-to-csv.sh
#   3. Creates a Markdown report via report.sh
#
# Output:
#   <RESULTS_DIR>/summary.csv   — CSV with all metrics
#   <RESULTS_DIR>/report.md     — Full Markdown comparison report

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

usage() {
    cat >&2 <<EOF
Usage: $(basename "$0") RESULTS_DIR [OPTIONS]

Run the full analysis pipeline on benchmark result files.

Arguments:
  RESULTS_DIR   Directory containing *.json benchmark result files
                (output from dfsctl bench run --save)

Options:
  --baseline SYSTEM   System name to use as comparison baseline
                      (default: kernel-nfs, falls back to first system)
  --dfsctl PATH       Path to dfsctl binary (default: dfsctl in PATH)
  --skip-compare      Skip the dfsctl bench compare step
  -h, --help          Show this help message

Output:
  RESULTS_DIR/summary.csv    CSV summary of all results
  RESULTS_DIR/report.md      Markdown comparison report

Examples:
  $(basename "$0") bench/results/
  $(basename "$0") bench/results/ --baseline dittofs-badger-fs
  $(basename "$0") bench/results/ --dfsctl ./bin/dfsctl
EOF
    exit 1
}

# --- Dependency check ---
if ! command -v jq &>/dev/null; then
    echo "error: jq is required but not installed. Install it with: brew install jq (macOS) or apt install jq (Linux)" >&2
    exit 1
fi

# --- Argument parsing ---
RESULTS_DIR=""
BASELINE="kernel-nfs"
DFSCTL="dfsctl"
SKIP_COMPARE=false

while [[ $# -gt 0 ]]; do
    case "$1" in
        --baseline)
            BASELINE="$2"
            shift 2
            ;;
        --dfsctl)
            DFSCTL="$2"
            shift 2
            ;;
        --skip-compare)
            SKIP_COMPARE=true
            shift
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
    echo "error: RESULTS_DIR is required" >&2
    echo "" >&2
    usage
fi

if [[ ! -d "$RESULTS_DIR" ]]; then
    echo "error: not a directory: $RESULTS_DIR" >&2
    exit 1
fi

# Collect JSON files.
json_files=()
while IFS= read -r -d '' f; do
    json_files+=("$f")
done < <(find "$RESULTS_DIR" -maxdepth 1 -name '*.json' -print0 | sort -z)

if [[ ${#json_files[@]} -eq 0 ]]; then
    echo "error: no *.json files found in $RESULTS_DIR" >&2
    exit 1
fi

echo "=== DittoFS Benchmark Analysis Pipeline ==="
echo ""
echo "Results directory: $RESULTS_DIR"
echo "JSON files found:  ${#json_files[@]}"
echo "Baseline system:   $BASELINE"
echo ""

# --- Step 1: dfsctl bench compare (table view) ---
if [[ "$SKIP_COMPARE" == "false" ]]; then
    if command -v "$DFSCTL" &>/dev/null; then
        echo "--- Step 1: Comparison table (dfsctl bench compare) ---"
        echo ""
        "$DFSCTL" bench compare "${json_files[@]}" || {
            echo "warning: dfsctl bench compare failed (non-fatal, continuing)" >&2
        }
        echo ""
    else
        echo "--- Step 1: Skipped (dfsctl not found at '$DFSCTL') ---"
        echo "  Install dfsctl or use --dfsctl to specify the path."
        echo ""
    fi
else
    echo "--- Step 1: Skipped (--skip-compare) ---"
    echo ""
fi

# --- Step 2: CSV summary ---
echo "--- Step 2: Generating CSV summary ---"
csv_out="$RESULTS_DIR/summary.csv"

"$SCRIPT_DIR/json-to-csv.sh" "${json_files[@]}" > "$csv_out"
echo "  Written: $csv_out"
echo ""

# --- Step 3: Markdown report ---
echo "--- Step 3: Generating Markdown report ---"
report_out="$RESULTS_DIR/report.md"

"$SCRIPT_DIR/report.sh" "$RESULTS_DIR" --baseline "$BASELINE" > "$report_out"
echo "  Written: $report_out"
echo ""

# --- Summary ---
echo "=== Analysis complete ==="
echo ""
echo "Outputs:"
echo "  CSV:      $csv_out"
echo "  Report:   $report_out"
echo ""
echo "View the report:"
echo "  cat $report_out"
