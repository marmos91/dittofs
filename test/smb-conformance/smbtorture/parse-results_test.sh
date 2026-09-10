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
| smb2.charset.Testing | proto | unpaired surrogate | - |
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
printf 'smb2.notify\t120\tno budget helps\n' > "${WORK}/timeouts.txt"
run_case "new failure inside a timed-out suite still fails" 1 <<'EOF'
test: smb2.notify.dir
failure: smb2.notify.dir [ regression ]
test: smb2.notify.mask
EOF
assert_output "timed-out suite reported" "- smb2.notify (gave up after 120s)"
assert_output "new failure inside timed-out suite named" "- smb2.notify.dir"
rm -f "${WORK}/timeouts.txt"

# -- A truncation someone signed off on is reported but stays green. --
printf 'smb2.notify\t120\tno budget helps\n' > "${WORK}/timeouts.txt"
run_case "signed-off timeout stays green" 0 <<'EOF'
test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.notify.mask
EOF
assert_output "timeout counted" "| Suites cut short | 1 |"
assert_output "signed-off timeout not counted against" "| Cut short unexpectedly | 0 |"
assert_output "sign-off reason shown" "expected: no budget helps"
rm -f "${WORK}/timeouts.txt"

# -- A truncation nobody signed off on reds the job on its own: the tests past
# -- the cut point are missing, not passing. This is the whole point of the
# -- third column, so it is pinned from both directions. --
printf 'smb2.compound_find\t120\t\n' > "${WORK}/timeouts.txt"
run_case "unexplained timeout fails the job" 1 <<'EOF'
test: smb2.connect.connect1
success: smb2.connect.connect1
test: smb2.compound_find.compound_find_close
EOF
assert_output "unexplained timeout counted" "| Cut short unexpectedly | 1 |"
assert_output "unexplained timeout named" "- smb2.compound_find (gave up after 120s)"
assert_output "unexplained timeout explains itself" "cut short with no sign-off"
rm -f "${WORK}/timeouts.txt"

# -- A two-field row (no third column at all) is unexplained too, not a parse
# -- error: the file gains a column, and an old row must not read as signed off. --
printf 'smb2.compound_find\t120\n' > "${WORK}/timeouts.txt"
run_case "two-field timeout row fails the job" 1 <<'EOF'
test: smb2.connect.connect1
success: smb2.connect.connect1
EOF
rm -f "${WORK}/timeouts.txt"

# -- Failures and unexplained truncations both count toward the exit code. --
printf 'smb2.compound_find\t120\t\n' > "${WORK}/timeouts.txt"
run_case "failure plus unexplained timeout counts both" 2 <<'EOF'
test: smb2.rename.rename1
failure: smb2.rename.rename1 [ regression ]
EOF
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

# -- smb2.replay.channel-sequence draws a ChannelSequence at random and, once
# in 32768 draws, lands on 0x7fff while expecting STATUS_FILE_NOT_AVAILABLE —
# a value the same table elsewhere requires to succeed. That draw is excused;
# any other channel-sequence failure is graded normally. --
run_case "self-contradictory CSN draw does not fail the job" 0 <<'EOF'
test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
EOF

run_case "channel-sequence failure without the draw still fails" 1 <<'EOF'
test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7ffe, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
EOF

run_case "one draw excuses only one failure block" 1 <<'EOF'
test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
failure: smb2.replay.channel-sequence [
a second, unrelated failure
]
EOF

run_case "the excused draw does not leak into the next test" 1 <<'EOF'
test: smb2.replay.channel-sequence
Testing setinfo (replay: true) with CSN 0x7fff, expecting: NT_STATUS_FILE_NOT_AVAILABLE
failure: smb2.replay.channel-sequence [
failed to test CSN with replay flag
]
test: smb2.replay.replay3
failure: smb2.replay.replay3 [ unrelated ]
EOF

# -- NT_STATUS_NO_MEMORY is not on its own evidence of anything. smbtorture's
# own iconv returns it for an unpaired UTF-16 surrogate, so
# smb2.charset.Testing partial surrogate fails with a genuine protocol
# assertion carrying that status. Excusing it hid a real, every-run failure
# and then advertised the row as a candidate to drop from KNOWN_FAILURES.md. --
run_case "NO_MEMORY assertion is still a failure" 0 <<'EOF'
test: charset.Testing composite character (a umlaut)
success: charset.Testing composite character (a umlaut)
test: charset.Testing partial surrogate
failure: charset.Testing partial surrogate [
../../source4/torture/smb2/charset.c:174: status was NT_STATUS_NO_MEMORY, expected NT_STATUS_OK: Failed to create partial surrogate 1
]
test: charset.Testing wide-a
success: charset.Testing wide-a
EOF
assert_output "NO_MEMORY assertion counted as a failure" "| Failed | 1 |"
assert_output "NO_MEMORY assertion graded against the blacklist" "### Known failures still failing (1)"
assert_not_output "NO_MEMORY assertion is not a removal candidate" "### Known failures that now PASS"

# -- The same assertion on a test that is not blacklisted reds the job. --
run_case "unlisted NO_MEMORY assertion fails the job" 1 <<'EOF'
test: smb2.rename.rename1
failure: smb2.rename.rename1 [
../../source4/torture/smb2/rename.c:174: status was NT_STATUS_NO_MEMORY, expected NT_STATUS_OK: whatever
]
EOF
assert_output "unlisted NO_MEMORY assertion named" "- smb2.rename.rename1"

# -- NO_MEMORY still earns the flake excuse when the block says the client was
# setting up its connection — the case the pattern was added for. --
run_case "NO_MEMORY during connection setup is excused" 0 <<'EOF'
test: smb2.rename.rename1
failure: smb2.rename.rename1 [
../../source4/torture/smb2/rename.c:41: status was NT_STATUS_NO_MEMORY, expected NT_STATUS_OK: smb2_connect failed
]
EOF
assert_output "excused block counted" "| Reclassified as flakes | 1 |"
assert_output "excused block named" "- smb2.rename.rename1 — connection setup"

# -- The canonical connect diagnostic is excused on its own, and says so. --
run_case "connection diagnostic is excused" 0 <<'EOF'
test: rename.rename1
failure: rename.rename1 [
../../source4/torture/smb2/rename.c:41: Establishing SMB2 connection failed
]
EOF
assert_output "connect flake named with the suite prefix" "- smb2.rename.rename1 — connection setup"


echo ""
if [[ "$FAILURES" -eq 0 ]]; then
    echo "PASS: all parse-results.sh grading tests passed"
    exit 0
fi
echo "FAIL: ${FAILURES} assertion(s) failed"
exit 1
