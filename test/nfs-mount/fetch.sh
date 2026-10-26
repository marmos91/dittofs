#!/usr/bin/env bash
# Fetch a pinned copy of nfstest, for the knfsd baseline.
#
# Usage:
#   ./fetch.sh nfstest     # prints its directory on stdout
#
# git.linux-nfs.org serves it over git:// only (https answers 403). It is
# pinned to a commit and the checkout is verified against it, so a moved
# branch upstream cannot change what a run grades. Override the cache location
# with NFS_MOUNT_CACHE.
#
# decision: fetched from upstream, not mirrored or vendored. In CI the fetch
# only happens when the actions cache misses, which is keyed on this file, so
# an outage at git.linux-nfs.org fails a baseline run only when it coincides
# with a re-pin or a cold cache.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE="${NFS_MOUNT_CACHE:-${SCRIPT_DIR}/.cache}"

# nfstest's newest tag, v3.2, is ten months older than master, which carries
# only fixes since, so master.
NFSTEST_URL="git://git.linux-nfs.org/projects/mora/nfstest.git"
NFSTEST_REV="94089545caa18bbe4be1b055125d0721fe8dee80"

log() { echo "[fetch] $*" >&2; }

# checkout URL REV DIR — DIR at REV, cloning only when it is missing or wrong.
checkout() {
    local url="$1" rev="$2" dir="$3"
    if [[ -d "$dir/.git" && "$(git -C "$dir" rev-parse HEAD 2>/dev/null)" == "$rev" ]]; then
        return 0
    fi
    log "cloning $url at ${rev:0:12}"
    rm -rf "$dir"
    git clone --quiet "$url" "$dir"
    git -C "$dir" -c advice.detachedHead=false checkout --quiet "$rev"
    [[ "$(git -C "$dir" rev-parse HEAD)" == "$rev" ]] || {
        log "checkout of $url is not at $rev"
        exit 1
    }
}

mkdir -p "$CACHE"

case "${1:-}" in
nfstest)
    dir="$CACHE/nfstest"
    checkout "$NFSTEST_URL" "$NFSTEST_REV" "$dir"
    echo "$dir"
    ;;
*)
    echo "usage: $0 nfstest" >&2
    exit 2
    ;;
esac
