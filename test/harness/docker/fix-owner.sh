#!/usr/bin/env bash
# fix-owner.sh PATH...: hand what a root container created under PATHs back to the host
# user, DT_HOST_UID:DT_HOST_GID. A Linux Docker Engine keeps uid 0 on bind-mounted
# files, so a run would leave root-owned logs, results and git index entries in the
# user's checkout. Docker Desktop maps ownership itself, so dtc and dt only set
# DT_FIX_OWNER=1 for a Linux engine. -xdev: never descend into another file system,
# such as an NFS mount a test left behind.
[[ "${DT_FIX_OWNER:-}" == 1 && -n "${DT_HOST_UID:-}" ]] || exit 0
for p in "$@"; do
    [[ -e "$p" ]] || continue
    find "$p" -xdev -uid 0 -print0 2>/dev/null |
        xargs -0 -r chown -h "$DT_HOST_UID:${DT_HOST_GID:-$DT_HOST_UID}" 2>/dev/null || true
done
exit 0
