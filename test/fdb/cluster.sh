#!/usr/bin/env bash
# A local FoundationDB for the fdbkv backend's tests: the client library, a
# cluster in containers, and go test pointed at both.
#
#   test/fdb/cluster.sh client      download libfdb_c and its headers
#   test/fdb/cluster.sh up [1|3]    start N fdbserver processes (default 1); three
#                                   run double replication with three coordinators
#   test/fdb/cluster.sh down        stop and remove them, and their data
#   test/fdb/cluster.sh kill N      kill process N (1-based) without a clean stop
#   test/fdb/cluster.sh start N     start a killed process N again
#   test/fdb/cluster.sh test ...    go test from the repository root, with the
#                                   client library on the cgo paths and
#                                   DITTOFS_FDB_CLUSTER_FILE set
#
# Host networking, so the cluster file's 127.0.0.1 addresses are the ones the
# test client dials. That works on Linux only; on macOS run it inside a Linux
# VM. The client library is x86_64 only here. Docker or podman.
set -euo pipefail

VERSION="${FDB_VERSION:-7.3.77}"
RUNTIME="${CONTAINER_RUNTIME:-$(command -v docker || command -v podman)}"
# On disk, not under /tmp: /tmp is a RAM disk on many Linux hosts, which hides
# the cost of every commit's sync.
BASE="${DITTOFS_FDB_DIR:-$HOME/.cache/dittofs-fdb}"
DATA="$BASE/data"
CLIENT="$BASE/client-$VERSION"
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
IMAGE="docker.io/foundationdb/foundationdb:$VERSION"

client() {
    local base="https://github.com/apple/foundationdb/releases/download/$VERSION"
    mkdir -p "$CLIENT/lib" "$CLIENT/include/foundationdb"
    curl -fsSL -o "$CLIENT/lib/libfdb_c.so" "$base/libfdb_c.x86_64.so"
    curl -fsSL "$base/fdb-headers-$VERSION.tar.gz" | tar -xz -C "$CLIENT/include/foundationdb"
    # fdb_c.h includes fdb_c_types.h, which the release's header tarball leaves out.
    curl -fsSL -o "$CLIENT/include/foundationdb/fdb_c_types.h" \
        "https://raw.githubusercontent.com/apple/foundationdb/$VERSION/bindings/c/foundationdb/fdb_c_types.h"
    echo "client $VERSION in $CLIENT"
}

process_up() {
    local n=$1 port=$((4499 + $1))
    mkdir -p "$DATA/p$n"
    "$RUNTIME" run -d --name "dittofs-fdb$n" --network host \
        -v "$DATA:/cluster" --entrypoint /usr/bin/fdbserver "$IMAGE" \
        --cluster-file /cluster/fdb.cluster --public-address "127.0.0.1:$port" \
        --listen-address "127.0.0.1:$port" --datadir "/cluster/p$n" --logdir "/cluster/p$n" \
        --locality-zoneid "z$n" --locality-machineid "m$n" >/dev/null
}

fdbcli() {
    "$RUNTIME" exec dittofs-fdb1 /usr/bin/fdbcli -C /cluster/fdb.cluster --exec "$1"
}

up() {
    local n=$1 coords="" mode=single
    for i in $(seq 1 "$n"); do coords+="${coords:+,}127.0.0.1:$((4499 + i))"; done
    # double, not triple: triple places logs in three zones, so three processes
    # cannot lose one. double keeps two durable copies and survives one loss.
    [[ "$n" -ge 3 ]] && mode=double
    echo "dittofs:dittofs@$coords" >"$DATA/fdb.cluster"
    for i in $(seq 1 "$n"); do process_up "$i"; done
    sleep 2
    fdbcli "configure new $mode ssd" >/dev/null
    for _ in $(seq 1 60); do
        if fdbcli "status minimal" 2>/dev/null | grep -q "available"; then
            echo "FoundationDB $VERSION up: $n process(es), $mode replication; cluster file $DATA/fdb.cluster"
            return 0
        fi
        sleep 1
    done
    fdbcli "status" >&2 || true
    return 1
}

# Created here, as the user: a bind mount of a missing directory would have the
# container runtime create it as root, and nothing after could write to it.
mkdir -p "$DATA"

case "${1:-}" in
client) client ;;
up) up "${2:-1}" ;;
down)
    for i in 1 2 3; do "$RUNTIME" rm -f "dittofs-fdb$i" >/dev/null 2>&1 || true; done
    # The processes may write as root; remove the data from inside a container.
    "$RUNTIME" run --rm -v "$DATA:/d" docker.io/library/busybox:1.37 sh -c 'rm -rf /d/*' >/dev/null 2>&1 || true
    ;;
kill) "$RUNTIME" kill -s KILL "dittofs-fdb${2:?process number}" >/dev/null ;;
start) "$RUNTIME" start "dittofs-fdb${2:?process number}" >/dev/null ;;
test)
    shift
    [[ -f "$CLIENT/lib/libfdb_c.so" ]] || client
    export CGO_CFLAGS="-I$CLIENT/include" CGO_LDFLAGS="-L$CLIENT/lib"
    export LD_LIBRARY_PATH="$CLIENT/lib${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
    export DITTOFS_FDB_CLUSTER_FILE="${DITTOFS_FDB_CLUSTER_FILE:-$DATA/fdb.cluster}"
    cd "$ROOT" && exec go test "$@"
    ;;
*)
    sed -n '2,17p' "$0"
    exit 2
    ;;
esac
