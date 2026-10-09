#!/usr/bin/env bash
# Exercise the nfstest grader: name normalisation, grading, and logs from runs
# that did not finish.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILURES=0

cat >"$WORK/known.md" <<'EOF'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| posix/read: file st_atime should be updated | semantics | expected failure | - |
| posix/seekdir: traceback | semantics | expected failure | - |
EOF

# run_case NAME WANT_EXIT MESSAGE [WANT_VERDICT] < log
run_case() {
    local name="$1" want="$2" message="$3" verdict="${4:-}" got
    cat >"$WORK/nfstest.log"
    rm -rf "$WORK/results" && mkdir -p "$WORK/results"
    "$SCRIPT_DIR/parse-results.sh" "$WORK/nfstest.log" "$WORK/known.md" "$WORK/results" \
        >"$WORK/output" 2>&1
    got=$?
    if [[ "$got" -ne "$want" ]] || ! grep -qF -- "$message" "$WORK/output"; then
        echo "FAIL: $name: exit $got, want $want; expected '$message'"
        cat "$WORK/output"
        FAILURES=$((FAILURES + 1))
    elif [[ -n "$verdict" && "$(cat "$WORK/results/verdict" 2>/dev/null)" != "$verdict" ]]; then
        echo "FAIL: $name: verdict '$(cat "$WORK/results/verdict" 2>/dev/null)', want '$verdict'"
        FAILURES=$((FAILURES + 1))
    else
        echo "ok: $name"
    fi
}

run_case "known failures only" 0 "All failures are known" "graded 0 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
*** Verify POSIX API read() on NFSv3
    TEST: Running test 'read'
    PASS: read - read should succeed
    FAIL: read - file st_atime should be updated
    TEST: Running test 'seekdir'
    FAIL: Traceback (most recent call last):
            File "/x/nfstest_posix", line 1715, in _tell_seek_dir_test
          ValueError: NULL pointer access
3 tests (1 passed, 2 failed)
NFSTEST-DONE 1
EOF

# The pass/fail tally moves between runs and "|" would end the table cell, so
# neither may reach the name a blacklist entry has to match.
run_case "name normalisation" 1 "posix/open: opening existent file should return an error when O_EXCL+O_CREAT is used" "graded 1 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
    TEST: Running test 'open'
    FAIL: open - opening existent file should return an error when O_EXCL|O_CREAT is used (256 passed, 256 failed)
1 tests (0 passed, 1 failed)
NFSTEST-DONE 1
EOF

# A new failure inside a subtest with a known one still counts.
run_case "new failure beside a known one" 1 "1 new failure(s)" "graded 1 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
    TEST: Running test 'read'
    FAIL: read - file st_atime should be updated
    FAIL: read - read should return the data written
2 tests (0 passed, 2 failed)
NFSTEST-DONE 1
EOF

run_case "module without a tally" 1 "lock: did not finish" "graded 1 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
    TEST: Running test 'read'
    PASS: read - read should succeed
1 tests (1 passed, 0 failed)
NFSTEST-MODULE nfstest_lock
    TEST: Running test 'btest01'
    PASS: Locking byte range should be granted
NFSTEST-DONE 2
EOF

run_case "no done marker" 1 "did not finish" "ungraded 0 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
    PASS: read - read should succeed
1 tests (1 passed, 0 failed)
EOF

run_case "module count mismatch" 1 "but the run reported 2" "ungraded 0 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
    PASS: read - read should succeed
1 tests (1 passed, 0 failed)
NFSTEST-DONE 2
EOF

# nfstest printing its usage and exiting is a module that did not run.
run_case "module that printed only usage" 1 "posix: did not finish" "graded 1 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
Usage: nfstest_posix --server <server> [options]
NFSTEST-DONE 1
EOF

# A tally that disagrees with the lines parsed means a line went unread, and
# that line could have been a failure: never green, even when every failure
# that was read is known.
run_case "tally disagrees with parsed lines" 1 "posix: tally says 1 passed, 2 failed; parsed 1, 1" "graded 1 0 0" <<'EOF'
NFSTEST-MODULE nfstest_posix
    TEST: Running test 'read'
    PASS: read - read should succeed
    FAIL: read - file st_atime should be updated
3 tests (1 passed, 2 failed)
NFSTEST-DONE 1
EOF

# The real tables: a row only v4.0 needs must excuse its failure on v4.0,
# graded against both tables as run.sh does, and on no v4.1 set.
cat >"$WORK/nfstest.log" <<'EOF'
NFSTEST-MODULE nfstest_dio
    TEST: Running test 'vectored_io'
    FAIL: Traceback (most recent call last):
          Exception: Packet trace file is empty
1 tests (0 passed, 1 failed)
NFSTEST-DONE 1
EOF
cat "$SCRIPT_DIR/KNOWN_FAILURES_V4.md" "$SCRIPT_DIR/KNOWN_FAILURES_V40.md" >"$WORK/v40.md"
if "$SCRIPT_DIR/parse-results.sh" "$WORK/nfstest.log" "$WORK/v40.md" >"$WORK/output" 2>&1; then
    echo "ok: the v4.0 tables excuse the v4.0-only row"
else
    echo "FAIL: the v4.0 tables excuse the v4.0-only row"; cat "$WORK/output"; FAILURES=$((FAILURES + 1))
fi
"$SCRIPT_DIR/parse-results.sh" "$WORK/nfstest.log" "$SCRIPT_DIR/KNOWN_FAILURES_V4.md" >"$WORK/output" 2>&1
if [[ $? -eq 1 ]]; then
    echo "ok: the shared v4 table does not"
else
    echo "FAIL: the shared v4 table does not"; cat "$WORK/output"; FAILURES=$((FAILURES + 1))
fi

[[ "$FAILURES" -eq 0 ]] || { echo "$FAILURES case(s) failed"; exit 1; }
echo "all nfstest grader cases passed"
