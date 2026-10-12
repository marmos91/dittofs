#!/usr/bin/env bash
# Grade a cthon04 log written by run.sh against a KNOWN_FAILURES table.
#
# Usage:
#   ./parse-results.sh <cthon04.log> <known-failures-file> [results-dir]
#
# Reads the "CTHON04-RESULT <name> <verdict>" lines and refuses to grade a log
# whose "CTHON04-DONE <n>" marker is missing or disagrees with the number of
# results: a run cut short has already printed every result it got to, and
# grading those alone would call a hung server green.
#
# Exit status: as test/nfs-mount/grade.sh, or 1 when the log is not gradable.

set -euo pipefail

LOG="${1:-}"
KNOWN_FAILURES_FILE="${2:-}"
RESULTS_DIR="${3:-}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ungraded() {
    echo "ERROR: $*" >&2
    [[ -n "$RESULTS_DIR" && -d "$RESULTS_DIR" ]] && echo "ungraded 0 0 0" >"$RESULTS_DIR/verdict"
    exit 1
}

[[ -f "$LOG" ]] || ungraded "log not found: ${LOG:-<unset>}"

done_line="$(grep -E '^CTHON04-DONE [0-9]+$' "$LOG" | tail -1 || true)"
[[ -n "$done_line" ]] || ungraded "no CTHON04-DONE marker — the run did not finish"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

grep -E '^CTHON04-RESULT [^ ]+ (PASS|FAIL|TIMEOUT)$' "$LOG" |
    awk '{ print $3 "\t" $2 }' >"$WORK/results" || true

got="$(wc -l <"$WORK/results" | tr -d ' ')"
want="${done_line#CTHON04-DONE }"
[[ "$got" -eq "$want" ]] || ungraded "log has $got result(s) but the run reported $want"
[[ "$got" -gt 0 ]] || ungraded "the run reported no results"

"$SCRIPT_DIR/../grade.sh" "$WORK/results" "$KNOWN_FAILURES_FILE" "$RESULTS_DIR"
