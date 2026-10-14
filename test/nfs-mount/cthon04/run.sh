#!/usr/bin/env bash
# Run the Connectathon (cthon04) suite against a DittoFS export mounted with a
# named option set, then grade the result.
#
# Usage:
#   sudo ./run.sh --variant v4.1
#
# Expects a provisioned server (the suite's setup step,
# test/posix/setup-posix.sh --no-mount). Mounts the export itself, so the
# option set under test is the only one in play, and unmounts on exit.
#
# The client is a network namespace of its own (see ../netns.sh), and the
# tests run as an ordinary user (see ../test-user.sh); only the mount is root's.
# The script runs twice: on the host it sets both up and grades, and inside the
# namespace, re-executed with DITTOFS_CLIENT_NS set, it mounts and tests.
#
# Upstream's runtests scripts stop a group at its first failing test, so one
# regression hides every test after it. This runs each test binary on its own
# instead, in the order and with the arguments upstream's scripts use, and
# prints one result line per test for parse-results.sh:
#
#   CTHON04-RESULT <group>/<test> PASS|FAIL|TIMEOUT
#   CTHON04-DONE <number of results>
#
# The DONE line is the completeness marker: a log without it is not graded.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUITE_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=../option-sets.sh
source "$SUITE_DIR/option-sets.sh"
# shellcheck source=../../common/mounts.sh
source "$SUITE_DIR/../common/mounts.sh"
# shellcheck source=../netns.sh
source "$SUITE_DIR/netns.sh"
# shellcheck source=../test-user.sh
source "$SUITE_DIR/test-user.sh"

ARGS=("$@")

VARIANT=""
while [[ $# -gt 0 ]]; do
    case "$1" in
    --variant) VARIANT="${2:?--variant requires a value}"; shift 2 ;;
    --variant=*) VARIANT="${1#*=}"; shift ;;
    -h | --help) sed -n '2,25p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

[[ -n "$VARIANT" ]] || { echo "--variant is required ($(option_set_names | tr '\n' ' '))" >&2; exit 2; }
option_set_opts "$VARIANT" >/dev/null || { echo "unknown option set: $VARIANT" >&2; exit 2; }
[[ $EUID -eq 0 ]] || { echo "must run as root (it mounts)" >&2; exit 2; }

NFS_PORT="${NFS_PORT:-12049}"
MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
RESULTS_DIR="${DITTOFS_RESULTS_DIR:-$(mktemp -d)}"
TEST_TIMEOUT="${CTHON04_TEST_TIMEOUT:-600}"
LOG="$RESULTS_DIR/cthon04.log"

# The known-failure tables come from test/conformance/suites.json, which
# test/conformance/run.sh hands over in DITTOFS_KNOWN_FAILURES; the manifest is
# the one place that says which tables grade which option set.
KNOWN_FAILURES="${DITTOFS_KNOWN_FAILURES:-}"
[[ -n "$KNOWN_FAILURES" ]] || {
    echo "no known-failure tables: run through test/conformance/run.sh, or set DITTOFS_KNOWN_FAILURES" >&2
    exit 2
}

MOUNT_OPTS="$(option_set_mount_opts "$VARIANT" "$NFS_PORT")"

