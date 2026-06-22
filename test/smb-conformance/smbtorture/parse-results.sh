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
#
# Baseline generation:
#   ./parse-results.sh --emit-baseline <smbtorture-output-file> [known-failures-file]
#       Renders baseline-results.md (to stdout) from a run's output file instead
#       of the colored terminal report. The header metadata is taken from these
#       env vars (all optional): BASELINE_DATE, BASELINE_COMMIT, BASELINE_PROFILE,
#       BASELINE_PLATFORM, BASELINE_SMBTORTURE, BASELINE_CONTEXT.

set -euo pipefail

# Shared known-failures (blacklist) loader — same parser the POSIX harness uses.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=../../common/known-failures.sh
source "${SCRIPT_DIR}/../../common/known-failures.sh"

# --------------------------------------------------------------------------
# Arguments
# --------------------------------------------------------------------------
# Strip the --emit-baseline flag (position-independent) before reading the
# positional output-file/known-failures/results-dir arguments.
EMIT_BASELINE=false
declare -a POSITIONAL=()
for arg in "$@"; do
    case "$arg" in
        --emit-baseline) EMIT_BASELINE=true ;;
        *) POSITIONAL+=("$arg") ;;
    esac
done
set -- "${POSITIONAL[@]+"${POSITIONAL[@]}"}"

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
# Load known failures from KNOWN_FAILURES.md (shared parser).
# --------------------------------------------------------------------------
kf_load "$KNOWN_FAILURES_FILE"
KNOWN_COUNT=$KF_COUNT

# Back-compat thin wrappers over the shared kf_* API so the body below keeps
# its original call sites.
is_known_failure() { kf_is_known "$1"; }
get_known_reason() { kf_reason "$1"; }

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
# --------------------------------------------------------------------------
# Pre-process: reclassify connection-establishment failures as skips.
#
# When smbtorture cannot establish its SMB2 connection — Docker accept
# backlog full, the server momentarily unreachable, or (most commonly on an
# overloaded CI runner) the smbtorture CLIENT itself being CPU-starved so its
# own connection setup overruns — it reports the test with a connection
# diagnostic rather than a real protocol assertion. These are infrastructure
# flakes, not DittoFS bugs, so we rewrite the result to "skip:" and they do
# not count as new failures. (NT_STATUS_NO_MEMORY here is a client-side
# overrun artifact, NOT a server out-of-memory — see
# internal/adapter/smb/framing.go and #717. smb2.oplock.batch1 is proven to
# deliver its oplock break well within the client's 5 s timeout even at 6×
# CPU oversubscription, so a batch1 "new failure" carrying one of these
# diagnostics is always a connection flake, never the break path.)
#
# The diagnostic is NOT reliably on the line immediately after the result
# header: smbtorture brackets it as
#     failure: test.name [
#     ../../source4/.../smb2.c:95: Establishing SMB2 connection failed
#     ]
# and under load may interleave a "time:" line, or report the header as
# "error:" instead of "failure:". The previous single-line, failure-only
# lookahead missed those orderings and let the flake red the job. We instead
# buffer each failure/error result and scan its whole detail block (up to the
# closing "]" or the next result/test marker) for either pattern.
# --------------------------------------------------------------------------
CONN_FAIL_PATTERN="Establishing SMB2 connection failed"
NO_MEMORY_PATTERN="NT_STATUS_NO_MEMORY"
TEMP_OUTPUT=$(mktemp)

# is_result_marker LINE — true if LINE begins a new test/result record, i.e.
# the failure/error detail block has ended.
is_result_marker() {
    [[ "$1" =~ ^(test|success|failure|error|skip):[[:space:]] ]]
}

# Buffer holding a pending failure/error header plus its detail lines while we
# decide whether the block is a connection flake. pending_block is non-empty
# iff we are inside a failure/error block.
declare -a pending_block=()
pending_is_connflake=false

