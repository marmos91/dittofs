#!/usr/bin/env bash
# Unit tests for parse-results.sh grading.
#
# The verdict must depend on exactly one thing: FAILURE lines whose test code is
# not on the KNOWN_FAILURES blacklist. Passes, warnings, unsupported operations,
# omitted tests and failure detail text must not move it, and a run that never
# reached its tally must not grade green. These pin that down with synthetic
# pynfs output — no server, no network, runs in a second.
#
# Usage: ./parse-results_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PARSER="${SCRIPT_DIR}/parse-results.sh"

WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT

FAILURES=0

cat > "${WORK}/KNOWN_FAILURES.md" <<'EOF'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| LOOK1 | proto | expected | - |
| LAYOUT* | feature | wildcard row, pNFS not implemented | - |
EOF

# run_case NAME EXPECTED_EXIT <<< output
# Writes the heredoc on stdin as a pynfs log, runs the parser, checks the exit
# code. Parser output is left in $WORK/last.txt for assert_output.
run_case() {
    local name="$1" want="$2"
    local dir="${WORK}/case"
    rm -rf "$dir"
    mkdir -p "$dir"
    cat > "${dir}/pynfs.log"

    "$PARSER" "${dir}/pynfs.log" "${WORK}/KNOWN_FAILURES.md" "$dir" \
        > "${WORK}/raw.txt" 2>&1
    local got=$?
    # The parser colourises; assertions match on the text, not the escapes.
    sed $'s/\033\[[0-9;]*m//g' "${WORK}/raw.txt" > "${WORK}/last.txt"
    if [[ "$got" -ne "$want" ]]; then
        echo "FAIL: ${name}: exit ${got}, want ${want}"
        cat "${WORK}/last.txt"
        FAILURES=$((FAILURES + 1))
        return
    fi
    echo "ok: ${name} (exit ${got})"
}

# assert_output NAME PATTERN — the last run's output must contain PATTERN.
assert_output() {
    local name="$1" pattern="$2"
    if ! grep -qF -- "$pattern" "${WORK}/last.txt"; then
        echo "FAIL: ${name}: output missing '${pattern}'"
        FAILURES=$((FAILURES + 1))
        return
    fi
    echo "ok: ${name}"
}

# assert_not_output NAME PATTERN — the last run's output must NOT contain it.
assert_not_output() {
    local name="$1" pattern="$2"
    if grep -qF -- "$pattern" "${WORK}/last.txt"; then
        echo "FAIL: ${name}: output unexpectedly contains '${pattern}'"
        FAILURES=$((FAILURES + 1))
        return
    fi
    echo "ok: ${name}"
}

# -- Blacklisted failures alone are green, exact row and wildcard row alike. --
run_case "known failures only" 0 <<'EOF'
**************************************************
LOOK1    st_lookup.testDir                                        : FAILURE
LAYOUTGET1 st_layout.testGet                                      : FAILURE
ACC1a    st_access.testReadable                                   : PASS
**************************************************
Command line asked for 3 of 689 tests
Of those: 0 Skipped, 2 Failed, 0 Warned, 1 Passed
EOF
assert_output "known count" "Known failures: 2"
assert_output "green verdict" "All failures known"

# -- One failure off the list turns the run red, and names it. --
run_case "one new failure" 1 <<'EOF'
**************************************************
LOOK1    st_lookup.testDir                                        : FAILURE
OPEN4    st_open.testStateid                                      : FAILURE
**************************************************
Command line asked for 2 of 689 tests
Of those: 0 Skipped, 2 Failed, 0 Warned, 1 Passed
EOF
assert_output "new failure named" "- OPEN4"
assert_output "append hint uses the new code" "| OPEN4 | <category> |"
assert_not_output "known row not reported as new" "- LOOK1"

# -- Two new failures exit 2: the exit code is the new-failure count. --
run_case "exit code counts new failures" 2 <<'EOF'
**************************************************
OPEN4    st_open.testStateid                                      : FAILURE
LOCK8    st_lock.testBlocking                                     : FAILURE
**************************************************
Of those: 0 Skipped, 2 Failed, 0 Warned, 0 Passed
EOF

# -- Outcomes that are not FAILURE never count, whatever pynfs calls them. --
run_case "warnings and unsupported are not failures" 0 <<'EOF'
**************************************************
OPENAT1  st_openattr.testOpenattr                                 : UNSUPPORTED
SEC2     st_secinfo.testNoName                                    : WARNING
DELEG1   st_delegation.testRead                                   : OMIT
ACC1a    st_access.testReadable                                   : PASS
**************************************************
Of those: 1 Skipped, 0 Failed, 2 Warned, 1 Passed
EOF
assert_output "nothing failing" "(none)"
assert_not_output "unsupported not graded" "OPENAT1"

# -- Failure detail lines mention the outcome in prose; they are not verdicts. --
run_case "detail text is not a test line" 0 <<'EOF'
**************************************************
LOOK1    st_lookup.testDir                                        : FAILURE
           OP_LOOKUP should return NFS4ERR_NOENT, instead got
           NFS4ERR_INVAL : FAILURE of the whole compound
**************************************************
Of those: 0 Skipped, 1 Failed, 0 Warned, 0 Passed
EOF
assert_output "only the real row graded" "Known failures: 1"

# -- A run that never reached its tally is an error, not a pass. --
run_case "truncated log is an error" 1 <<'EOF'
**************************************************
ACC1a    st_access.testReadable                                   : PASS
EOF
assert_output "truncation explained" "pynfs did not finish"

# -- An interrupted run has a tally but covered only part of the suite. --
run_case "interrupted run is an error" 1 <<'EOF'
**************************************************
ACC1a    st_access.testReadable                                   : PASS
**************************************************
Tests interrupted! Only 12 tests run
Of those: 0 Skipped, 0 Failed, 0 Warned, 12 Passed
EOF
assert_output "interruption explained" "Tests interrupted"

# -- A blacklist path that does not exist must fail loudly rather than excuse
# -- everything: kf_load returns quietly on a missing file, so the safety here
# -- comes from an empty table making every failure new.
mkdir -p "${WORK}/typo"
cat > "${WORK}/typo/pynfs.log" <<'EOF'
**************************************************
LOOK1    st_lookup.testDir                                        : FAILURE
**************************************************
Of those: 0 Skipped, 1 Failed, 0 Warned, 0 Passed
EOF
"$PARSER" "${WORK}/typo/pynfs.log" "${WORK}/does-not-exist.md" "${WORK}/typo" \
    > "${WORK}/raw.txt" 2>&1
typo_rc=$?
sed $'s/\033\[[0-9;]*m//g' "${WORK}/raw.txt" > "${WORK}/last.txt"
if [[ "$typo_rc" -ne 1 ]]; then
    echo "FAIL: missing blacklist file: exit ${typo_rc}, want 1"
    cat "${WORK}/last.txt"
    FAILURES=$((FAILURES + 1))
else
    echo "ok: missing blacklist file grades everything red (exit ${typo_rc})"
fi
assert_output "blacklist row now new" "- LOOK1"
assert_output "empty table reported" "Loaded 0 known failures"

echo ""
if [[ "$FAILURES" -eq 0 ]]; then
    echo "PASS: all parse-results.sh grading tests passed"
    exit 0
fi
echo "FAIL: ${FAILURES} assertion(s) failed"
exit 1
