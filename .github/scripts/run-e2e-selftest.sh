#!/usr/bin/env bash
#
# Self-test for run-e2e.sh. Drives the harness with stand-in test commands that
# reproduce each way a real run loses its signal, and asserts the harness reports
# failure, promptly, every time.
#
# Case 3 also runs the *old* pipeline shape against the same stand-in and asserts
# it does NOT finish — without that, the case would pass against a harness that
# never had to refuse anything.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
harness="$here/run-e2e.sh"
work="$(mktemp -d)"
# The leak stand-in deliberately leaves a descriptor holder behind; its odd sleep
# duration is what makes it identifiable, so reap it here rather than let it (and
# the control case's `tee`, which it holds open) outlive this script.
trap 'rm -rf "$work"; pkill -f "sleep 654321" >/dev/null 2>&1' EXIT

# Stand-in test commands. Output mimics `go test -v` closely enough for grading.
cat >"$work/pass.sh" <<'EOF'
#!/usr/bin/env bash
echo "--- PASS: TestThing (0.01s)"
echo "PASS"
printf 'ok  \tgithub.com/example/pkg\t0.012s\n'
EOF

cat >"$work/fail.sh" <<'EOF'
#!/usr/bin/env bash
echo "--- FAIL: TestThing (0.01s)"
printf 'FAIL\tgithub.com/example/pkg\t0.012s\n'
echo "FAIL"
exit 1
EOF

# Times out internally (like `go test -timeout`), then exits — leaving a child
# that still holds the inherited stdout/stderr descriptors.
cat >"$work/leak.sh" <<'EOF'
#!/usr/bin/env bash
echo "panic: test timed out after 30m0s"
echo "	running tests:"
printf 'FAIL\tgithub.com/example/pkg\t1800.031s\n'
printf 'ok  \tgithub.com/example/pkg/other\t0.7s\n'
echo "FAIL"
sleep 654321 &
exit 1
EOF

# Never finishes at all.
cat >"$work/wedge.sh" <<'EOF'
#!/usr/bin/env bash
echo "=== RUN   TestThing"
sleep 60
EOF

# Exits 0 having produced no result — the false-green shape.
cat >"$work/green.sh" <<'EOF'
#!/usr/bin/env bash
echo "=== RUN   TestThing"
exit 0
EOF

chmod +x "$work"/*.sh

fails=0
run_case() {
    local name="$1" want="$2" max_s="$3" wall="$4"
    shift 4
    local start elapsed got
    start=$SECONDS
    # The harness is supposed to bound itself; bound it from outside too so a
    # harness that has lost that property reports a clean failure here instead
    # of hanging this script the way it hung the job.
    E2E_LOG="$work/$name.log" E2E_WALL="$wall" E2E_WALL_GRACE=2s \
        timeout --kill-after=5s "$((max_s + 10))s" "$harness" "$@" >"$work/$name.out" 2>&1
    got=$?
    elapsed=$((SECONDS - start))
    if [ "$got" -eq 124 ] || [ "$got" -eq 137 ]; then
        echo "FAIL $name: harness did not return within $((max_s + 10))s"
        fails=$((fails + 1))
        return
    fi
    if [ "$got" -ne "$want" ]; then
        echo "FAIL $name: exit $got, want $want"
        sed 's/^/    /' "$work/$name.out"
        fails=$((fails + 1))
        return
    fi
    if [ "$elapsed" -gt "$max_s" ]; then
        echo "FAIL $name: took ${elapsed}s, want <= ${max_s}s"
        fails=$((fails + 1))
        return
    fi
    echo "ok   $name: exit $got in ${elapsed}s — $(grep -o '::error title=[^:]*::.*' "$work/$name.out" || echo 'passed')"
}

run_case pass  0 10 30s "$work/pass.sh"
run_case fail  1 10 30s "$work/fail.sh"
run_case leak  1 10 30s "$work/leak.sh"
run_case wedge 1 15  5s "$work/wedge.sh"
run_case green 1 10 30s "$work/green.sh"

# A wedge must fail *because of the wall*. Asserting only the exit code would let
# a stand-in that dies for some unrelated reason stand in for a wedge.
reason() {
    if ! grep -q "$2" "$work/$1.out"; then
        echo "FAIL $1: failed for the wrong reason (wanted \"$2\")"
        sed 's/^/    /' "$work/$1.out"
        fails=$((fails + 1))
    fi
}
reason wedge "no result after"
reason green "no package reported ok"
reason fail  "test command exited 1"

# The real suite runs under sudo, where the harness may not be permitted to
# signal what it started at all. A wall that only holds against a signalable
# child is not a wall, so prove it against a root one wherever that is possible.
if sudo -n true >/dev/null 2>&1; then
    run_case sudo_wedge 1 15 5s sudo -n "$work/wedge.sh"
    reason sudo_wedge "no result after"
else
    echo "skip sudo_wedge: sudo needs a password here"
fi

# The leak case must actually be able to hang something, or it proves nothing.
# The old shape was `<cmd> 2>&1 | tee log`; give it 10s to finish and expect it
# not to.
start=$SECONDS
timeout 10s bash -c 'set -o pipefail; "$1" 2>&1 | tee "$2" >/dev/null' _ "$work/leak.sh" "$work/old.log"
old=$?
if [ "$old" -eq 124 ]; then
    echo "ok   control: old pipeline shape still running after $((SECONDS - start))s on the same stand-in"
else
    echo "FAIL control: old pipeline shape exited $old — the leak case no longer reproduces the defect"
    fails=$((fails + 1))
fi

if [ "$fails" -ne 0 ]; then
    echo "$fails case(s) failed"
    exit 1
fi
echo "all cases passed"
