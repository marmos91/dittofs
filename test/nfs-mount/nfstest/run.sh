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

COUNT=0
{
    echo "nfstest $(git -C "$NFSTEST" rev-parse --short HEAD), option set $VARIANT: vers=$VERSION,$MTOPTS"
    IFS=',' read -ra mods <<<"$MODULES"
    for m in "${mods[@]}"; do
        echo "NFSTEST-MODULE nfstest_$m"
        (cd "$RESULTS_DIR" && PYTHONPATH="$NFSTEST" timeout --kill-after=30 "$MODULE_TIMEOUT" \
            python3 "$NFSTEST/test/nfstest_$m" \
            --server "$SERVER" --port "$NFS_PORT" --export /export \
            --nfsversion "$VERSION" --mtpoint "$MOUNT_POINT" \
            --mtopts "$MTOPTS" --rmtraces --notty)
        echo "nfstest_$m exited $?"
        COUNT=$((COUNT + 1))
        cleanup
    done
    echo "NFSTEST-DONE $COUNT"
} 2>&1 | tee "$LOG"

"$SCRIPT_DIR/parse-results.sh" "$LOG" "$KNOWN_FAILURES" "$RESULTS_DIR"
exit $?
