#!/usr/bin/env bash
# Unit tests for parse-results.sh grading.
#
# The verdict must depend on exactly one thing: failures that are not on the
# KNOWN_FAILURES.md blacklist. Timeouts, blacklisted failures, and tests the
# harness never finished must not move it. These tests pin that down with
# synthetic smbtorture output — no Docker, no server, runs in a second.
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
| smb2.lock.lock1 | proto | expected | - |
| smb2.oplock.* | proto | wildcard row | - |
EOF

# run_case NAME EXPECTED_EXIT <<< output
# Writes the heredoc on stdin as a results file, runs the parser, and checks
# the exit code. Parser output is left in $WORK/last.txt for assert_output.
run_case() {
    local name="$1" want="$2"
    local dir="${WORK}/case"
    rm -rf "$dir"
    mkdir -p "$dir"
    cat > "${dir}/smbtorture-output.txt"
    [[ -f "${WORK}/timeouts.txt" ]] && cp "${WORK}/timeouts.txt" "${dir}/timeouts.txt"

    "$PARSER" "${dir}/smbtorture-output.txt" "${WORK}/KNOWN_FAILURES.md" "$dir" \
        > "${WORK}/last.txt" 2>&1
    local got=$?
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
test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
EOF
assert_output "known failures listed" "### Known failures still failing (2)"

# -- One failure off the blacklist reds the job and is named. --
run_case "new failure fails the job" 1 <<'EOF'
test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
test: smb2.rename.rename1
failure: smb2.rename.rename1 [ regression ]
EOF
assert_output "new failure named" "- smb2.rename.rename1"

# -- Same results, different order: same verdict, same new-failure set. --
run_case "verdict is order-independent" 1 <<'EOF'
test: smb2.rename.rename1
failure: smb2.rename.rename1 [ regression ]
test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
test: smb2.connect.connect1
success: smb2.connect.connect1
EOF
assert_output "new failure named regardless of order" "- smb2.rename.rename1"

# -- An unfinished test is inconclusive: reported, but not a pass or a fail. --
run_case "unfinished test does not fail the job" 0 <<'EOF'
test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.notify.mask
EOF
assert_output "unfinished test counted" "| Inconclusive | 1 |"
assert_output "unfinished test named" "- smb2.notify.mask"
assert_output "unfinished test is not a pass" "| Passed | 1 |"

# -- A timeout must not hide a new failure the suite did produce. --
printf 'smb2.notify\t120\n' > "${WORK}/timeouts.txt"
run_case "new failure inside a timed-out suite still fails" 1 <<'EOF'
test: smb2.notify.dir
failure: smb2.notify.dir [ regression ]
test: smb2.notify.mask
EOF
assert_output "timed-out suite reported" "- smb2.notify (gave up after 120s)"
assert_output "new failure inside timed-out suite named" "- smb2.notify.dir"
rm -f "${WORK}/timeouts.txt"

# -- A timeout on its own is reported but leaves the job green. --
printf 'smb2.notify\t120\n' > "${WORK}/timeouts.txt"
run_case "timeout alone stays green" 0 <<'EOF'
test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.notify.mask
EOF
assert_output "timeout counted" "| Suites cut short | 1 |"
rm -f "${WORK}/timeouts.txt"

# -- A blacklisted test that passes is surfaced as a removal candidate. --
run_case "blacklisted test that passes" 0 <<'EOF'
test: smb2.lock.lock1
success: smb2.lock.lock1
test: smb2.oplock.batch1
failure: smb2.oplock.batch1 [ wildcard ]
EOF
assert_output "now-passing surfaced" "### Known failures that now PASS"
assert_output "now-passing named" "- smb2.lock.lock1"

# -- A name that both passes and fails in one run is not a removal candidate. --
run_case "flapping blacklisted test is not a candidate" 0 <<'EOF'
test: smb2.lock.lock1
success: smb2.lock.lock1
test: smb2.lock.lock1
failure: smb2.lock.lock1 [ expected ]
EOF
assert_not_output "flapping name withheld" "### Known failures that now PASS"

echo ""
if [[ "$FAILURES" -eq 0 ]]; then
    echo "PASS: all parse-results.sh grading tests passed"
    exit 0
fi
echo "FAIL: ${FAILURES} assertion(s) failed"
exit 1
