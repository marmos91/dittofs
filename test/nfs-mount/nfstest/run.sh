#!/usr/bin/env bash
# Run NetApp's nfstest modules against a DittoFS export under a named mount
# option set, then grade the result.
#
# Usage:
#   sudo ./run.sh --variant v4.1 [--modules posix,lock]
#
# Expects a provisioned server (../setup.sh). nfstest mounts and unmounts the
# export itself for every module, with the option set under test.
#
# Writes one log with each module's output between "NFSTEST-MODULE <module>"
# markers and a final "NFSTEST-DONE <number of modules>", which is what
# parse-results.sh grades.

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
SUITE_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"
# shellcheck source=../option-sets.sh
source "$SUITE_DIR/option-sets.sh"
# shellcheck source=../mounts.sh
source "$SUITE_DIR/mounts.sh"

VARIANT=""
MODULES="${NFSTEST_MODULES:-}"
while [[ $# -gt 0 ]]; do
    case "$1" in
    --variant) VARIANT="${2:?--variant requires a value}"; shift 2 ;;
    --variant=*) VARIANT="${1#*=}"; shift ;;
    --modules) MODULES="${2:?--modules requires a value}"; shift 2 ;;
    --modules=*) MODULES="${1#*=}"; shift ;;
    -h | --help) sed -n '2,14p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

[[ -n "$VARIANT" ]] || { echo "--variant is required ($(option_set_names | tr '\n' ' '))" >&2; exit 2; }
option_set_opts "$VARIANT" >/dev/null || { echo "unknown option set: $VARIANT" >&2; exit 2; }
[[ $EUID -eq 0 ]] || { echo "must run as root (nfstest mounts and captures packets)" >&2; exit 2; }

NFS_PORT="${NFS_PORT:-12049}"
MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
RESULTS_DIR="${DITTOFS_RESULTS_DIR:-$(mktemp -d)}"
MODULE_TIMEOUT="${NFSTEST_MODULE_TIMEOUT:-1200}"
LOG="$RESULTS_DIR/nfstest.log"
VERSION="$(option_set_version "$VARIANT")"

if [[ "$VERSION" == 3 ]]; then
    KNOWN_FAILURES="$SCRIPT_DIR/KNOWN_FAILURES_V3.md"
elif [[ "$VERSION" == 4.0 ]]; then
    # v4.0 is graded against the shared v4 table plus its own, so a row only
    # v4.0 needs cannot excuse the same failure on a v4.1 set.
    KNOWN_FAILURES="$RESULTS_DIR/known-failures.md"
    cat "$SCRIPT_DIR/KNOWN_FAILURES_V4.md" "$SCRIPT_DIR/KNOWN_FAILURES_V40.md" >"$KNOWN_FAILURES"
else
    KNOWN_FAILURES="$SCRIPT_DIR/KNOWN_FAILURES_V4.md"
fi

# The modules that apply to this option set, unless --modules narrowed them.
if [[ -z "$MODULES" ]]; then
    # decision: nfstest_cache is left out. It needs a second client host: the
    # "remote" client it drives gets the same mount point and options as the
    # local one, so on a single host the two mounts collide. Add it when the
    # suite gains a second client (a network namespace or another runner).
    MODULES="posix,dio"
    # See option-sets.sh: under nolock the locks never leave the client.
    option_set_locks "$VARIANT" && MODULES+=",lock"
    [[ "$VERSION" != 3 ]] && MODULES+=",delegation"
fi

NFSTEST="$("$SUITE_DIR/fetch.sh" nfstest)" || { echo "could not fetch nfstest" >&2; exit 2; }

# The server is this host, addressed by its primary IP rather than localhost.
# nfstest checks many assertions against a packet capture, and with 127.0.0.1
# at both ends it cannot match the client's calls to the server: every "OPEN
# should be sent" style check fails although the capture holds the call. With
# the primary address it captures on loopback all the same, and matches.
SERVER="${NFSTEST_SERVER:-$(hostname -I | awk '{print $1}')}"
[[ -n "$SERVER" ]] || { echo "cannot determine this host's address; set NFSTEST_SERVER" >&2; exit 2; }

# nfstest takes the version and the port on flags of their own. NFSv3 also
# needs the MOUNT protocol's port, which DittoFS serves on the same one.
MTOPTS="hard"
extra="$(option_set_extra_opts "$VARIANT")"
[[ -n "$extra" ]] && MTOPTS+=",$extra"
[[ "$VERSION" == 3 ]] && MTOPTS+=",mountport=$NFS_PORT"