if [[ -z "${DITTOFS_CLIENT_NS:-}" ]]; then
    FETCHED="$("$SUITE_DIR/fetch.sh" cthon04)" || { echo "could not fetch cthon04" >&2; exit 2; }
    # A fresh private directory, not a fixed path another user could have
    # prepared; it is opened to reading only once it is this script's own.
    WORKBASE="$(mktemp -d /tmp/dittofs-nfs-mount.XXXXXX)" || exit 2
    chmod 0755 "$WORKBASE"
    CTHON="$WORKBASE/cthon04"
    teardown() {
        client_ns_down_all
        test_user_release
        rm -rf "$WORKBASE"
    }
    trap teardown EXIT
    client_ns_down_all
    test_user_ensure || { echo "could not create the test user" >&2; exit 2; }
    # The tests build and run from here as the test user.
    test_user_copy "$FETCHED" "$CTHON" || { echo "could not copy cthon04" >&2; exit 2; }
    client_ns_up 1 || exit 2

    client_ns_exec 1 env DITTOFS_CLIENT_NS=1 CTHON="$CTHON" DITTOFS_RESULTS_DIR="$RESULTS_DIR" \
        "$SCRIPT_DIR/run.sh" "${ARGS[@]}" || echo "the client side exited $?" >&2

    # The teardown step deletes the server log; keep it with the results.
    gzip -c /tmp/dittofs-posix-server.log >"$RESULTS_DIR/server.log.gz" 2>/dev/null || true

    # Not exec: the EXIT trap has to run to tear the namespace down.
    "$SCRIPT_DIR/parse-results.sh" "$LOG" "$KNOWN_FAILURES" "$RESULTS_DIR"
    exit $?
fi

# From here on this runs inside the client namespace.
SERVER="$NS_SERVER_ADDR"
AS_USER=(setpriv --reuid="$TEST_USER_UID" --regid="$TEST_USER_UID" --init-groups
    env HOME="$(test_user_home)" USER="$TEST_USER_NAME" LOGNAME="$TEST_USER_NAME")

mkdir -p "$MOUNT_POINT"
detach "$MOUNT_POINT"
echo "mount -t nfs -o $MOUNT_OPTS $SERVER:/export $MOUNT_POINT"
timeout 60 mount -t nfs -o "$MOUNT_OPTS" "$SERVER:/export" "$MOUNT_POINT" || {
    echo "mount failed" >&2
    exit 2
}
# Without this a failed mount leaves an empty local directory behind, and the
# whole suite passes against the runner's own disk.
is_mounted "$MOUNT_POINT" || { echo "$MOUNT_POINT is not a mount point" >&2; exit 2; }

unmount() {
    cd /
    detach "$MOUNT_POINT"
}
trap unmount EXIT

# What the kernel actually negotiated, which is not always what was asked for.
nfsstat -m 2>/dev/null | grep -A1 -F "$MOUNT_POINT" | tee "$RESULTS_DIR/mount-options.txt"

WORK="$MOUNT_POINT/cthon04-$$"
"${AS_USER[@]}" rm -rf "$WORK"
"${AS_USER[@]}" mkdir -p "$WORK" || { echo "the test user cannot write to the export" >&2; exit 2; }

# settle_silly_renames DIR — wait, up to 30 seconds, until DIR holds no file
# the client has silly-renamed.
#
# decision: the basic tests share one directory, and test6 onwards start by
# deleting it. A file unlinked while the client still has writes for it, as
# test5's is under a write delegation, lives on as a .nfs file until those
# writes land, and the client refuses to delete it meanwhile, so the next test
# fails on an artefact of the previous one rather than on what it tests.
# Upstream's runtests does not wait. A wait that does not end is still reported,
# so a file the client never lets go of shows up in the log.
settle_silly_renames() {
    local dir="$1" _
    for _ in $(seq 1 30); do
        "${AS_USER[@]}" find "$dir" -name '.nfs*' 2>/dev/null | grep -q . || return 0
        [[ $_ -eq 1 ]] && echo "waiting for silly-renamed files in $dir to clear"
        sleep 1
    done
    echo "silly-renamed files in $dir did not clear in 30s:"
    "${AS_USER[@]}" find "$dir" -name '.nfs*' 2>&1
}

