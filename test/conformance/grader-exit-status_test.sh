#!/usr/bin/env bash
# Exercise outcome classification and the eight-bit exit-status boundary.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILURES=0

command -v xmlstarlet >/dev/null 2>&1 || {
    echo "xmlstarlet is required to test the WPTS TRX grader" >&2
    exit 1
}

cat >"$WORK/known.md" <<'EOF'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| KNOWN | semantics | expected failure | - |
| known/00.t | semantics | expected failure | - |
EOF

# Each argument describes one named result; counters match the generated rows.
write_wpts_fixture() {
    local spec name outcome
    local passed=0 failed=0 errors=0 timeouts=0 aborted=0 skipped=0
    {
        echo '<TestRun xmlns="http://microsoft.com/schemas/VisualStudio/TeamTest/2010"><Results>'
        for spec in "$@"; do
            name="${spec%:*}"
            outcome="${spec##*:}"
            printf '<UnitTestResult testName="%s" outcome="%s"/>\n' "$name" "$outcome"
            case "$outcome" in
                Passed) passed=$((passed + 1)) ;;
                Failed) failed=$((failed + 1)) ;;
                Error) errors=$((errors + 1)) ;;
                Timeout) timeouts=$((timeouts + 1)) ;;
                Aborted) aborted=$((aborted + 1)) ;;
                NotExecuted) skipped=$((skipped + 1)) ;;
            esac
        done
        echo '</Results><ResultSummary>'
        printf '<Counters total="%d" passed="%d" failed="%d" error="%d" timeout="%d" aborted="%d" notExecuted="%d"/>\n' \
            "$#" "$passed" "$failed" "$errors" "$timeouts" "$aborted" "$skipped"
        echo '</ResultSummary></TestRun>'
    } >"$WORK/wpts.trx"
}

check_wpts_fixture() {
    local label="$1" expected_exit="$2" new="$3" known="$4" skipped="$5"
    shift 5
    "$TEST_DIR/smb-conformance/parse-results.sh" "$WORK/wpts.trx" "$WORK/known.md" >"$WORK/raw" 2>&1
    local got=$? failed=0 name
    sed $'s/\033\[[0-9;]*m//g' "$WORK/raw" >"$WORK/output"
    sed -n '/^--- Summary ---$/,$p' "$WORK/output" >"$WORK/summary"
    if [[ "$got" -ne "$expected_exit" ]] ||
        ! grep -qE "New failures:[[:space:]]+$new$" "$WORK/output" ||
        ! grep -qE "Known failures:[[:space:]]+$known$" "$WORK/output" ||
        ! grep -qE "Skipped:[[:space:]]+$skipped$" "$WORK/summary"; then
        failed=1
    fi
    for name in "$@"; do
        grep -qFx "  - $name" "$WORK/output" || failed=1
    done
    if [[ "$failed" -ne 0 ]]; then
        echo "FAIL: wpts $label: exit $got, want $expected_exit; classification or failure list differs"
        cat "$WORK/output"
        FAILURES=$((FAILURES + 1))
    else
        echo "ok: wpts $label"
    fi
}

for outcome in Failed Error Timeout Aborted; do
    write_wpts_fixture "NEW:$outcome"
    check_wpts_fixture "unlisted $outcome" 1 1 0 0 NEW
    write_wpts_fixture "KNOWN:$outcome"
    check_wpts_fixture "known $outcome" 0 0 1 0
done
write_wpts_fixture 'PASS1:Passed'
check_wpts_fixture 'pass' 0 0 0 0
write_wpts_fixture 'SKIP1:NotExecuted'
check_wpts_fixture 'not executed' 0 0 0 1
write_wpts_fixture 'PASS1:Passed' 'KNOWN:Failed' 'NEW_TIMEOUT:Timeout' 'NEW_ABORTED:Aborted' 'SKIP1:NotExecuted'
check_wpts_fixture 'mixed outcomes' 2 2 1 1 NEW_TIMEOUT NEW_ABORTED

for count in 0 1 254 255 256 257 512; do
    expected="$count"
    [[ "$count" -le 254 ]] || expected=254

    # Include a pass and a known failure in every fixture, so the requested
    # count must be the NEW failures rather than the total number of results.
    {
        echo '**************************************************'
        echo 'PASS1 st_sample.pass : PASS'
        echo 'KNOWN st_sample.known : FAILURE'
        for ((i = 1; i <= count; i++)); do
            echo "NEW$i st_sample.new$i : FAILURE"
        done
        echo '**************************************************'
        echo "Of those: 0 Skipped, $((count + 1)) Failed, 0 Warned, 1 Passed"
    } >"$WORK/pynfs.log"

    {
        echo 'Test Summary Report'
        echo '-------------------'
        echo '/fixture/tests/known/00.t (Wstat: 0 Tests: 1 Failed: 1)'
        for ((i = 1; i <= count; i++)); do
            echo "/fixture/tests/new/$i.t (Wstat: 0 Tests: 1 Failed: 1)"
        done
        echo "Files=$((count + 2)), Tests=$((count + 2)), 0 wallclock secs (0.01 CPU)"
        echo 'Result: FAIL'
    } >"$WORK/prove.log"

    results=('PASS1:Passed' 'KNOWN:Failed')
    outcomes=(Failed Error Timeout Aborted)
    for ((i = 1; i <= count; i++)); do
        results+=("NEW$i:${outcomes[$(((i - 1) % 4))]}")
    done
    write_wpts_fixture "${results[@]}"

    for suite in pynfs posix wpts; do
        case "$suite" in
            pynfs) parser="$TEST_DIR/nfs-conformance/pynfs/parse-results.sh"; input="$WORK/pynfs.log" ;;
            posix) parser="$TEST_DIR/posix/parse-results.sh"; input="$WORK/prove.log" ;;
            wpts) parser="$TEST_DIR/smb-conformance/parse-results.sh"; input="$WORK/wpts.trx" ;;
        esac
        "$parser" "$input" "$WORK/known.md" >"$WORK/raw" 2>&1
        got=$?
        sed $'s/\033\[[0-9;]*m//g' "$WORK/raw" >"$WORK/output"
        if [[ "$got" -ne "$expected" ]] ||
            ! grep -qiE "New failures:[[:space:]]+$count$" "$WORK/output" ||
            ! grep -qiE 'Known failures:[[:space:]]+1$' "$WORK/output"; then
            echo "FAIL: $suite with $count new failures: exit $got, want $expected; exact counts must survive"
            tail -12 "$WORK/output"
            FAILURES=$((FAILURES + 1))
        else
            echo "ok: $suite with $count new failures (exit $got, full count retained)"
        fi
    done
done

echo "failures: $FAILURES"
[[ "$FAILURES" -eq 0 ]]
