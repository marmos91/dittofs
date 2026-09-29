# shellcheck shell=bash
# Shared by the repro scripts (source it): portable NFS mount and unmount, hashing,
# and the lock label dt uses, for macOS and Linux (natively or in dtc).
# Linux needs root to mount: directly as root (dtc), otherwise through sudo. macOS
# mounts a user-owned mount point without it.
REPRO_OS="$(uname -s)"
as_root() { if [[ "$(id -u)" == 0 ]]; then "$@"; else sudo "$@"; fi; }
nfs3_mount() { # nfs3_mount HOST:PATH MOUNTPOINT PORT
    if [[ "$REPRO_OS" == Darwin ]]; then
        mount -t nfs -o "vers=3,tcp,port=$3,mountport=$3,nolocks,noresvport" "$1" "$2"
    else
        as_root mount -t nfs -o "vers=3,tcp,port=$3,mountport=$3,nolock" "$1" "$2"
    fi
}
nfs_umount() { # nfs_umount MOUNTPOINT: a plain unmount, forced if that fails
    if [[ "$REPRO_OS" == Darwin ]]; then
        umount "$1" 2>/dev/null || diskutil unmount force "$1" >/dev/null 2>&1
    else
        as_root umount "$1" 2>/dev/null || as_root umount -f -l "$1" 2>/dev/null
    fi
}
is_mounted() { mount | awk -v m="$1" '$3 == m {f = 1} END {exit !f}'; }
sha16() { { shasum -a 256 "$1" 2>/dev/null || sha256sum "$1"; } | cut -c1-16; }
# Where a lock is taken, as dt records it: pids are only checked where they live.
dt_where() {
    if [[ "${DT_IN_CONTAINER:-}" == 1 ]]; then echo "container:${DT_CONTAINER_NAME:-$(hostname)}"; else echo host; fi
}
