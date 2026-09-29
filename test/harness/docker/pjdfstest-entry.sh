#!/usr/bin/env bash
# Runs inside the pjdfstest container: mount the host dfs export with the same
# options test/posix/setup-posix.sh uses, then hand over to the repo's run-posix.sh.
#   pjdfstest-entry.sh <nfs-version> [run-posix.sh args...]
set -euo pipefail
VER="${1:?nfs version}"; shift
HOST="${DFS_HOST:-host.docker.internal}"
PORT="${NFS_PORT:-12049}"
MNT=/tmp/dittofs-test
case "$VER" in
    3)   OPTS="nfsvers=3,tcp,port=$PORT,mountport=$PORT,nolock,noac,sync,lookupcache=none" ;;
    4|4.0) VER=4.0; OPTS="vers=4.0,port=$PORT,noac,sync,lookupcache=none" ;;
    4.1) OPTS="vers=4.1,port=$PORT,noac,sync,lookupcache=none" ;;
    *) echo "unknown NFS version: $VER" >&2; exit 2 ;;
esac
mkdir -p "$MNT"
echo "[pjdfstest] mount -t nfs -o $OPTS $HOST:/export $MNT"
timeout 60 mount -t nfs -o "$OPTS" "$HOST:/export" "$MNT"
# Unmount, then hand the results run-posix.sh wrote back to the caller (a no-op
# unless DT_FIX_OWNER=1, i.e. on a Linux engine).
trap 'umount -f "$MNT" 2>/dev/null || umount -l "$MNT" 2>/dev/null || true; /fix-owner.sh /repo/test/posix/results 2>/dev/null || true' EXIT
rc=0
DITTOFS_MOUNT="$MNT" /repo/test/posix/run-posix.sh --nfs-version "$VER" "$@" || rc=$?
exit "$rc"
