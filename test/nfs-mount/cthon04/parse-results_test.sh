#!/usr/bin/env bash
# Exercise the cthon04 grader with complete, failing and interrupted logs.
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
FAILURES=0

cat >"$WORK/known.md" <<'EOF'
| Test Name | Category | Reason | Issue |
|-----------|----------|--------|-------|
| basic/test6 | semantics | expected failure | - |
| special/* | semantics | expected failure | - |
EOF

# run_case NAME WANT_EXIT MESSAGE [WANT_VERDICT] < log
run_case() {
    local name="$1" want="$2" message="$3" verdict="${4:-}" got
    cat >"$WORK/cthon04.log"
    rm -rf "$WORK/results" && mkdir -p "$WORK/results"
    "$SCRIPT_DIR/parse-results.sh" "$WORK/cthon04.log" "$WORK/known.md" "$WORK/results" \
        >"$WORK/output" 2>&1
    got=$?
    if [[ "$got" -ne "$want" ]] || ! grep -qF "$message" "$WORK/output"; then
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

run_case "all pass" 0 "All failures are known" "graded 0 0 0" <<'EOF'
CTHON04-RESULT basic/test1 PASS
CTHON04-RESULT special/holey PASS
CTHON04-DONE 2
EOF

run_case "known failures only" 0 "All failures are known" "graded 0 0 0" <<'EOF'
CTHON04-RESULT basic/test1 PASS
CTHON04-RESULT basic/test6 FAIL
CTHON04-RESULT special/telldir FAIL
CTHON04-DONE 3
EOF

run_case "new failure" 1 "1 new failure(s)" "graded 1 0 0" <<'EOF'
CTHON04-RESULT basic/test1 PASS
CTHON04-RESULT basic/test7 FAIL
CTHON04-DONE 2
EOF

# A blacklist entry names a test that fails, not one that hangs.
run_case "timeout on a known name" 1 "1 new failure(s)" "graded 1 0 0" <<'EOF'
CTHON04-RESULT basic/test6 TIMEOUT
CTHON04-DONE 1
EOF

run_case "known test now passes" 0 "consider removing" <<'EOF'
CTHON04-RESULT basic/test6 PASS
CTHON04-DONE 1
EOF

# A run cut short has printed only the results it reached; those must not be
# graded as if they were all of them.
run_case "no done marker" 1 "did not finish" "ungraded 0 0 0" <<'EOF'
CTHON04-RESULT basic/test1 PASS
EOF

run_case "result count mismatch" 1 "but the run reported 3" "ungraded 0 0 0" <<'EOF'
CTHON04-RESULT basic/test1 PASS
CTHON04-RESULT basic/test2 PASS
CTHON04-DONE 3
EOF

run_case "no results" 1 "reported no results" "ungraded 0 0 0" <<'EOF'
CTHON04-DONE 0
EOF

# The exit status is a count, capped below 256 so it cannot wrap to success.
{
    for i in $(seq 1 300); do echo "CTHON04-RESULT new/$i FAIL"; done
    echo "CTHON04-DONE 300"
} | run_case "count capped at 254" 254 "300 new failure(s)" "graded 300 0 0"

[[ "$FAILURES" -eq 0 ]] || { echo "$FAILURES case(s) failed"; exit 1; }
echo "all cthon04 grader cases passed"
