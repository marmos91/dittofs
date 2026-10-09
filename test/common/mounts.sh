#!/usr/bin/env bash
# Mount-point helpers that cannot hang on a dead server. Sourced, not executed.
#
# A `hard` NFS mount whose server has stopped answering blocks every stat() of
# it forever, in an uninterruptible wait that no signal ends. `mountpoint -q`
# stats the path, so a cleanup that asks it whether there is anything to
# unmount hangs exactly when there is — and the CI job then dies at its timeout
# with no verdict, instead of grading the hang as the failure it is.

# is_mounted PATH — true when PATH is a mount point. Reads /proc/mounts and never
# touches PATH itself.
is_mounted() {
    awk -v p="$1" '$2 == p { found = 1 } END { exit !found }' /proc/mounts
}

# detach PATH — unmount PATH if it is mounted, without waiting on its server.
# The lazy flag detaches the mount from the namespace at once even while
# requests on it are stuck; the time limit covers a umount(8) that stats first.
detach() {
    is_mounted "$1" || return 0
    timeout 30 umount -f -l "$1" 2>/dev/null || echo "could not detach $1" >&2
}
