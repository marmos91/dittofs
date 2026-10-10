#!/usr/bin/env bash
# Fetch (and for cthon04, build) a pinned copy of an upstream NFS test suite.
#
# Usage:
#   ./fetch.sh cthon04|nfstest     # prints the suite's directory on stdout
#
# Both suites are hosted on git.linux-nfs.org, which serves them over git://
# only (https answers 403). Each is pinned to a commit and the checkout is
# verified against it, so a moved branch upstream cannot change what a run
# grades. Override the cache location with NFS_MOUNT_CACHE.
#
# decision: the suites are fetched from upstream, not mirrored or vendored. In
# CI the fetch only happens when the actions cache misses: it is keyed on this
# file, the nightly keeps it warm, and GitHub drops a cache only after a week
# unused. So an outage at git.linux-nfs.org turns a run red only when it
# coincides with a re-pin or a cold cache, for reasons unrelated to DittoFS.
# Mirror both repositories on GitHub, or vendor pinned tarballs, if a nightly
# ever does go red on this fetch.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CACHE="${NFS_MOUNT_CACHE:-${SCRIPT_DIR}/.cache}"

# cthon04 has no tags; the newest commit on master. nfstest's newest tag, v3.2,
# is ten months older than master, which carries only fixes since — so master.
CTHON04_URL="git://git.linux-nfs.org/projects/steved/cthon04.git"
CTHON04_REV="86a4501a6e1e415844dc632894a85a5253cc1505"
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
cthon04)
    dir="$CACHE/cthon04"
    checkout "$CTHON04_URL" "$CTHON04_REV" "$dir"
    # The stamp names the revision it was built from, so a re-pin rebuilds.
    if [[ "$(cat "$dir/.built" 2>/dev/null)" != "$CTHON04_REV" ]]; then
        log "building cthon04"
        make -C "$dir" >"$dir/build.log" 2>&1 || {
            tail -30 "$dir/build.log" >&2
            exit 1
        }
        echo "$CTHON04_REV" >"$dir/.built"
    fi
    echo "$dir"
    ;;
nfstest)
    dir="$CACHE/nfstest"
    checkout "$NFSTEST_URL" "$NFSTEST_REV" "$dir"
    echo "$dir"
    ;;
*)
    echo "usage: $0 cthon04|nfstest" >&2
    exit 2
    ;;
esac
