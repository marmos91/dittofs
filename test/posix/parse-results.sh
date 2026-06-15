#!/usr/bin/env bash
# Parses pjdfstest `prove` output and grades it against a KNOWN_FAILURES table.
#
# Mirrors the SMB conformance harness (test/smb-conformance/.../parse-results.sh):
# the blacklist of expected failures lives in a Markdown table and is loaded by
# the shared test/common/known-failures.sh parser. A run is green when every
# failing test file is on the blacklist; only NEW failures fail CI.
#
# Classification (per pjdfstest .t file):
#   - PASS:  file passed (all assertions ok)
#   - KNOWN: file failed but is in KNOWN_FAILURES.md (yellow)
#   - FAIL:  file failed and is NOT in KNOWN_FAILURES.md (red)
#
# Exit codes:
#   0  All failures are known (or no failures)
#   >0 Number of NEW unexpected failing test files
#   1  Missing/unparseable output (no Test Summary, pjdfstest didn't run)
#
# Usage:
#   ./parse-results.sh <prove-output-log> <known-failures-file> [results-dir]

set -euo pipefail

OUTPUT_FILE="${1:-}"
KNOWN_FAILURES_FILE="${2:-}"
RESULTS_DIR="${3:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../common/known-failures.sh
source "${SCRIPT_DIR}/../common/known-failures.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'

if [[ -z "$OUTPUT_FILE" || ! -f "$OUTPUT_FILE" ]]; then
    echo "Usage: $(basename "$0") <prove-output-log> <known-failures-file> [results-dir]"
    echo "ERROR: output file not found: ${OUTPUT_FILE:-<unset>}"
    exit 1
fi

if grep -q "could not find pjdfstest" "$OUTPUT_FILE"; then
    echo -e "${RED}ERROR: pjdfstest not found — tests did not run.${NC}"
    exit 1
fi

kf_load "$KNOWN_FAILURES_FILE"
echo "Loaded ${KF_COUNT} known failure pattern(s) from $(basename "${KNOWN_FAILURES_FILE:-<none>}")"

# --------------------------------------------------------------------------
# Extract failing test files from the prove "Test Summary Report".
#
# prove emits, for every file with at least one failed assertion, a line like:
#   /nix/store/.../tests/utimensat/09.t   (Wstat: 0 Tests: 7 Failed: 1)
# We pull the path after tests/ and the Failed: count, and only treat
# Failed: >= 1 as a failure (Failed: 0 with a non-zero Wstat is a dubious
# exit but no failed assertion — still surfaced, see below).
# --------------------------------------------------------------------------
if ! grep -q "Test Summary Report" "$OUTPUT_FILE" && grep -q "All tests successful" "$OUTPUT_FILE"; then
    echo -e "${GREEN}${BOLD}RESULT: All tests successful. CI green.${NC}"
    [[ -n "$RESULTS_DIR" && -d "$RESULTS_DIR" ]] && {
        printf '| Metric | Count |\n|--------|-------|\n| Failed | 0 |\n| Known | 0 |\n| New Failures | 0 |\n' > "${RESULTS_DIR}/summary.txt"
    }
    exit 0
fi

SUMMARY_BLOCK="$(sed -n '/Test Summary Report/,/^Files=/p' "$OUTPUT_FILE")"
if [[ -z "$SUMMARY_BLOCK" ]]; then
    echo -e "${RED}ERROR: no 'Test Summary Report' and no 'All tests successful' marker — output not recognized.${NC}"
    echo "Last 30 lines:"; tail -30 "$OUTPUT_FILE"
    exit 1
fi

# tests_rel_path LINE — echo the pjdfstest path relative to tests/ for a
# summary line (".../tests/utimensat/09.t   (Wstat...)" -> "utimensat/09.t"),
# or nothing if the line carries no tests/ path. POSIX grep/sed only (the
# harness must run on the BSD grep of macOS dev boxes as well as Linux CI).
tests_rel_path() {
    printf '%s\n' "$1" \
        | grep -oE '/tests/[^ ]+\.t' \
        | sed -E 's#.*/tests/##' \
        | head -1
}

# A failing file is one whose summary line reports "Failed: <n>" with n>=1, OR
# a dubious exit ("Wstat: <nonzero> ... Failed: 0") — a crash with no graded
# assertion, which must still be surfaced and graded, never silently ignored.
declare -a FAILING_FILES=()
while IFS= read -r line; do
    case "$line" in
        *Failed:\ [1-9]*) ;;                       # >=1 failed assertion
        *Wstat:\ [1-9]*Failed:\ 0*) ;;             # dubious exit, no assertion
        *) continue ;;
    esac
    rel="$(tests_rel_path "$line")"
    [[ -z "$rel" ]] && continue
    dup=false
    for existing in "${FAILING_FILES[@]:-}"; do
        [[ "$existing" == "$rel" ]] && { dup=true; break; }
    done
    $dup || FAILING_FILES+=("$rel")
done < <(printf '%s\n' "$SUMMARY_BLOCK")

# --------------------------------------------------------------------------
# Grade.
# --------------------------------------------------------------------------
KNOWN_HITS=0
NEW_FAILURES=0
declare -a NEW_FAILURE_LIST=()

echo ""
echo -e "${BOLD}--- Failing test files ---${NC}"
if [[ ${#FAILING_FILES[@]} -eq 0 ]]; then
    echo "  (none)"
fi
for ft in "${FAILING_FILES[@]:-}"; do
    [[ -z "$ft" ]] && continue
    if kf_is_known "$ft"; then
        KNOWN_HITS=$((KNOWN_HITS + 1))
        printf "  ${YELLOW}%-24s KNOWN${NC} (%s)\n" "$ft" "$(kf_reason "$ft")"
    else
        NEW_FAILURES=$((NEW_FAILURES + 1))
        NEW_FAILURE_LIST+=("$ft")
        printf "  ${RED}%-24s FAIL${NC}\n" "$ft"
    fi
done

echo ""
echo -e "${BOLD}--- Summary ---${NC}"
echo -e "  Known failures:   ${YELLOW}${KNOWN_HITS}${NC}"
echo -e "  New failures:     ${RED}${NEW_FAILURES}${NC}"
echo ""

if [[ -n "$RESULTS_DIR" && -d "$RESULTS_DIR" ]]; then
    {
        echo "| Metric | Count |"
        echo "|--------|-------|"
        echo "| Failing files | ${#FAILING_FILES[@]} |"
        echo "| Known | ${KNOWN_HITS} |"
        echo "| New Failures | ${NEW_FAILURES} |"
    } > "${RESULTS_DIR}/summary.txt"
fi

if [[ "$NEW_FAILURES" -gt 0 ]]; then
    echo -e "${RED}${BOLD}RESULT: ${NEW_FAILURES} new failure(s) detected!${NC}"
    echo "New failures not in $(basename "${KNOWN_FAILURES_FILE:-KNOWN_FAILURES.md}"):"
    for name in "${NEW_FAILURE_LIST[@]}"; do
        echo "  - ${name}"
    done
    echo ""
    echo "If expected, append to the KNOWN_FAILURES table:"
    echo "  | ${NEW_FAILURE_LIST[0]} | <category> | <reason> | <issue> |"
else
    echo -e "${GREEN}${BOLD}RESULT: All failures are known. CI green.${NC}"
fi

exit "$NEW_FAILURES"
