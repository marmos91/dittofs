#!/usr/bin/env bash
# Parses WPTS TRX output and produces colored summary table
#
# Classifies test outcomes against KNOWN_FAILURES.md:
#   - PASS:  Test passed (green)
#   - KNOWN: Test failed but is in KNOWN_FAILURES.md (yellow)
#   - FAIL:  Test failed and is NOT in KNOWN_FAILURES.md (red)
#   - SKIP:  Test was not executed (dim)
#
# Exit codes:
#   0  All failures are known (or no failures)
#   >0 Number of new unexpected failures
#   1  Missing TRX file or no results
#
# Usage:
#   ./parse-results.sh <trx-file> [known-failures-file]

set -euo pipefail

# --------------------------------------------------------------------------
# Arguments
# --------------------------------------------------------------------------
TRX_FILE="${1:-}"
KNOWN_FAILURES_FILE="${2:-KNOWN_FAILURES.md}"
VERBOSE="${VERBOSE:-false}"

if [[ -z "$TRX_FILE" ]]; then
    echo "Usage: $(basename "$0") <trx-file> [known-failures-file]"
    exit 1
fi

if [[ ! -f "$TRX_FILE" ]]; then
    echo "ERROR: TRX file not found: ${TRX_FILE}"
    exit 1
fi

# --------------------------------------------------------------------------
# Check dependencies
# --------------------------------------------------------------------------
if ! command -v xmlstarlet >/dev/null 2>&1; then
    echo "ERROR: xmlstarlet is required but not installed."
    echo ""
    echo "Install with:"
    echo "  macOS:  brew install xmlstarlet"
    echo "  Ubuntu: sudo apt-get install -y xmlstarlet"
    echo "  Alpine: apk add xmlstarlet"
    exit 1
fi

# --------------------------------------------------------------------------
# Colors
# --------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
DIM='\033[2m'
BOLD='\033[1m'
NC='\033[0m'

# --------------------------------------------------------------------------
# TRX namespace
# --------------------------------------------------------------------------
NS="http://microsoft.com/schemas/VisualStudio/TeamTest/2010"

# --------------------------------------------------------------------------
# Extract summary counters
# --------------------------------------------------------------------------
TOTAL=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@total" "$TRX_FILE" 2>/dev/null || echo "0")
PASSED=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@passed" "$TRX_FILE" 2>/dev/null || echo "0")
FAILED=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@failed" "$TRX_FILE" 2>/dev/null || echo "0")
ERRORED=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@error" "$TRX_FILE" 2>/dev/null || echo "0")
TIMEOUT=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@timeout" "$TRX_FILE" 2>/dev/null || echo "0")
ABORTED=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@aborted" "$TRX_FILE" 2>/dev/null || echo "0")
NOT_EXECUTED=$(xmlstarlet sel -N t="$NS" -t -v "//t:Counters/@notExecuted" "$TRX_FILE" 2>/dev/null || echo "0")

if [[ "$TOTAL" -eq 0 ]]; then
    echo "WARNING: TRX file contains 0 tests. WPTS may not have run correctly."
    exit 1
fi

# --------------------------------------------------------------------------
# Load known failures from KNOWN_FAILURES.md
# --------------------------------------------------------------------------
declare -A KNOWN_FAILURES
declare -A KNOWN_REASONS
# Workaround: bash set -u treats empty associative arrays as unbound
KNOWN_FAILURES[_]="" ; unset 'KNOWN_FAILURES[_]'
KNOWN_REASONS[_]=""  ; unset 'KNOWN_REASONS[_]'

