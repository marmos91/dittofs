#!/usr/bin/env bash
# Run the Connectathon (cthon04) suite against a DittoFS export mounted with a
# named option set, then grade the result.
#
# Usage:
#   sudo ./run.sh --variant v4.1 [--keep-mount]
#
# Expects a provisioned server (../setup.sh). Mounts the export itself, so the
# option set under test is the only one in play, and unmounts on exit.
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
# shellcheck source=../mounts.sh
source "$SUITE_DIR/mounts.sh"

VARIANT=""
KEEP_MOUNT=false
while [[ $# -gt 0 ]]; do
    case "$1" in
    --variant) VARIANT="${2:?--variant requires a value}"; shift 2 ;;
    --variant=*) VARIANT="${1#*=}"; shift ;;
    --keep-mount) KEEP_MOUNT=true; shift ;;
    -h | --help) sed -n '2,20p' "$0"; exit 0 ;;
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

if [[ "$(option_set_version "$VARIANT")" == 3 ]]; then
    KNOWN_FAILURES="$SCRIPT_DIR/KNOWN_FAILURES_V3.md"
else
    KNOWN_FAILURES="$SCRIPT_DIR/KNOWN_FAILURES_V4.md"
fi

CTHON="$("$SUITE_DIR/fetch.sh" cthon04)" || { echo "could not fetch cthon04" >&2; exit 2; }

MOUNT_OPTS="$(option_set_mount_opts "$VARIANT" "$NFS_PORT")"
mkdir -p "$MOUNT_POINT"
detach "$MOUNT_POINT"
echo "mount -t nfs -o $MOUNT_OPTS localhost:/export $MOUNT_POINT"
timeout 60 mount -t nfs -o "$MOUNT_OPTS" localhost:/export "$MOUNT_POINT" || {
    echo "mount failed" >&2
    exit 2
}
# Without this a failed mount leaves an empty local directory behind, and the
# whole suite passes against the runner's own disk.
is_mounted "$MOUNT_POINT" || { echo "$MOUNT_POINT is not a mount point" >&2; exit 2; }

unmount() {
    [[ "$KEEP_MOUNT" == true ]] && return
    cd /
    detach "$MOUNT_POINT"
}
trap unmount EXIT

# What the kernel actually negotiated, which is not always what was asked for.
nfsstat -m 2>/dev/null | grep -A1 -F "$MOUNT_POINT" | tee "$RESULTS_DIR/mount-options.txt"

WORK="$MOUNT_POINT/cthon04-$$"
rm -rf "$WORK"
mkdir -p "$WORK"

RESULTS=0
# result NAME CMD... — run CMD under the per-test timeout and record its verdict.
result() {
    local name="$1" status verdict
    shift
    echo ""
    echo "=== $name"
    timeout --kill-after=30 "$TEST_TIMEOUT" "$@"
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
    echo "cthon04 $(git -C "$CTHON" rev-parse --short HEAD), option set $VARIANT: $MOUNT_OPTS"

    # basic: one directory for the group, because test2 removes the tree test1
    # builds. A failure does not stop the group.
    export NFSTESTDIR="$WORK/basic"
    mkdir -p "$NFSTESTDIR"
    cd "$CTHON/basic"
    for t in test1 test2 test3 test4 test5 test6 test7 test8 test9; do
        result "basic/$t" "./$t" -t
    done

    # general compiles and runs programs on the mount; upstream treats the whole
    # sequence as one test, and so does this.
    export NFSTESTDIR="$WORK/general"
    cd "$CTHON/general"
    result "general/all" sh ./runtests -t

    # special: the programs run from a copy on the mount, as upstream's
    # runtests.wrk does, each with the arguments that script gives it.
    export NFSTESTDIR="$WORK/special"
    mkdir -p "$NFSTESTDIR"
    make -s -C "$CTHON/special" copy DESTDIR="$NFSTESTDIR" >/dev/null
    cd "$NFSTESTDIR"
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

    # lock: only when locks reach the server — see option-sets.sh for why the
    # NFSv3 sets mount nolock. Testing local locks would grade the kernel.
    if option_set_locks "$VARIANT"; then
        export NFSTESTDIR="$WORK/lock"
        mkdir -p "$NFSTESTDIR"
        cd "$CTHON/lock"
        result lock/tlocklfs ./tlocklfs -r "$NFSTESTDIR"
        result lock/tlock64 ./tlock64 -r "$NFSTESTDIR"
    else
        echo ""
        echo "lock group skipped: $VARIANT mounts with nolock"
    fi

    cd /
    rm -rf "$WORK"
    echo ""
    echo "CTHON04-DONE $RESULTS"
} 2>&1 | tee "$LOG"

# Not exec: the EXIT trap has to run to unmount.
"$SCRIPT_DIR/parse-results.sh" "$LOG" "$KNOWN_FAILURES" "$RESULTS_DIR"
exit $?
