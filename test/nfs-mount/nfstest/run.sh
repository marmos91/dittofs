#!/usr/bin/env bash
# Run NetApp's nfstest modules against a DittoFS export under a named mount
# option set, then grade the result.
#
# Usage:
#   sudo ./run.sh --variant v4.1 [--modules posix,lock]
#
# Expects a provisioned server (the suite's setup step,
# test/posix/setup-posix.sh --no-mount). nfstest mounts and unmounts the
# export itself for every module, with the option set under test.
#
# The client is a network namespace of its own (see ../netns.sh), and nfstest
# runs as an ordinary user with passwordless sudo (see ../test-user.sh), which
# is how it expects to run. A second namespace, reached over ssh, is the second
# client that the delegation recall tests and nfstest_cache drive. The script
# runs twice: on the host it sets all that up and grades, and inside the
# namespace, re-executed with DITTOFS_CLIENT_NS set, it runs the modules.
#
# Writes one log with each module's output between "NFSTEST-MODULE <module>"
# markers and a final "NFSTEST-DONE <number of modules>", which is what
# parse-results.sh grades.

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
MODULES="${NFSTEST_MODULES:-}"
while [[ $# -gt 0 ]]; do
    case "$1" in
    --variant) VARIANT="${2:?--variant requires a value}"; shift 2 ;;
    --variant=*) VARIANT="${1#*=}"; shift ;;
    --modules) MODULES="${2:?--modules requires a value}"; shift 2 ;;
    --modules=*) MODULES="${1#*=}"; shift ;;
    -h | --help) sed -n '2,22p' "$0"; exit 0 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

[[ -n "$VARIANT" ]] || { echo "--variant is required ($(option_set_names | tr '\n' ' '))" >&2; exit 2; }
option_set_opts "$VARIANT" >/dev/null || { echo "unknown option set: $VARIANT" >&2; exit 2; }
[[ $EUID -eq 0 ]] || { echo "must run as root (nfstest mounts and captures packets)" >&2; exit 2; }

NFS_PORT="${NFS_PORT:-12049}"
# The export and the MOUNT port default to what DittoFS serves; baseline-knfsd.sh
# overrides both to run the same modules against the kernel server, whose
# mountd is found through rpcbind (an empty NFSTEST_MOUNTPORT).
NFSTEST_EXPORT="${NFSTEST_EXPORT:-/export}"
NFSTEST_MOUNTPORT="${NFSTEST_MOUNTPORT-$NFS_PORT}"
MOUNT_POINT="${DITTOFS_MOUNT:-/tmp/dittofs-test}"
RESULTS_DIR="${DITTOFS_RESULTS_DIR:-$(mktemp -d)}"
MODULE_TIMEOUT="${NFSTEST_MODULE_TIMEOUT:-1200}"
LOG="$RESULTS_DIR/nfstest.log"
VERSION="$(option_set_version "$VARIANT")"

# The known-failure tables come from the caller in DITTOFS_KNOWN_FAILURES:
# baseline-knfsd.sh says which tables grade which option set.
KNOWN_FAILURES="${DITTOFS_KNOWN_FAILURES:-}"
[[ -n "$KNOWN_FAILURES" ]] || {
    echo "no known-failure tables: run through baseline-knfsd.sh, or set DITTOFS_KNOWN_FAILURES" >&2
    exit 2
}

# The modules that apply to this option set, unless --modules narrowed them.
if [[ -z "$MODULES" ]]; then
    MODULES="posix,dio"
    # nfstest_cache times attribute caching against a change from the second
    # client. On NFSv4 that change recalls the first client's delegation, which
    # makes it revalidate at once — correct, and fatal to every timing check
    # the module makes. So it runs on NFSv3 only.
    [[ "$VERSION" == 3 ]] && MODULES+=",cache"
    # lock is the slowest module after dio and found nothing that varies with
    # caching or I/O size, so it runs on the plain sets of each protocol only.
    if option_set_locks "$VARIANT" && [[ "$VARIANT" == v3 || "$VARIANT" == v4.0 || "$VARIANT" == v4.1 ]]; then
        MODULES+=",lock"
    fi
    [[ "$VERSION" != 3 ]] && MODULES+=",delegation"
fi

SERVER="$NS_SERVER_ADDR"
CLIENT2="$(ns_addr 2)"

# nfstest takes the version and the port on flags of their own. NFSv3 also
# needs the MOUNT protocol's port, which DittoFS serves on the same one.
MTOPTS="hard"
extra="$(option_set_extra_opts "$VARIANT")"
[[ -n "$extra" ]] && MTOPTS+=",$extra"
[[ "$VERSION" == 3 && -n "$NFSTEST_MOUNTPORT" ]] && MTOPTS+=",mountport=$NFSTEST_MOUNTPORT"

