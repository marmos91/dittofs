#!/usr/bin/env bash
# Parses pynfs testserver output and grades it against a KNOWN_FAILURES table.
#
# Mirrors test/posix/parse-results.sh and the SMB conformance harness: the
# blacklist of expected failures lives in a Markdown table loaded by the shared
# test/common/known-failures.sh parser. A run is green when every failing test
# is on the blacklist; only NEW failures fail CI.
#
# pynfs prints one line per test, formatted "%-8s %s" of (code, fullname)
# padded to 65 columns, then " : " and the outcome:
#
#   LOOK1    st_lookup.testDir                                     : FAILURE
#
# Outcomes are PASS, FAILURE, WARNING, UNSUPPORTED and OMIT. Only FAILURE is
# graded. WARNING and UNSUPPORTED mean the server declined an optional feature,
# which pynfs itself counts as "Warned", not "Failed"; OMIT means a dependency
# did not run. Failure detail follows on indented continuation lines, which is
# why lines are matched on the padded " : " form rather than by keyword.
#
# Only the final results block is graded — the run of lines between the last two
# rows of fifty asterisks that printresults() emits. This matters because pynfs
# -v prints every test in the *same* format as it runs, once on entry and once
# on completion, so scanning the whole file would count each failure three times
# and grade a passing run red.
#
# The blacklist is keyed on the test CODE, because that is what you pass back to
# testserver.py to re-run a single test. `pynfs-4.0 --showcodes` lists them.
#
# Exit codes:
#   0   All failures known (or no failures)
#   >0  Number of NEW unexpected failures
#   1   Missing or unparseable output (pynfs did not finish)
#
# Usage:
#   ./parse-results.sh <pynfs-output-log> <known-failures-file> [results-dir]

set -euo pipefail

OUTPUT_FILE="${1:-}"
KNOWN_FAILURES_FILE="${2:-}"
RESULTS_DIR="${3:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../../common/known-failures.sh
source "${SCRIPT_DIR}/../../common/known-failures.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'

if [[ -z "$OUTPUT_FILE" || ! -f "$OUTPUT_FILE" ]]; then
    echo "Usage: $(basename "$0") <pynfs-output-log> <known-failures-file> [results-dir]"
    echo "ERROR: output file not found: ${OUTPUT_FILE:-<unset>}"
    exit 1
fi

# pynfs always ends with this tally. Without it the run crashed, was killed, or
# never reached the server, and a truncated log must not grade green.
TALLY="$(grep -E '^Of those: [0-9]+ Skipped, [0-9]+ Failed' "$OUTPUT_FILE" | tail -1 || true)"
if [[ -z "$TALLY" ]]; then
    echo -e "${RED}ERROR: no 'Of those: ... Skipped, ... Failed' tally — pynfs did not finish.${NC}"
    echo "Last 30 lines:"; tail -30 "$OUTPUT_FILE"
    exit 1
fi
if grep -q '^Tests interrupted!' "$OUTPUT_FILE"; then
    echo -e "${RED}ERROR: pynfs reported 'Tests interrupted!' — partial run.${NC}"
    echo "$TALLY"
    exit 1
fi

kf_load "$KNOWN_FAILURES_FILE"
echo -e "${BOLD}Loaded ${KF_COUNT} known failures from $(basename "${KNOWN_FAILURES_FILE:-<none>}")${NC}"
echo "$TALLY"

# Isolate the final results block, then collect the code of every FAILURE line
# in it. Field 1 is the code; the outcome is the last field.
RESULTS_BLOCK="$(awk '
    /^\*{50}$/ { n++; starts[n] = NR }
    END { if (n >= 2) print starts[n-1] "," starts[n] }
' "$OUTPUT_FILE")"

if [[ -z "$RESULTS_BLOCK" ]]; then
    echo -e "${RED}ERROR: no results block — pynfs printed a tally but no per-test results.${NC}"
    echo "$TALLY"
    exit 1
fi

declare -a FAILING=()
while IFS= read -r code; do
    [[ -z "$code" ]] && continue
    FAILING+=("$code")
done < <(sed -n "${RESULTS_BLOCK}p" "$OUTPUT_FILE" \
         | awk '/ : FAILURE[[:space:]]*$/ { print $1 }')

KNOWN_HITS=0
NEW_FAILURES=0
declare -a NEW_FAILURE_LIST=()

echo ""
echo -e "${BOLD}--- Failing tests ---${NC}"
if [[ ${#FAILING[@]} -eq 0 ]]; then
    echo "  (none)"
fi
for code in "${FAILING[@]:-}"; do
    [[ -z "$code" ]] && continue
    if kf_is_known "$code"; then
        KNOWN_HITS=$((KNOWN_HITS + 1))
        printf "  ${YELLOW}%-16s KNOWN${NC} (%s)\n" "$code" "$(kf_reason "$code")"
    else
        NEW_FAILURES=$((NEW_FAILURES + 1))
        NEW_FAILURE_LIST+=("$code")
        printf "  ${RED}%-16s FAIL${NC}\n" "$code"
    fi
done

echo ""
echo -e "${BOLD}--- Summary ---${NC}"
echo -e "  Known failures: ${YELLOW}${KNOWN_HITS}${NC}"
echo -e "  New failures:   ${RED}${NEW_FAILURES}${NC}"
echo ""

if [[ -n "$RESULTS_DIR" && -d "$RESULTS_DIR" ]]; then
    {
        echo "| Metric | Count |"
        echo "|--------|-------|"
        echo "| Failing tests | ${#FAILING[@]} |"
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
    echo -e "${GREEN}${BOLD}RESULT: All failures known. CI green.${NC}"
fi

exit "$NEW_FAILURES"
