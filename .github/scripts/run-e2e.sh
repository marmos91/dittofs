#!/usr/bin/env bash
#
# Runs the E2E suite passed as "$@", writes its combined output to $E2E_LOG, and
# grades the result. Exits 0 only for a run that completed and passed.
#
# The output goes to a *file*, never through a pipe. A pipe reader sees EOF only
# once every write end is closed, so one process the suite leaves behind holding
# the inherited descriptor keeps the reader — and therefore this script, and the
# whole job — alive long after the tests finished. A file has no such rendezvous:
# leaked holders are then merely leaked, not load-bearing. The file is replayed
# to stdout once it is complete and reading it cannot block.
#
# The run is bounded by E2E_WALL so that a wedge anywhere below this script fails
# *this step*. That is the property that matters: a job ended by its own timeout
# concludes `cancelled` rather than `failure`, and alarms that key on `failure`
# then stay silent. The suite runs under sudo, so the bound rests on being able
# to signal a privileged child — measured, not assumed: the self-test's
# sudo_wedge case asserts it wherever passwordless sudo is available.
#
# Grading uses both the exit status and the log: the status alone was already
# supposed to be sufficient and still lost the signal once, so a zero status
# with a log that shows no completed pass is treated as a failure.
set -uo pipefail

LOG="${E2E_LOG:?E2E_LOG must name the log file to write}"
WALL="${E2E_WALL:-45m}"
GRACE="${E2E_WALL_GRACE:-2m}"

if [ "$#" -eq 0 ]; then
    echo "usage: $0 <test command> [args...]" >&2
    exit 2
fi

mkdir -p "$(dirname "$LOG")"
: >"$LOG"

# TERM first so the suite can dump state, SIGKILL after GRACE for anything that
# ignores it.
timeout --signal=TERM --kill-after="$GRACE" "$WALL" "$@" >"$LOG" 2>&1
status=$?

if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then
    # Name what was still running. The holder of a wedge like this used to be
    # invisible, which is why it took five silent runs to notice one.
    echo "::group::processes still running at the wall"
    ps -eo pid,ppid,pgid,stat,etimes,user,args 2>/dev/null || true
    echo "::endgroup::"
fi

cat "$LOG"

# Anything of ours still running is a leak. It can no longer hold this step open,
# but it is worth naming.
survivors=$(pgrep -a -f '(^|/)(dfs|dfsctl|mount\.nfs|mount\.cifs|rpc\.statd|rpcbind)( |$)' 2>/dev/null)
if [ -n "$survivors" ]; then
    echo "::warning title=Leaked processes::the suite left processes running"
    printf '%s\n' "$survivors"
fi

fail() {
    echo "::error title=E2E suite failed::$1"
    exit 1
}

if [ "$status" -eq 124 ] || [ "$status" -eq 137 ]; then
    fail "no result after $WALL — the suite or something it started never finished"
fi
if [ "$status" -ne 0 ]; then
    fail "test command exited $status"
fi
if grep -qE '^(panic|fatal error):' "$LOG"; then
    fail "the test binary panicked"
fi
if grep -qE '^(FAIL|--- FAIL)' "$LOG"; then
    fail "at least one test failed"
fi
if ! grep -qE '^ok[[:space:]]' "$LOG"; then
    fail "no package reported ok — the suite did not run to completion"
fi

echo "E2E suite passed"
