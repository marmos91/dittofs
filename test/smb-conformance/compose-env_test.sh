#!/usr/bin/env bash
# Unit tests for the conformance admission lease in compose-env.sh.
#
# The lease is the whole of admission, so what has to hold is that it refuses a
# second run regardless of which checkout that run comes from or which mode it
# would have used, and that it is gone the instant its holder is — including
# when the holder is killed rather than exiting. No Docker, no server: each
# case sources the real compose-env.sh and calls the real entry point.
#
# Usage: ./compose-env_test.sh

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
COMPOSE_ENV="${SCRIPT_DIR}/compose-env.sh"

WORK="$(mktemp -d)"
cleanup() {
    [[ -n "${HOLDER_PID:-}" ]] && kill -9 "$HOLDER_PID" 2>/dev/null
    [[ -s "${WORK}/child.pid" ]] && kill -9 "$(cat "${WORK}/child.pid")" 2>/dev/null
    rm -rf "$WORK"
}
trap cleanup EXIT

FAILURES=0
HOLDER_PID=""

# The lease under test is a scratch file, never the host-wide one a real run
# would take.
export DITTOFS_CONFORMANCE_LEASE="${WORK}/lease"

# Two copies of the harness at different repository roots, which is what two
# checkouts of DittoFS look like to compose-env.sh: each derives its own
# COMPOSE_PROJECT_NAME, and neither may admit a run while the other holds.
for co in co1 co2; do
    mkdir -p "${WORK}/${co}/test/smb-conformance"
    cp "$COMPOSE_ENV" "${WORK}/${co}/test/smb-conformance/compose-env.sh"
done
CO1="${WORK}/co1/test/smb-conformance/compose-env.sh"
CO2="${WORK}/co2/test/smb-conformance/compose-env.sh"

# hold.sh ENV_FILE READY_FLAG GATE_FIFO [CHILD_PID_FILE]
# Takes the lease through the real entry point and blocks in a shell builtin,
# so the only process holding the lease is this one — unless CHILD_PID_FILE is
# given, in which case it also leaves a child behind, which is what a killed
# harness looks like when the server or the client container outlives it.
cat > "${WORK}/hold.sh" <<'EOF'
#!/usr/bin/env bash
set -uo pipefail
# shellcheck disable=SC1090
source "$1"
require_exclusive_stack
if [[ -n "${4:-}" ]]; then
    sleep 120 &
    echo $! > "$4"
fi
: > "$2"
read -r _ < "$3" || true
EOF
chmod +x "${WORK}/hold.sh"

fail() {
    echo "FAIL: $1"
    FAILURES=$((FAILURES + 1))
}

# start_holder ENV_FILE [--with-child] — starts a run holding the lease and
# returns once it has it. --with-child leaves a process of that run behind when
# the holder is killed.
start_holder() {
    local ready="${WORK}/ready" child=""
    rm -f "$ready" "${WORK}/child.pid"
    [[ "${2:-}" == "--with-child" ]] && child="${WORK}/child.pid"
    rm -f "${WORK}/gate"
    mkfifo "${WORK}/gate"
    "${WORK}/hold.sh" "$1" "$ready" "${WORK}/gate" "$child" \
        >"${WORK}/holder.txt" 2>&1 &
    HOLDER_PID=$!
    local waited=0
    while [[ ! -f "$ready" ]]; do
        if ! kill -0 "$HOLDER_PID" 2>/dev/null; then
            fail "holder exited before taking the lease"
            cat "${WORK}/holder.txt"
            return 1
        fi
        sleep 0.1
        waited=$((waited + 1))
        [[ "$waited" -gt 300 ]] && { fail "holder never took the lease"; return 1; }
    done
    return 0
}

# try_admit ENV_FILE — attempts a run, leaving output in $WORK/last.txt.
# Echoes the exit status.
try_admit() {
    # shellcheck disable=SC2016
    bash -c 'source "$1"; require_exclusive_stack; echo ADMITTED' _ "$1" \
        >"${WORK}/last.txt" 2>&1
    echo $?
}

assert_output() {
    grep -qF -- "$2" "${WORK}/last.txt" || {
        fail "$1: output missing '$2'"
        cat "${WORK}/last.txt"
        return
    }
    echo "ok: $1"
}

