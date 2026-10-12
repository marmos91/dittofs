#!/usr/bin/env bash
# Named NFS mount option sets — the variant axis of the cthon04 and nfstest
# suites. Sourced, not executed.
#
# Every other NFS harness here mounts with caching and locking switched off
# (pjdfstest: noac,nolock,sync,lookupcache=none; e2e: actimeo=0). These sets
# are the opposite: they are what a client gets when nobody tunes the mount,
# plus one deliberate deviation each, so a difference between them points at
# the option that caused it.
#
# `default` passes no options at all, so the kernel negotiates exactly what a
# plain `mount -t nfs server:/export` gets. Against DittoFS that is NFSv4.2: the
# client takes the highest version the server offers. The other sets pin a
# version so a failure can be told apart by version.
#
# The port is not part of a set. It is appended by the callers, because the
# two suites take it differently: cthon04 through `mount -o`, nfstest through
# its own --port flag.
#
# decision: every NFSv3 set mounts with nolock. A kernel NFSv3 client finds the
# lock manager through rpcbind on port 111, and on a single host the client's
# own lockd owns that registration the moment a locking mount activates — so a
# v3 lock request would reach the local lockd, not DittoFS, and a lock test
# would pass against the wrong server (test/e2e/nlm_locking_test.go hits the
# same wall). Locking is exercised on the v4 sets, where it is in-protocol.
# Withdraw this when these suites run the client in its own network namespace,
# as test/e2e/framework/netns.go already does for the NLM interop tests.

# option_set_names — every set, in the order the manifest lists them.
option_set_names() {
    printf '%s\n' default v3 v4.0 v4.1 v4.1-noac v4.1-smallio
}

# option_set_opts NAME — the mount options for NAME, without the port. Fails
# for an unknown name so a typo in the manifest cannot fall back to a default.
option_set_opts() {
    case "$1" in
    default) echo "" ;;
    v3) echo "vers=3,tcp,mountproto=tcp,nolock" ;;
    v4.0) echo "vers=4.0" ;;
    v4.1) echo "vers=4.1" ;;
    v4.1-noac) echo "vers=4.1,noac" ;;
    v4.1-smallio) echo "vers=4.1,rsize=32768,wsize=32768" ;;
    *) return 1 ;;
    esac
}

# DEFAULT_NFS_VERSION — what the kernel negotiates against DittoFS when the
# mount names no version. cthon04 never needs it: it mounts `default` with no
# vers= and takes whatever is negotiated. nfstest has to name a version, so for
# `default` it runs this one; if a kernel or DittoFS change moves the default,
# update it here.
DEFAULT_NFS_VERSION=4.2

# option_set_version NAME — the NFS version the set mounts with (3, 4.0, 4.1,
# or DEFAULT_NFS_VERSION for a set that names none).
option_set_version() {
    local opts
    opts="$(option_set_opts "$1")" || return 1
    if [[ ",${opts}," != *,vers=* ]]; then
        echo "$DEFAULT_NFS_VERSION"
        return
    fi
    opts=",${opts},"
    opts="${opts#*,vers=}"
    echo "${opts%%,*}"
}

# option_set_mount_opts NAME PORT — the full `mount -o` string. NFSv3 also needs
# the MOUNT protocol's port, which DittoFS serves on the same one.
option_set_mount_opts() {
    local opts
    opts="$(option_set_opts "$1")" || return 1
    if [[ "$(option_set_version "$1")" == 3 ]]; then
        echo "${opts:+${opts},}port=$2,mountport=$2"
    else
        echo "${opts:+${opts},}port=$2"
    fi
}

# option_set_extra_opts NAME — the options other than vers=, for tools that
# take the version separately (nfstest's --nfsversion).
option_set_extra_opts() {
    local opts out="" o
    opts="$(option_set_opts "$1")" || return 1
    IFS=',' read -ra parts <<<"$opts"
    for o in "${parts[@]}"; do
        [[ "$o" == vers=* ]] && continue
        out+="${out:+,}${o}"
    done
    echo "$out"
}

# option_set_locks NAME — true when locks reach the server under this set.
option_set_locks() {
    [[ ",$(option_set_opts "$1")," != *,nolock,* ]]
}
