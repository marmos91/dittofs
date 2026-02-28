#!/usr/bin/env bash
# Parses smbtorture output and produces colored summary table
#
# Classifies test outcomes against KNOWN_FAILURES.md:
#   - PASS:  Test passed (green)
#   - KNOWN: Test failed but is in KNOWN_FAILURES.md (yellow)
#   - FAIL:  Test failed and is NOT in KNOWN_FAILURES.md (red)
#   - SKIP:  Test was skipped (dim)
#
# Exit codes:
#   0  All failures are known (or no failures)
#   >0 Number of new unexpected failures
#   1  Missing output file or no results
#
# Usage:
#   ./parse-results.sh <smbtorture-output-file> [known-failures-file] [results-dir]

set -euo pipefail

# --------------------------------------------------------------------------
# Arguments
# --------------------------------------------------------------------------
OUTPUT_FILE="${1:-}"
KNOWN_FAILURES_FILE="${2:-KNOWN_FAILURES.md}"
RESULTS_DIR="${3:-}"
VERBOSE="${VERBOSE:-false}"

if [[ -z "$OUTPUT_FILE" ]]; then
    echo "Usage: $(basename "$0") <smbtorture-output-file> [known-failures-file] [results-dir]"
    exit 1
fi

if [[ ! -f "$OUTPUT_FILE" ]]; then
    echo "ERROR: Output file not found: ${OUTPUT_FILE}"
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

        # Split on | -- field 1 is empty (leading |), field 2 is test name
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

        # Support wildcard patterns (e.g., smb2.durable-open.*)
        KNOWN_FAILURES["$name"]=1
        KNOWN_REASONS["$name"]="${reason:-unknown}"
    done < "$KNOWN_FAILURES_FILE"
fi

KNOWN_COUNT=${#KNOWN_FAILURES[@]}

# --------------------------------------------------------------------------
# Helper: check if test name matches any known failure (including wildcards)
# --------------------------------------------------------------------------
is_known_failure() {
    local test_name="$1"

    # Exact match
    if [[ -n "${KNOWN_FAILURES[$test_name]+_}" ]]; then
        return 0
    fi

    # Wildcard match: check patterns ending with .*
    for pattern in "${!KNOWN_FAILURES[@]}"; do
        if [[ "$pattern" == *'.*' ]]; then
            local prefix="${pattern%.\*}"
            if [[ "$test_name" == "$prefix"* ]]; then
                return 0
            fi
        fi
    done

    return 1
}

get_known_reason() {
    local test_name="$1"

    # Exact match
    if [[ -n "${KNOWN_REASONS[$test_name]+_}" ]]; then
        echo "${KNOWN_REASONS[$test_name]}"
        return
    fi

    # Wildcard match
    for pattern in "${!KNOWN_REASONS[@]}"; do
        if [[ "$pattern" == *'.*' ]]; then
            local prefix="${pattern%.\*}"
            if [[ "$test_name" == "$prefix"* ]]; then
                echo "${KNOWN_REASONS[$pattern]}"
                return
            fi
        fi
    done

    echo "unknown"
}

# --------------------------------------------------------------------------
# Parse smbtorture output
#
# smbtorture output varies by version but typically includes lines like:
#   success: smb2.connect.connect1
#   failure: smb2.lock.lock1 [...]
#   skip: smb2.multichannel.interface_info [...]
#   error: smb2.something [...]
#
# Alternative format (subunit-style):
#   smb2.connect.connect1           ok
#   smb2.lock.lock1                 FAILED
#   smb2.multichannel               SKIP
# --------------------------------------------------------------------------
PASS_COUNT=0
FAIL_COUNT=0
SKIP_COUNT=0
NEW_FAILURES=0
KNOWN_HITS=0
TOTAL=0

declare -a NEW_FAILURE_LIST=()
declare -a ALL_RESULTS=()

while IFS= read -r line; do
    test_name=""
    outcome=""

    # ---------- Keyword-prefixed format ----------
    # "success: test.name"
    # "failure: test.name [reason]"
    # "error: test.name [reason]"
    # "skip: test.name [reason]"
    #
    # Note: smbtorture may emit bare names (e.g. "failure: dosmode") without
    # the suite prefix. We normalize by prepending "smb2." when missing so
    # known-failure patterns like "smb2.dosmode.*" match correctly.
    if [[ "$line" =~ ^(success|failure|error|skip):[[:space:]]+(.*) ]]; then
        keyword="${BASH_REMATCH[1]}"
        test_name="${BASH_REMATCH[2]%% *}"  # Extract first token (test name)

        # Normalize: prepend smb2. if not already present
        if [[ "$test_name" != smb2.* ]]; then
            test_name="smb2.${test_name}"
        fi

        case "$keyword" in
            success) outcome="pass" ;;
            failure|error) outcome="fail" ;;
            skip) outcome="skip" ;;
        esac

    # ---------- Subunit-style format ----------
    # "  smb2.connect.connect1     ok"
    # "  smb2.lock.lock1           FAILED"
    # "  smb2.multichannel         SKIP"
    # "  dosmode                   FAILED"   (bare name without smb2. prefix)
    elif [[ "$line" =~ ^[[:space:]]*([a-zA-Z][^[:space:]]+)[[:space:]]+(ok|OK|FAILED|FAIL|SKIP|SKIPPED)[[:space:]]*$ ]]; then
        test_name="${BASH_REMATCH[1]}"

        # Normalize: prepend smb2. if not already present
        if [[ "$test_name" != smb2.* ]]; then
            test_name="smb2.${test_name}"
        fi

        status="${BASH_REMATCH[2]}"

        case "$status" in
            ok|OK) outcome="pass" ;;
            FAILED|FAIL) outcome="fail" ;;
            SKIP|SKIPPED) outcome="skip" ;;
        esac
    fi

    # Skip lines that don't match any format
    [[ -z "$test_name" ]] && continue

    TOTAL=$((TOTAL + 1))
    ALL_RESULTS+=("${test_name}|${outcome}")

    case "$outcome" in
        pass)
            PASS_COUNT=$((PASS_COUNT + 1))
            ;;
        fail)
            FAIL_COUNT=$((FAIL_COUNT + 1))
            if is_known_failure "$test_name"; then
                KNOWN_HITS=$((KNOWN_HITS + 1))
            else
                NEW_FAILURES=$((NEW_FAILURES + 1))
                NEW_FAILURE_LIST+=("$test_name")
            fi
            ;;
        skip)
            SKIP_COUNT=$((SKIP_COUNT + 1))
            ;;
    esac