if [[ -z "${DITTOFS_CLIENT_NS:-}" ]]; then
    FETCHED="$("$SUITE_DIR/fetch.sh" nfstest)" || { echo "could not fetch nfstest" >&2; exit 2; }
    # A fresh private directory, not a fixed path another user could have
    # prepared; it is opened to reading only once it is this script's own.
    WORKBASE="$(mktemp -d /tmp/dittofs-nfs-mount.XXXXXX)" || exit 2
    chmod 0755 "$WORKBASE"
    NFSTEST="$WORKBASE/nfstest"
    SSHD_DIR="$WORKBASE/sshd"
    teardown() {
        client_ns_down_all
        test_user_release
        rm -rf "$WORKBASE"
    }
    trap teardown EXIT
    client_ns_down_all
    test_user_ensure || { echo "could not create the test user" >&2; exit 2; }
    test_user_copy "$FETCHED" "$NFSTEST" || { echo "could not copy nfstest" >&2; exit 2; }
    client_ns_up 1 || exit 2
    client_ns_up 2 || exit 2
    client_ns_mirror 2 1 || { echo "could not mirror the second client's traffic" >&2; exit 2; }

    # The second client: sshd in namespace 2, which nfstest reaches as the
    # test user. Every command it runs there, mount included, runs in that
    # namespace.
    sshd_config="$(test_user_sshd_config "$SSHD_DIR" "$CLIENT2")" || exit 2
    client_ns_exec 2 "$(command -v sshd || echo /usr/sbin/sshd)" -D -e -f "$sshd_config" \
        >"$RESULTS_DIR/sshd-client2.log" 2>&1 &
    for _ in $(seq 1 20); do
        nc -z -w 1 "$CLIENT2" 22 2>/dev/null && break
        sleep 0.5
    done
    nc -z -w 1 "$CLIENT2" 22 2>/dev/null || { echo "sshd on the second client never came up" >&2; exit 2; }

    client_ns_exec 1 env DITTOFS_CLIENT_NS=1 NFSTEST="$NFSTEST" DITTOFS_RESULTS_DIR="$RESULTS_DIR" \
        NFSTEST_MODULES="$MODULES" "$SCRIPT_DIR/run.sh" "${ARGS[@]}" ||
        echo "the client side exited $?" >&2

    # The teardown step deletes the server log; keep it with the results.
    gzip -c /tmp/dittofs-posix-server.log >"$RESULTS_DIR/server.log.gz" 2>/dev/null || true

    # Not exec: the EXIT trap has to run to tear the namespaces down.
    "$SCRIPT_DIR/parse-results.sh" "$LOG" "$KNOWN_FAILURES" "$RESULTS_DIR"
    exit $?
fi

# From here on this runs inside the client namespace.

# nfstest runs from a directory the test user can write; the results directory
# belongs to whoever started the suite.
WORKDIR="$(mktemp -d /tmp/dittofs-nfstest-cwd.XXXXXX)"
chown "$TEST_USER_NAME:$TEST_USER_NAME" "$WORKDIR"

mkdir -p "$MOUNT_POINT"
detach "$MOUNT_POINT"

cleanup() {
    # A module killed by its timeout never stops its packet capture, nor ends
    # what it started on the second client: its ssh sessions, its remote
    # procedure server there, and that client's mount, which lives in the
    # second namespace and is reachable only through it.
    pkill -f 'tcpdump .*nfstest_' 2>/dev/null || true
    pkill -u "$TEST_USER_NAME" -x ssh 2>/dev/null || true
    sudo -u "$TEST_USER_NAME" -H timeout 30 ssh "$CLIENT2" \
        "sudo pkill -u $TEST_USER_NAME -x python3; sudo umount -f -l $MOUNT_POINT" >/dev/null 2>&1 || true
    detach "$MOUNT_POINT"
}
trap 'cleanup; rm -rf "$WORKDIR"' EXIT

# module_args MODULE — the arguments one module needs beyond the common ones.
module_args() {
    case "$1" in
    delegation)
        # The recall tests need a second client. Without a port it mounts
        # 2049, and every recall waits out a refused mount.
        echo "--client $CLIENT2:port=$NFS_PORT"
        ;;
    cache)
        echo "--client $CLIENT2"
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
    echo "nfstest $(git -c safe.directory="*" -C "$NFSTEST" rev-parse --short HEAD), option set $VARIANT: vers=$VERSION,$MTOPTS"
    echo "client $(ns_addr 1), second client $CLIENT2, run as $TEST_USER_NAME (uid $TEST_USER_UID)"
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
        # shellcheck disable=SC2046  # module_args is a word list on purpose
        # timeout runs inside sudo, as the test user: sudo cannot pass on a
        # SIGKILL, so from outside it would end sudo and leave a module stuck
        # on a hung mount running.
        (cd "$WORKDIR" && sudo -u "$TEST_USER_NAME" -H \
            timeout --kill-after=30 "$MODULE_TIMEOUT" env PYTHONPATH="$NFSTEST" \
            python3 "$NFSTEST/test/nfstest_$m" \
            --server "$SERVER" --port "$NFS_PORT" --export "$NFSTEST_EXPORT" \
            --nfsversion "$VERSION" --mtpoint "$MOUNT_POINT" \
            --mtopts "$MTOPTS" --rmtraces --notty $(module_args "$m"))
        echo "nfstest_$m exited $?"
        COUNT=$((COUNT + 1))
        cleanup
    done
    echo "NFSTEST-DONE $COUNT"
} 2>&1 | tee "$LOG"
