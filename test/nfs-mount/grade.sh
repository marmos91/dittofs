#!/usr/bin/env bash
# Grade normalised test results against a KNOWN_FAILURES table. Shared by the
# cthon04 and nfstest parsers, which each turn their suite's log into the
# input format below and leave the verdict to this.
#
# Usage:
#   ./grade.sh <results> <known-failures-tables> [results-dir]
#
# <known-failures-tables> is one table or several, colon-separated, graded
# together: a shared table plus a per-variant overlay.
#
# <results> holds one line per test, "<VERDICT><TAB><name>", where VERDICT is
# PASS, FAIL, TIMEOUT or INCOMPLETE. TIMEOUT and INCOMPLETE grade as failures
# that no blacklist row can excuse: a test that never finished, or a result
# the parser could not account for, is not a pass whatever its name.
#
# Exit status: the number of failures not on the blacklist, capped at 254 so an
# eight-bit status cannot wrap a large count to success. When a results
# directory is given, a "verdict" sidecar records the count for
# test/conformance/run.sh, which cannot tell a failure count from a crash by
# the exit status alone.

set -euo pipefail

RESULTS="${1:-}"
KNOWN_FAILURES_FILE="${2:-}"
RESULTS_DIR="${3:-}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../common/known-failures.sh
source "${SCRIPT_DIR}/../common/known-failures.sh"

RED='\033[0;31m'; GREEN='\033[0;32m'; YELLOW='\033[1;33m'; BOLD='\033[1m'; NC='\033[0m'

[[ -f "$RESULTS" ]] || { echo "usage: $(basename "$0") <results> <known-failures> [results-dir]" >&2; exit 1; }

IFS=':' read -ra TABLES <<<"$KNOWN_FAILURES_FILE"
for table in "${TABLES[@]}"; do
    [[ -f "$table" ]] || { echo "known-failure table not found: $table" >&2; exit 1; }
    kf_load "$table"
done
echo "Loaded ${KF_COUNT} known failure pattern(s) from ${#TABLES[@]} table(s)"

PASSED=0 KNOWN=0 NEW=0
declare -a NEW_LIST=() STALE_LIST=()

echo ""
echo -e "${BOLD}--- Failures ---${NC}"
while IFS=$'\t' read -r verdict name; do
    [[ -z "$name" ]] && continue
    case "$verdict" in
    PASS)
        PASSED=$((PASSED + 1))
        # A blacklisted test that passes is worth knowing about — its entry now
        # hides nothing but a future regression. Reported, never failed: the
        # entry may cover a flaky test, and removing it is a human call.
        kf_is_known "$name" && STALE_LIST+=("$name")
        ;;
    FAIL | TIMEOUT | INCOMPLETE)
        if [[ "$verdict" == FAIL ]] && kf_is_known "$name"; then
            KNOWN=$((KNOWN + 1))
            printf "  ${YELLOW}KNOWN${NC} %s (%s)\n" "$name" "$(kf_reason "$name")"
        else
            NEW=$((NEW + 1))
            NEW_LIST+=("$name")
            printf "  ${RED}%-10s${NC} %s\n" "$verdict" "$name"
        fi
        ;;
    *)
        echo "unrecognised verdict '$verdict' for $name" >&2
        exit 1
        ;;
    esac
done <"$RESULTS"
[[ $((KNOWN + NEW)) -eq 0 ]] && echo "  (none)"

if [[ ${#STALE_LIST[@]} -gt 0 ]]; then
    echo ""
    echo -e "${BOLD}--- Blacklisted but passing (consider removing) ---${NC}"
    printf '  %s\n' "${STALE_LIST[@]}"
fi

echo ""
echo -e "${BOLD}--- Summary ---${NC}"
echo -e "  Passed:         ${GREEN}${PASSED}${NC}"
echo -e "  Known failures: ${YELLOW}${KNOWN}${NC}"
echo -e "  New failures:   ${RED}${NEW}${NC}"
echo ""

if [[ -n "$RESULTS_DIR" && -d "$RESULTS_DIR" ]]; then
    {
        echo "| Metric | Count |"
        echo "|--------|-------|"
        echo "| Passed | ${PASSED} |"
        echo "| Known | ${KNOWN} |"
        echo "| New Failures | ${NEW} |"
    } >"${RESULTS_DIR}/summary.txt"
    echo "graded ${NEW} 0 0" >"${RESULTS_DIR}/verdict"
fi

if [[ "$NEW" -gt 0 ]]; then
    echo -e "${RED}${BOLD}RESULT: ${NEW} new failure(s) detected!${NC}"
    echo "If expected, add it to the table that fits, shared or per version:"
    echo "  | ${NEW_LIST[0]} | <category> | <reason> | <issue> |"
else
    echo -e "${GREEN}${BOLD}RESULT: All failures are known. CI green.${NC}"
fi

exit "$((NEW > 254 ? 254 : NEW))"