done < "$OUTPUT_FILE"

# --------------------------------------------------------------------------
# Print header
# --------------------------------------------------------------------------
echo ""
echo -e "${BOLD}=== smbtorture Results ===${NC}"
echo ""
echo -e "  Total:     ${BOLD}${TOTAL}${NC}"
echo -e "  Passed:    ${GREEN}${PASS_COUNT}${NC}"
echo -e "  Failed:    ${RED}${FAIL_COUNT}${NC}"
echo -e "  Skipped:   ${DIM}${SKIP_COUNT}${NC}"
echo -e "  Known:     ${YELLOW}${KNOWN_COUNT} tracked${NC}"
echo ""

if [[ "$TOTAL" -eq 0 ]]; then
    echo "WARNING: No test results found in smbtorture output."
    echo "smbtorture may not have run correctly. Check the output file:"
    echo "  ${OUTPUT_FILE}"
    exit 1
fi

# --------------------------------------------------------------------------
# Print per-test table
# --------------------------------------------------------------------------
printf "%-70s %s\n" "Test Name" "Status"
printf "%-70s %s\n" "$(printf '%0.s-' {1..68})" "------"

for entry in "${ALL_RESULTS[@]}"; do
    IFS='|' read -r test_name outcome <<< "$entry"

    local_display="$test_name"
    if [[ ${#local_display} -gt 68 ]]; then
        local_display="${local_display:0:65}..."
    fi

    case "$outcome" in
        pass)
            printf "  ${GREEN}%-68s PASS${NC}\n" "$local_display"
            ;;
        fail)
            if is_known_failure "$test_name"; then
                if [[ "$VERBOSE" == "true" ]]; then
                    printf "  ${YELLOW}%-68s KNOWN (%s)${NC}\n" "$local_display" "$(get_known_reason "$test_name")"
                else
                    printf "  ${YELLOW}%-68s KNOWN${NC}\n" "$local_display"
                fi
            else
                printf "  ${RED}%-68s FAIL${NC}\n" "$local_display"
            fi
            ;;
        skip)
            if [[ "$VERBOSE" == "true" ]]; then
                printf "  ${DIM}%-68s SKIP${NC}\n" "$local_display"
            fi
            ;;
    esac
done

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

# --------------------------------------------------------------------------
# Write summary.txt for CI step summary
# --------------------------------------------------------------------------
if [[ -n "$RESULTS_DIR" ]] && [[ -d "$RESULTS_DIR" ]]; then
    {
        echo "| Metric | Count |"
        echo "|--------|-------|"
        echo "| Total | ${TOTAL} |"
        echo "| Passed | ${PASS_COUNT} |"
        echo "| Failed | ${FAIL_COUNT} |"
        echo "| Known | ${KNOWN_HITS} |"
        echo "| New Failures | ${NEW_FAILURES} |"
        echo "| Skipped | ${SKIP_COUNT} |"
    } > "${RESULTS_DIR}/summary.txt"
fi

# --------------------------------------------------------------------------
# Report new failures
# --------------------------------------------------------------------------
if [[ "$NEW_FAILURES" -gt 0 ]]; then
    echo -e "${RED}${BOLD}RESULT: ${NEW_FAILURES} new failure(s) detected!${NC}"
    echo ""
    echo "New failures not in KNOWN_FAILURES.md:"
    for name in "${NEW_FAILURE_LIST[@]}"; do
        echo "  - ${name}"
    done
    echo ""
    echo "To add as known failures, append to KNOWN_FAILURES.md:"
    echo "  | <test-name> | <category> | <reason> | - |"
    echo ""
else
    echo -e "${GREEN}${BOLD}RESULT: All failures are known. CI green.${NC}"
    echo ""
fi

# Exit with count of new failures (0 = success)
exit "$NEW_FAILURES"
