# shellcheck shell=bash
# Scopes the conformance Compose stack to the checkout it is run from, and
# admits one conformance run at a time on the host.
#
# Compose names a project after the directory it is invoked from, so the main
# checkout and every worktree of this repository all resolve to one project
# called "smb-conformance". Two harness runs started from different checkouts
# therefore share one set of containers: the second adopts the first's server
# and network, and whichever finishes first tears them down with
# `docker compose down -v` while the other is still using them. The surviving
# run then reports a connection refusal or a half-initialised store, which
# reads exactly like the change under test having broken the server.
#
# The project name below carries a checksum of the repository root, so each
# checkout owns its own stack.
#
# The published host ports stay a single set, so two stacks still cannot be up
# at the same time. That is deliberate: these suites assert on lease breaks and
# oplock breaks within a second of the request, margins that do not survive two
# suites contending for the same CPU. Serial is the intended mode, so a run
# that starts while another is in flight refuses before it creates anything,
# instead of failing to bind a port somewhere in the middle of bootstrap.

# ponytail: cksum is POSIX and present everywhere the harness runs; the value
# only has to separate a handful of checkouts on one machine. Reach for a real
# hash only if two roots ever collide.
_compose_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_PROJECT_NAME="smb-conformance-$(printf '%s' "$_compose_repo_root" | cksum | cut -d' ' -f1)"
export COMPOSE_PROJECT_NAME

# The lease that admits one run at a time. Fixed, host-wide path: what is
# contended is the host — ports 445 and 8080, and the single DittoFS the whole
# run talks to — not anything owned by a checkout or a user. The override
# exists so the lease can be exercised without disturbing a real run.
CONFORMANCE_LEASE_FILE="${DITTOFS_CONFORMANCE_LEASE:-/tmp/dittofs-smb-conformance.lease}"

# Identifies this run in the lease file, so a refused run can name what it lost
# to rather than printing a bare "already running".
_conformance_lease_owner="$_compose_repo_root"
unset _compose_repo_root

# require_exclusive_stack — admit this run, or abort naming what blocks it.
#
# Two separate refusals, in order:
#
#   1. Another run is in flight. Decided by a lease on a host-wide file, taken
#      here and held until this process exits.
#   2. A stack this checkout left behind. Decided by asking Docker for our own
#      Compose project.
#
# decision: admission is the lease alone; the leftover check answers a
# different question and cannot substitute for it. A lease covers only runs
# that still exist, and a stack left by `--keep` outlives the run that made it:
# its server is already bootstrapped, so the `docker compose up` below adopts
# it and fails partway through on an existing admin password or store, which is
# the unexplained mid-run collapse both of these exist to replace. Conversely
# the leftover check cannot see a live run at all in `--mode local`, which
# creates no Compose project. A leftover from a *different* checkout is not
# covered by either: it carries another project name, and if it is still
# running the port bind fails — loud, and attributable to a port rather than to
# the server under test. Widen the leftover query beyond this project only if
# that bind failure is ever actually mistaken for a defect.
require_exclusive_stack() {
    _acquire_conformance_lease
    _require_no_leftover_stack
}

# Takes the lease, or aborts. Held on file descriptor 9, which the kernel drops
# when the last process holding it closes it. There is nothing to reap after a
# killed run and no stale lock for the next run to time out or override.
#
# decision: descriptor 9 is inherited, so the lease outlives this shell for as
# long as any process the run started still holds it — the native server that
# `--mode local` leaves running when the harness is killed outright, or the
# client container's CLI. That is the semantic wanted, not a leak: what the
# lease guards is the host ports and the one server under test, and those are
# still in use by exactly those processes. It does mean the pid recorded below
# may be gone while the lease is still held, so the refusal points at lsof
# rather than claiming the holder is the recorded pid.
#
# The file itself is left behind on purpose — unlinking it would let a third
# run create a fresh inode and lock that instead of the one still held.
#
# ponytail: perl rather than flock(1), which is util-linux and absent from
# macOS, where this harness also runs. The lock lives on the open file
# description that the redirection shares with perl, so it survives perl
# exiting and is released only when this shell closes descriptor 9. Swap in
# flock(1) if the harness ever stops supporting macOS.
_acquire_conformance_lease() {
    if ! : >>"$CONFORMANCE_LEASE_FILE" 2>/dev/null; then
        echo "ERROR: cannot open the conformance lease at ${CONFORMANCE_LEASE_FILE}." >&2
        echo "       Set DITTOFS_CONFORMANCE_LEASE to a writable path." >&2
        exit 1
    fi
    exec 9>>"$CONFORMANCE_LEASE_FILE"

    if perl -e 'use Fcntl ":flock"; exit(flock(STDIN, LOCK_EX|LOCK_NB) ? 0 : 1)' <&9; then
        printf 'pid %s  %s  project %s  started %s\n' \
            "$$" "$_conformance_lease_owner" "$COMPOSE_PROJECT_NAME" \
            "$(date -u +%Y-%m-%dT%H:%M:%SZ)" >"$CONFORMANCE_LEASE_FILE"
        return 0
    fi

    local holder
    holder="$(head -1 "$CONFORMANCE_LEASE_FILE" 2>/dev/null)"
    cat >&2 <<EOF

ERROR: another SMB conformance run holds the lease at ${CONFORMANCE_LEASE_FILE}.
       Holder: ${holder:-unknown (it has not recorded itself yet)}

The harness publishes fixed host ports and the suites are timing-sensitive, so
only one run may be in flight at a time — whichever mode it runs in, since
\`--mode local\` starts a native server and an unlabelled container that no
Docker query would attribute to a conformance run.

Wait for that run to finish; the lease is released by the kernel, so there is
nothing to clean up by hand. If the run above was killed, the lease is still
held by whatever it left behind — the native server \`--mode local\` starts, or
the client container. Find what that is with:
    lsof ${CONFORMANCE_LEASE_FILE}

EOF
    exit 1
}

# Refuses a stack this checkout left behind, running or merely stopped. A
# stopped project still owns the bootstrapped volume, and `docker compose up`
# adopts it rather than starting clean.
_require_no_leftover_stack() {
    # A docker that cannot be reached reports nothing rather than aborting the
    # run here; the first compose command will fail with a better message.
    local leftover
    leftover="$(docker ps -aq \
        --filter "label=com.docker.compose.project=${COMPOSE_PROJECT_NAME}" \
        2>/dev/null | head -1 || true)"
    [[ -z "$leftover" ]] && return 0

    cat >&2 <<EOF

ERROR: an SMB conformance stack from this checkout still exists
       (Compose project ${COMPOSE_PROJECT_NAME}).

No run owns it — a run left it behind with --keep, or it was stopped by hand or
by a daemon restart. A new run cannot reuse it: its server is already
bootstrapped, so bootstrap fails partway through on an existing admin password
or store. Clear it from anywhere with:
    docker rm -f \$(docker ps -aq --filter label=com.docker.compose.project=${COMPOSE_PROJECT_NAME})
    docker volume rm \$(docker volume ls -q --filter label=com.docker.compose.project=${COMPOSE_PROJECT_NAME})

Both queries go by label rather than by name or Compose file, because
neither of those is reliable here: \`docker compose\` has to run from the
directory holding the compose file, and \`docker rm\` does not expand globs,
so a name pattern silently removes nothing. The label also covers the
one-off containers \`docker compose run\` creates, which \`down -v\` does not
reap and which block every retry until they go.

EOF
    exit 1
}
