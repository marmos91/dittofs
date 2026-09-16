#!/usr/bin/env bash
# Unit tests for the conformance admission lease in compose-env.sh.
#
# The lease is the host-wide half of admission, so what has to hold is that it
# refuses a second run regardless of which checkout that run comes from or which
# mode it would have used, and that it is gone the instant its holder is —
# including when the holder is killed rather than exiting. No Docker, no server:
# each case sources the real compose-env.sh and calls the real entry point,
# require_exclusive_stack, which is the function both runners call.
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

# require_exclusive_stack also runs the leftover-stack scan, which asks docker
# what is up. The test must not depend on the host's Docker state — a developer
# running this with a conformance stack up would otherwise see every case
# refuse — so docker is stubbed. Unreachable by default, which is what the scan
# treats as "nothing to report", leaving the lease as the only thing that can
# refuse. DOCKER_STUB_STACK makes the stub report a stack, which is how the case
# that the two mechanisms still compose is driven.
mkdir -p "${WORK}/bin"
cat > "${WORK}/bin/docker" <<'EOF'
#!/bin/sh
if [ -n "${DOCKER_STUB_STACK:-}" ]; then
    case "$1 $2" in
        "ps -a"|"volume ls") echo "${DOCKER_STUB_STACK}" ;;
    esac
    exit 0
fi
exit 1
EOF
chmod +x "${WORK}/bin/docker"
export PATH="${WORK}/bin:${PATH}"

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
    [[ "${2:-}" == "--with-child" ]] && child="${WORK}/child.pid"
    rm -f "$ready" "${WORK}/child.pid" "${WORK}/gate"
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

# kill_holder — ends the holding run the way a watchdog, a Ctrl-C or a
# cancelled CI job does, without letting it run any cleanup.
kill_holder() {
    kill -9 "$HOLDER_PID" 2>/dev/null
    wait "$HOLDER_PID" 2>/dev/null
    HOLDER_PID=""
}

# try_admit ENV_FILE — attempts a run, leaving output in $WORK/last.txt.
# Echoes the exit status.
try_admit() {
    # shellcheck disable=SC2016
    bash -c 'source "$1"; require_exclusive_stack; echo ADMITTED' _ "$1" \
        >"${WORK}/last.txt" 2>&1
    echo $?
}

# assert_exit WANT GOT NAME
assert_exit() {
    if [[ "$2" -eq "$1" ]]; then
        echo "ok: $3"
        return
    fi
    fail "$3: exit ${2}, want ${1}"
    cat "${WORK}/last.txt"
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
# -- lost to. The two runs come from different checkouts, which the per-checkout
# -- claim cannot see and which the lease is here to cover. --
if start_holder "$CO1"; then
    assert_exit 1 "$(try_admit "$CO2")" "a second run from another checkout is refused"
    assert_output "the refusal names the holder" "pid ${HOLDER_PID}"
    assert_output "the refusal names the lease" "$DITTOFS_CONFORMANCE_LEASE"

    # -- A run killed outright leaves no lease to reap. Watchdogs, Ctrl-C and
    # -- cancelled CI jobs make this the common ending, and a lease that had to
    # -- be cleared by hand would be worse than the check it replaces. --
    kill_holder
    assert_exit 0 "$(try_admit "$CO2")" "a killed holder leaves no lease behind"
fi

# -- Killing the harness while a process it started is still alive keeps the
# -- lease taken. That is the point: --mode local leaves a native server
# -- holding the ports, and admitting a run against it is the non-result the
# -- lease exists to prevent. --
if start_holder "$CO1" --with-child; then
    child="$(cat "${WORK}/child.pid")"
    kill_holder
    assert_exit 1 "$(try_admit "$CO2")" "the lease survives while a process of the run does"

    # Not `wait`: $child was forked inside hold.sh, so it is not a job of this
    # shell and `wait` would return at once without it having gone anywhere.
    kill -9 "$child" 2>/dev/null
    while kill -0 "$child" 2>/dev/null; do sleep 0.1; done
    : > "${WORK}/child.pid"
    assert_exit 0 "$(try_admit "$CO2")" "the lease goes with the run's last process"
fi

# -- Back-to-back runs from the same checkout are the normal case and must not
# -- refuse each other. --
assert_exit 0 "$(try_admit "$CO1")" "successive runs are admitted"
assert_exit 0 "$(try_admit "$CO1")" "the lease is released when its run exits"

# -- An unwritable lease path is refused rather than silently skipped: a lease
# -- that cannot be taken must never read as admission. --
export DITTOFS_CONFORMANCE_LEASE="${WORK}/no-such-dir/lease"
assert_exit 1 "$(try_admit "$CO1")" "an unopenable lease refuses the run"

export DITTOFS_CONFORMANCE_LEASE="${WORK}/lease"

# -- A lease that cannot be taken for want of perl is refused the same way, and
# -- says so: reporting it as a run in flight sends the reader looking for one
# -- that does not exist. --
mkdir -p "${WORK}/noperl"
printf '#!/bin/sh\nexit 127\n' > "${WORK}/noperl/perl"
chmod +x "${WORK}/noperl/perl"
PATH="${WORK}/noperl:${PATH}" assert_exit 1 "$(PATH="${WORK}/noperl:${PATH}" try_admit "$CO1")" \
    "a missing perl refuses the run"
assert_output "the refusal names perl" "needs perl"

# -- The lease did not replace the leftover scan: a stack docker still reports
# -- is refused even with the lease free, because a stack that outlived its run
# -- holds no lease yet still owns the bootstrapped volume. --
assert_exit 1 "$(DOCKER_STUB_STACK="smb-conformance-12345" try_admit "$CO1")" \
    "a leftover stack is still refused with the lease free"
assert_output "the refusal names the leftover project" "smb-conformance-12345"

echo ""
if [[ "$FAILURES" -eq 0 ]]; then
    echo "PASS: all compose-env.sh admission tests passed"
    exit 0
fi
echo "FAIL: ${FAILURES} assertion(s) failed"
exit 1
