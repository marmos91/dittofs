#!/usr/bin/env bash
# Run the cthon04 / nfstest conformance suites from a machine that is not a
# Linux host — a Mac with Docker, say — inside a privileged Ubuntu container
# that stands in for the CI runner. CI does not use this: it runs the same
# test/conformance/run.sh steps directly on the CI host.
#
# Usage:
#   test/nfs-mount/dev-docker.sh <suite> [profile] [variant]
#   test/nfs-mount/dev-docker.sh cthon04 memory v4.1
#   test/nfs-mount/dev-docker.sh nfstest memory v3
#   test/nfs-mount/dev-docker.sh shell              # a shell in the container
#
# The container uses the Docker VM's kernel NFS client, so it loads the nfs
# modules from the VM's /lib/modules. The checkout is copied in rather than
# bind-mounted, because the suites build dfs and dfsctl at the repo root and a
# Linux build would otherwise replace the host's binaries. Results land in
# test/nfs-mount/results/<suite>/<profile>-<variant>/.
#
# NFSTEST_MODULES (e.g. "posix,dio") is passed through to narrow an nfstest run.
#
# Only the memory and badger profiles work here; the -s3 profiles need the
# localstack service CI provides.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"
IMAGE="dittofs-nfs-mount-dev"
NAME="dittofs-nfs-mount-dev-$$"
RESULTS="$SCRIPT_DIR/results"

SUITE="${1:-}"
PROFILE="${2:-memory}"
VARIANT="${3:-v4.1}"
[[ -n "$SUITE" ]] || { sed -n '2,20p' "$0"; exit 2; }

# Every container shares the Docker VM's one kernel, so two runs are not
# isolated: a sync() in one waits on the other's NFS writeback, and a server
# that hangs in one run stalls the other. Run them one at a time.
if docker ps --format '{{.Names}}' | grep -q '^dittofs-nfs-mount-dev-'; then
    echo "another dev-docker.sh run is active; they share a kernel, so run one at a time" >&2
    exit 2
fi

docker build -q -t "$IMAGE" -f "$SCRIPT_DIR/Dockerfile.dev" "$SCRIPT_DIR" >/dev/null

mkdir -p "$RESULTS"
# Named volumes keep the Go caches and the fetched suites between runs.
docker run -d --rm --privileged --name "$NAME" \
    -v /lib/modules:/lib/modules:ro \
    -v dittofs-nfs-mount-gocache:/root/.cache \
    -v dittofs-nfs-mount-gomod:/root/go/pkg/mod \
    -v dittofs-nfs-mount-suites:/suites \
    -v "$RESULTS":/results \
    -e NFS_MOUNT_CACHE=/suites \
    -e NFSTEST_MODULES="${NFSTEST_MODULES:-}" \
    -w /work "$IMAGE" sleep infinity >/dev/null
trap 'docker rm -f "$NAME" >/dev/null 2>&1' EXIT

docker exec "$NAME" sh -c '
    modprobe nfs && modprobe nfsv4 || { echo "cannot load the nfs kernel modules" >&2; exit 1; }
    mkdir -p /run/rpc_pipefs && mount -t rpc_pipefs rpc_pipefs /run/rpc_pipefs
    # A runner host starts statd on demand through systemd the first time an
    # NFSv3 mount asks for locking; a container has no systemd. nfstest_dio
    # remounts with its own options, without nolock, and needs it.
    rpcbind && rpc.statd
    mkdir -p /work'

# Tracked files plus new ones not yet committed, minus anything ignored.
(cd "$REPO_ROOT" && git ls-files -co --exclude-standard -z | COPYFILE_DISABLE=1 tar -c --no-xattrs --no-mac-metadata --null -T - -f -) |
    docker exec -i "$NAME" tar -x -C /work

if [[ "$SUITE" == shell ]]; then
    docker exec -it "$NAME" bash
else
    docker exec "$NAME" test/conformance/run.sh \
        --suite "$SUITE" --profile "$PROFILE" --variant "$VARIANT" --results-dir /results
fi