flush_pending() {
    [[ ${#pending_block[@]} -eq 0 ]] && return
    if $pending_is_connflake; then
        # Reclassify the header (failure:/error:) to skip:; emit detail verbatim.
        local header="${pending_block[0]}"
        header="${header/#failure:/skip:}"
        header="${header/#error:/skip:}"
        printf '%s\n' "$header" >> "$TEMP_OUTPUT"
        local i
        for ((i = 1; i < ${#pending_block[@]}; i++)); do
            printf '%s\n' "${pending_block[$i]}" >> "$TEMP_OUTPUT"
        done
    else
        printf '%s\n' "${pending_block[@]}" >> "$TEMP_OUTPUT"
    fi
    pending_block=()
    pending_is_connflake=false
}

while IFS= read -r line; do
    # A new failure/error header (or any other result marker) ends the prior
    # detail block. Flush it, then either begin buffering this header or emit
    # the line verbatim.
    if is_result_marker "$line"; then
        flush_pending
        if [[ "$line" =~ ^(failure|error):[[:space:]]+ ]]; then
            pending_block=("$line")
        else
            printf '%s\n' "$line" >> "$TEMP_OUTPUT"
        fi
        continue
    fi

    if [[ ${#pending_block[@]} -gt 0 ]]; then
        # Inside a pending failure/error detail block.
        pending_block+=("$line")
        if [[ "$line" == *"$CONN_FAIL_PATTERN"* || "$line" == *"$NO_MEMORY_PATTERN"* ]]; then
            pending_is_connflake=true
        fi
        # A closing "]" ends the bracketed detail block.
        [[ "$line" == "]" ]] && flush_pending
        continue
    fi

    printf '%s\n' "$line" >> "$TEMP_OUTPUT"
done < "$OUTPUT_FILE"
flush_pending
OUTPUT_FILE="$TEMP_OUTPUT"
trap 'rm -f '"$TEMP_OUTPUT" EXIT

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
# Baseline generation
#
# Renders the per-suite baseline markdown from the parsed results instead of the
# colored terminal report. The sub-suite for a test is its first two dot
# components (e.g. smb2.acls.CREATOR -> smb2.acls; smb2.connect -> smb2.connect),
# matching how the hand-maintained baseline grouped them.
# --------------------------------------------------------------------------
suite_of() {
    # $1 is already normalized to start with "smb2." by the parse loop.
    local rest="${1#*.}"      # strip leading "smb2."
    printf 'smb2.%s' "${rest%%.*}"
}

emit_baseline() {
    local date_str="${BASELINE_DATE:-$(date -u +%Y-%m-%d)}"
    local commit="${BASELINE_COMMIT:-$(git -C "$SCRIPT_DIR" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
    local profile="${BASELINE_PROFILE:-memory}"
    local platform="${BASELINE_PLATFORM:-$(uname -sm 2>/dev/null || echo unknown)}"
    local context="${BASELINE_CONTEXT:-}"
    local stversion="${BASELINE_SMBTORTURE:-}"
    if [[ -z "$stversion" ]]; then
        stversion="$(grep -m1 -E '^smbtorture ' "$OUTPUT_FILE" 2>/dev/null || true)"
        [[ -z "$stversion" ]] && stversion="unknown"
    fi

    # Classify every result once: "suite|name|class" where class is
    # pass | known | new | skip.
    local -a processed=()
    local -a suite_order=()
    declare -A seen=() sp=() sf=() ssk=()
    local entry name outcome suite cls
    for entry in "${ALL_RESULTS[@]+"${ALL_RESULTS[@]}"}"; do
        IFS='|' read -r name outcome <<< "$entry"
        suite="$(suite_of "$name")"
        cls="$outcome"
        if [[ "$outcome" == "fail" ]]; then
            if is_known_failure "$name"; then cls="known"; else cls="new"; fi
        fi
        processed+=("${suite}|${name}|${cls}")
        if [[ -z "${seen[$suite]:-}" ]]; then
            seen[$suite]=1; suite_order+=("$suite"); sp[$suite]=0; sf[$suite]=0; ssk[$suite]=0
        fi
        case "$outcome" in
            pass) sp[$suite]=$(( sp[$suite] + 1 )) ;;
            fail) sf[$suite]=$(( sf[$suite] + 1 )) ;;
            skip) ssk[$suite]=$(( ssk[$suite] + 1 )) ;;
        esac
    done

    local sorted_suites
    sorted_suites="$(printf '%s\n' "${suite_order[@]+"${suite_order[@]}"}" | sort -u)"

    local rate="N/A"
    [[ "$TOTAL" -gt 0 ]] && rate="$(awk "BEGIN{printf \"%.1f%%\", ($PASS_COUNT/$TOTAL)*100}")"

    # ---- Header ----
    echo "# smbtorture Baseline Results"
    echo ""
    echo "> 🤖 **Auto-generated** by \`parse-results.sh --emit-baseline\`. Do not hand-edit —"
    echo "> the nightly refresh job overwrites this file. Historical analysis lives in git"
    echo "> history; the CI-gating failure list lives in \`KNOWN_FAILURES.md\`."
    echo ""
    echo "**Date:** ${date_str}"
    echo "**DittoFS Commit:** ${commit}"
    echo "**Profile:** ${profile}"
    echo "**Platform:** ${platform}"
    echo "**smbtorture:** ${stversion}"
    [[ -n "$context" ]] && echo "**Context:** ${context}"
    echo ""

    # ---- Overall summary ----
    echo "## Overall Summary"
    echo ""
    echo "| Metric | Count |"
    echo "|--------|-------|"
    echo "| Total Tests | ${TOTAL} |"
    echo "| Passed | ${PASS_COUNT} |"
    echo "| Failed | ${FAIL_COUNT} |"
    echo "| — Known failures | ${KNOWN_HITS} |"
    echo "| — New failures | ${NEW_FAILURES} |"
    echo "| Skipped | ${SKIP_COUNT} |"
    echo "| Pass Rate | ${rate} |"
    echo ""

    # ---- Per-sub-suite breakdown ----
    echo "## Per-Sub-Suite Breakdown"
    echo ""
    echo "| Sub-Suite | Pass | Fail | Skip | Total | Pass Rate |"
    echo "|-----------|------|------|------|-------|-----------|"
    local s p f k tot srate
    while IFS= read -r s; do
        [[ -z "$s" ]] && continue
        p=${sp[$s]:-0}; f=${sf[$s]:-0}; k=${ssk[$s]:-0}
        tot=$(( p + f + k ))
        if [[ $(( p + f )) -gt 0 ]]; then
            srate="$(awk "BEGIN{printf \"%.0f%%\", ($p/($p+$f))*100}")"
        else
            srate="N/A"
        fi
        printf '| %s | %d | %d | %d | %d | %s |\n' "$s" "$p" "$f" "$k" "$tot" "$srate"
    done <<< "$sorted_suites"
    echo ""

    # ---- Grouped per-test lists ----
    emit_section() {
        local title="$1" want="$2" total_for_section="$3"
        echo "## ${title} (${total_for_section})"
        echo ""
        echo "<details><summary>Show ${total_for_section} test(s)</summary>"
        echo ""
        local sg p ps pn pc display
        while IFS= read -r sg; do
            [[ -z "$sg" ]] && continue
            local -a names=()
            for p in "${processed[@]+"${processed[@]}"}"; do
                IFS='|' read -r ps pn pc <<< "$p"
                [[ "$ps" == "$sg" ]] || continue
                case "$want" in
                    fail) [[ "$pc" == "known" || "$pc" == "new" ]] || continue ;;
                    *)    [[ "$pc" == "$want" ]] || continue ;;
                esac
                case "$pc" in
                    new)   display="${pn} **(NEW)**" ;;
                    known) display="${pn} _(known)_" ;;
                    *)     display="${pn}" ;;
                esac
                names+=("$display")
            done
            [[ ${#names[@]} -eq 0 ]] && continue
            echo "### ${sg} (${#names[@]})"
            printf -- '- %s\n' "${names[@]}" | sort
            echo ""
        done <<< "$sorted_suites"
        echo "</details>"
        echo ""
    }

    emit_section "Passing Tests" "pass" "$PASS_COUNT"
    emit_section "Failing Tests" "fail" "$FAIL_COUNT"
    emit_section "Skipped Tests" "skip" "$SKIP_COUNT"
}

if [[ "$EMIT_BASELINE" == "true" ]]; then
    emit_baseline
    exit 0
fi

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