# -- The two copies really are distinct checkouts, or the contention tests
# -- below would only be proving that one project name blocks itself. --
name1="$(bash -c 'source "$1"; echo "$COMPOSE_PROJECT_NAME"' _ "$CO1")"
name2="$(bash -c 'source "$1"; echo "$COMPOSE_PROJECT_NAME"' _ "$CO2")"
if [[ "$name1" == "$name2" ]]; then
    fail "the two checkouts share a Compose project name (${name1})"
else
    echo "ok: distinct checkouts (${name1} vs ${name2})"
fi

# -- A second run is refused while the first is in flight, and is told what it
# -- lost to. The two runs come from different checkouts, which the label scan
# -- this lease replaces treated as unrelated. --
if start_holder "$CO1"; then
    got="$(try_admit "$CO2")"
    [[ "$got" -eq 1 ]] || fail "second run admitted (exit ${got}), want refusal"
    [[ "$got" -eq 1 ]] && echo "ok: a second run from another checkout is refused"
    assert_output "the refusal names the holder" "pid ${HOLDER_PID}"
    assert_output "the refusal names the lease" "$DITTOFS_CONFORMANCE_LEASE"

    # -- A run killed outright leaves no lease to reap. Watchdogs, Ctrl-C and
    # -- cancelled CI jobs make this the common ending, and a lease that had to
    # -- be cleared by hand would be worse than the check it replaces. --
    kill -9 "$HOLDER_PID" 2>/dev/null
    wait "$HOLDER_PID" 2>/dev/null
    HOLDER_PID=""
    got="$(try_admit "$CO2")"
    [[ "$got" -eq 0 ]] || fail "lease survived a killed holder (exit ${got})"
    [[ "$got" -eq 0 ]] && echo "ok: a killed holder leaves no lease behind"
fi

# -- Killing the harness while a process it started is still alive keeps the
# -- lease taken. That is the point: --mode local leaves a native server
# -- holding the ports, and admitting a run against it is the non-result the
# -- lease exists to prevent. --
if start_holder "$CO1" --with-child; then
    child="$(cat "${WORK}/child.pid")"
    kill -9 "$HOLDER_PID" 2>/dev/null
    wait "$HOLDER_PID" 2>/dev/null
    HOLDER_PID=""
    got="$(try_admit "$CO2")"
    [[ "$got" -eq 1 ]] || fail "a run was admitted while the previous run's processes live (exit ${got})"
    [[ "$got" -eq 1 ]] && echo "ok: the lease survives while a process of the run does"

    kill -9 "$child" 2>/dev/null
    wait "$child" 2>/dev/null
    : > "${WORK}/child.pid"
    got="$(try_admit "$CO2")"
    [[ "$got" -eq 0 ]] || fail "lease survived the last process of the run (exit ${got})"
    [[ "$got" -eq 0 ]] && echo "ok: the lease goes with the run's last process"
fi

# -- Back-to-back runs from the same checkout are the normal case and must not
# -- refuse each other. --
got="$(try_admit "$CO1")"
[[ "$got" -eq 0 ]] || fail "a run was refused with no other run in flight (exit ${got})"
[[ "$got" -eq 0 ]] && echo "ok: successive runs are admitted"
got="$(try_admit "$CO1")"
[[ "$got" -eq 0 ]] || fail "the previous run's lease outlived it (exit ${got})"
[[ "$got" -eq 0 ]] && echo "ok: the lease is released when its run exits"

# -- An unwritable lease path is refused rather than silently skipped: a lease
# -- that cannot be taken must never read as admission. --
export DITTOFS_CONFORMANCE_LEASE="${WORK}/no-such-dir/lease"
got="$(try_admit "$CO1")"
[[ "$got" -eq 1 ]] || fail "an unopenable lease admitted the run (exit ${got})"
[[ "$got" -eq 1 ]] && echo "ok: an unopenable lease refuses the run"

echo ""
if [[ "$FAILURES" -eq 0 ]]; then
    echo "PASS: all compose-env.sh admission tests passed"
    exit 0
fi
echo "FAIL: ${FAILURES} assertion(s) failed"
exit 1