RESULTS=0
# result NAME CMD... — run CMD under the per-test timeout and record its verdict.
result() {
    local name="$1" status verdict
    shift
    echo ""
    echo "=== $name"
    # Against a server that has died every program waits out its timeout, and
    # the job would hit its own limit with no verdict. Grade it at once instead.
    if ! nc -z -w 5 "$SERVER" "$NFS_PORT" 2>/dev/null; then
        echo "skipped: nothing answers on $SERVER:$NFS_PORT any more"
        echo "CTHON04-RESULT $name TIMEOUT"
        RESULTS=$((RESULTS + 1))
        return
    fi
    # The program changes into RUN_DIR itself, as the test user: root is
    # squashed and cannot reach the test user's directories on the export.
    timeout --kill-after=30 "$TEST_TIMEOUT" "${AS_USER[@]}" \
        bash -c 'cd "$1" || exit 1; shift; exec "$@"' in-dir "$RUN_DIR" "$@"
    status=$?
    case "$status" in
    0) verdict=PASS ;;
    124 | 137) verdict=TIMEOUT ;;
    *) verdict=FAIL ;;
    esac
    echo "CTHON04-RESULT $name $verdict"
    RESULTS=$((RESULTS + 1))
}

{
    echo "cthon04 $(git -c safe.directory="*" -C "$CTHON" rev-parse --short HEAD), option set $VARIANT: $MOUNT_OPTS"
    echo "client $(ns_addr 1), tests run as $TEST_USER_NAME (uid $TEST_USER_UID)"

    # basic: one directory for the group, because test2 removes the tree test1
    # builds. A failure does not stop the group.
    export NFSTESTDIR="$WORK/basic"
    "${AS_USER[@]}" mkdir -p "$NFSTESTDIR"
    RUN_DIR="$CTHON/basic"
    for t in test1 test2 test3 test4 test5 test6 test7 test8 test9; do
        settle_silly_renames "$NFSTESTDIR"
        result "basic/$t" "./$t" -t
    done

    # general compiles and runs programs on the mount; upstream treats the whole
    # sequence as one test, and so does this.
    export NFSTESTDIR="$WORK/general"
    RUN_DIR="$CTHON/general"
    result "general/all" sh ./runtests -t

    # special: the programs run from a copy on the mount, as upstream's
    # runtests.wrk does, each with the arguments that script gives it.
    export NFSTESTDIR="$WORK/special"
    "${AS_USER[@]}" mkdir -p "$NFSTESTDIR"
    "${AS_USER[@]}" make -s -C "$CTHON/special" copy DESTDIR="$NFSTESTDIR" >/dev/null
    RUN_DIR="$NFSTESTDIR"
    umask 0
    result special/op_unlk env TMPDIR= ./op_unlk
    result special/op_ren env TMPDIR= ./op_ren
    result special/op_chmod env TMPDIR= ./op_chmod
    result special/dupreq ./dupreq 100 testfile
    result special/excltest ./excltest
    result special/negseek ./negseek testfile
    result special/rename ./rename 100
    result special/truncate ./truncate
    result special/holey ./holey
    result special/nfsidem ./nfsidem 50 testdir
    result special/rewind ./rewind
    result special/telldir ./telldir
    result special/freesp ./freesp
    result special/bigfile ./bigfile -s 30 "bigfile$$"
    result special/bigfile2 ./bigfile2 "bigfile2$$"
    umask 022

    # lock: only when locks reach the server. Testing local locks would grade
    # the kernel.
    if option_set_locks "$VARIANT"; then
        export NFSTESTDIR="$WORK/lock"
        "${AS_USER[@]}" mkdir -p "$NFSTESTDIR"
        RUN_DIR="$CTHON/lock"
        result lock/tlocklfs ./tlocklfs -r "$NFSTESTDIR"
        result lock/tlock64 ./tlock64 -r "$NFSTESTDIR"
    else
        echo ""
        echo "lock group skipped: $VARIANT mounts with nolock"
    fi

    cd /
    "${AS_USER[@]}" rm -rf "$WORK"
    echo ""
    echo "CTHON04-DONE $RESULTS"
} 2>&1 | tee "$LOG"