if [[ -f "$KNOWN_FAILURES_FILE" ]]; then
    while IFS= read -r line; do
        # Skip empty lines, comments, markdown headers
        [[ -z "$line" ]] && continue
        [[ "$line" == \#* ]] && continue

        # Only process lines that look like markdown table rows (start with |)
        [[ "$line" != \|* ]] && continue

        # Skip separator rows (e.g., |---|---|---|---|)
        [[ "$line" =~ ^\|[[:space:]]*-+ ]] && continue

        # Split on | — table rows start with |, so field 1 is empty, field 2 is the test name
        IFS='|' read -r _ name _ reason _ <<< "$line"

        # Trim whitespace from name
        name="${name#"${name%%[![:space:]]*}"}"
        name="${name%"${name##*[![:space:]]}"}"

        # Skip the header row
        [[ "$name" == "Test Name" ]] && continue
        [[ -z "$name" ]] && continue

        # Trim whitespace from reason
        reason="${reason#"${reason%%[![:space:]]*}"}"
        reason="${reason%"${reason##*[![:space:]]}"}"

        KNOWN_FAILURES["$name"]=1
        KNOWN_REASONS["$name"]="${reason:-unknown}"
    done < "$KNOWN_FAILURES_FILE"
fi

KNOWN_COUNT=${#KNOWN_FAILURES[@]}

# --------------------------------------------------------------------------
# Print header
# --------------------------------------------------------------------------
echo ""
echo -e "${BOLD}=== SMB Conformance Results ===${NC}"
echo ""
echo -e "  Total:     ${BOLD}${TOTAL}${NC}"
echo -e "  Passed:    ${GREEN}${PASSED}${NC}"
echo -e "  Failed:    ${RED}${FAILED}${NC}"
echo -e "  Errors:    ${RED}${ERRORED}${NC}"
echo -e "  Timeouts:  ${RED}${TIMEOUT}${NC}"
echo -e "  Aborted:   ${RED}${ABORTED}${NC}"
echo -e "  Skipped:   ${DIM}${NOT_EXECUTED}${NC}"
echo -e "  Known:     ${YELLOW}${KNOWN_COUNT} tracked${NC}"
echo ""

# --------------------------------------------------------------------------
# Extract per-test results and classify
# --------------------------------------------------------------------------
NEW_FAILURES=0
KNOWN_HITS=0
PASS_COUNT=0
SKIP_COUNT=0

# Extract testName|outcome pairs
RESULTS=$(xmlstarlet sel -N t="$NS" -t -m "//t:UnitTestResult" \
    -v "@testName" -o "|" -v "@outcome" -n "$TRX_FILE" 2>/dev/null || true)

if [[ -z "$RESULTS" ]]; then
    echo "WARNING: Could not extract individual test results from TRX."
    echo "The TRX file may use an unexpected format."
    exit 1
fi

# Print per-test table header
printf "%-80s %s\n" "Test Name" "Status"
printf "%-80s %s\n" "$(printf '%0.s-' {1..78})" "------"

while IFS='|' read -r test_name outcome; do
    [[ -z "$test_name" ]] && continue

    # Truncate long test names for display
    local_display="$test_name"
    if [[ ${#local_display} -gt 78 ]]; then
        local_display="${local_display:0:75}..."
    fi

    case "$outcome" in
        Passed)
            printf "  ${GREEN}%-78s PASS${NC}\n" "$local_display"
            PASS_COUNT=$((PASS_COUNT + 1))
            ;;
        Failed|Error)
            if [[ -n "${KNOWN_FAILURES[$test_name]+_}" ]]; then
                if [[ "$VERBOSE" == "true" ]]; then
                    printf "  ${YELLOW}%-78s KNOWN (%s)${NC}\n" "$local_display" "${KNOWN_REASONS[$test_name]}"
                else
                    printf "  ${YELLOW}%-78s KNOWN${NC}\n" "$local_display"
                fi
                KNOWN_HITS=$((KNOWN_HITS + 1))
            else
                printf "  ${RED}%-78s FAIL${NC}\n" "$local_display"
                NEW_FAILURES=$((NEW_FAILURES + 1))
            fi
            ;;
        NotExecuted)
            if [[ "$VERBOSE" == "true" ]]; then
                printf "  ${DIM}%-78s SKIP${NC}\n" "$local_display"
            fi
            SKIP_COUNT=$((SKIP_COUNT + 1))
            ;;
        *)
            printf "  ${DIM}%-78s %s${NC}\n" "$local_display" "$outcome"
            SKIP_COUNT=$((SKIP_COUNT + 1))
            ;;
    esac
done <<< "$RESULTS"

# --------------------------------------------------------------------------
# Summary
# --------------------------------------------------------------------------
echo ""
echo -e "${BOLD}--- Summary ---${NC}"
echo -e "  Passed:           ${GREEN}${PASS_COUNT}${NC}"
echo -e "  Known failures:   ${YELLOW}${KNOWN_HITS}${NC}"
echo -e "  New failures:     ${RED}${NEW_FAILURES}${NC}"
echo -e "  Skipped:          ${DIM}${SKIP_COUNT}${NC}"
echo ""

if [[ "$NEW_FAILURES" -gt 0 ]]; then
    echo -e "${RED}${BOLD}RESULT: ${NEW_FAILURES} new failure(s) detected!${NC}"
    echo ""
    echo "New failures not in KNOWN_FAILURES.md:"
    while IFS='|' read -r test_name outcome; do
        [[ -z "$test_name" ]] && continue
        case "$outcome" in
            Failed|Error)
                if [[ -z "${KNOWN_FAILURES[$test_name]+_}" ]]; then
                    echo "  - ${test_name}"
                fi
                ;;
        esac
    done <<< "$RESULTS"
    echo ""
    echo "To add as known failures, append to KNOWN_FAILURES.md:"
    echo "  | <test-name> | <category> | <reason> | #<issue> |"
    echo ""
else
    echo -e "${GREEN}${BOLD}RESULT: All failures are known. CI green.${NC}"
    echo ""
fi

# Exit with count of new failures (0 = success)
exit "$NEW_FAILURES"