mkdir -p "$MOUNT_POINT"
detach "$MOUNT_POINT"

cleanup() {
    # nfstest leaves its packet capture running when a module dies mid-test.
    pkill -f 'tcpdump .*nfstest_' 2>/dev/null || true
    detach "$MOUNT_POINT"
}
trap cleanup EXIT

# --tbsize sizes the kernel buffer of every packet capture. nfstest's default is
# 192 MiB; with the captures it orphans (see reap_orphan_captures) a dio run
# once held over thirty at once, and the host ran out of memory until the OOM
# killer took dfs. The reaper ends the leak; the smaller buffer bounds what an
# orphan costs in the two seconds before the reaper finds it.

# reap_orphan_captures — kill nfstest's packet captures that nfstest has let go
# of, every two seconds, until killed. nfstest starts each tcpdump through
# `sh -c` and stops it by killing that shell, which orphans the tcpdump rather
# than ending it. Every orphan goes on capturing all loopback traffic: within
# one dio module they reach dozens, a new capture then starts too slowly to see
# its test's traffic, and every packet-checking assertion after that fails. A
# capture is kept while its parent is still nfstest's shell or nfstest itself.
reap_orphan_captures() {
    local pid ppid parent
    while sleep 2; do
        for pid in $(pgrep -f 'tcpdump .*nfstest_' 2>/dev/null); do
            ppid="$(ps -o ppid= -p "$pid" 2>/dev/null | tr -d ' ')"
            [[ -n "$ppid" ]] || continue
            parent="$(ps -o args= -p "$ppid" 2>/dev/null)"
            [[ "$parent" == *tcpdump* || "$parent" == *nfstest_* ]] || kill "$pid" 2>/dev/null
        done
    done
}

# module_args MODULE — the arguments one module needs beyond the common ones.
module_args() {
    case "$1" in
    delegation)
        # The recall tests mount a second client, which takes its port from
        # --client and otherwise tries 2049: every recall then waits out a
        # refused mount and the module runs past its timeout.
        echo "--client port=$NFS_PORT"
        ;;
    dio)
        # dio checks every assertion against a packet capture it stops right
        # before reading. The default two seconds for tcpdump to flush is not
        # always enough on a busy runner, and an empty capture reads as a
        # failed test.
        echo "--trcdelay 5"
        ;;
    esac
}

COUNT=0
{
    echo "nfstest $(git -C "$NFSTEST" rev-parse --short HEAD), option set $VARIANT: vers=$VERSION,$MTOPTS"
    IFS=',' read -ra mods <<<"$MODULES"
    for m in "${mods[@]}"; do
        echo "NFSTEST-MODULE nfstest_$m"
        # A module against a server that has died waits out its whole timeout.
        # Skip it instead: with no closing tally it grades as "did not finish",
        # which is what it is, and the job ends long before its own timeout.
        if ! nc -z -w 5 "$SERVER" "$NFS_PORT" 2>/dev/null; then
            echo "skipped: nothing answers on $SERVER:$NFS_PORT any more"
            COUNT=$((COUNT + 1))
            continue
        fi
        reap_orphan_captures &
        reaper=$!
        # shellcheck disable=SC2046  # module_args is a word list on purpose
        (cd "$RESULTS_DIR" && PYTHONPATH="$NFSTEST" timeout --kill-after=30 "$MODULE_TIMEOUT" \
            python3 "$NFSTEST/test/nfstest_$m" \
            --server "$SERVER" --port "$NFS_PORT" --export /export \
            --nfsversion "$VERSION" --mtpoint "$MOUNT_POINT" \
            --mtopts "$MTOPTS" --tbsize 8k --rmtraces --notty $(module_args "$m"))
        echo "nfstest_$m exited $?"
        kill "$reaper" 2>/dev/null
        wait "$reaper" 2>/dev/null
        COUNT=$((COUNT + 1))
        cleanup
    done
    echo "NFSTEST-DONE $COUNT"
} 2>&1 | tee "$LOG"

# The teardown step deletes the server log; keep it with the results, which
# CI uploads.
gzip -c /tmp/dittofs-posix-server.log >"$RESULTS_DIR/server.log.gz" 2>/dev/null || true

"$SCRIPT_DIR/parse-results.sh" "$LOG" "$KNOWN_FAILURES" "$RESULTS_DIR"
exit $?
