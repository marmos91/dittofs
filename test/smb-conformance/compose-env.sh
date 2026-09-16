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
# that starts while a stack is already up refuses before it creates anything
# and names the directory holding that stack, instead of failing to bind a port
# somewhere in the middle of bootstrap.
#
# Three mechanisms admit a run, and each answers a question the others cannot:
# the host-wide lease (one run at a time anywhere on the machine), the atomic
# per-checkout claim (two runs from one checkout cannot both proceed), and the
# leftover scan (a stack that outlived its run still owns the bootstrapped
# volume). See the decision at each.

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

# Recorded in the lease file, so a refused run can name what it lost to rather
# than printing a bare "already running".
_conformance_lease_owner="$_compose_repo_root"
unset _compose_repo_root

# Takes the lease, or aborts. Held on file descriptor 9, which the kernel drops
# when the last process holding it closes it: there is nothing to reap after a
# killed run, and no stale lock for the next run to time out or override.
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
# decision: the lock belongs to the inode, not to the path, so removing the
# file while a run holds it lets the next run create a fresh inode at the same
# path, lock that one, and be admitted beside a live run. Nothing here can see
# that — a contender sees only its own consistent inode, and comparing the
# locked inode against the path catches nothing, which was measured rather than
# assumed. The residual is accepted because the harness never removes the file
# and no /tmp sweeper reaches one touched by every run, so closing it means a
# lock with no path at all — an abstract socket or a fixed loopback port. Take
# one of those if a lease is ever actually observed to be bypassed this way.
#
# ponytail: perl rather than flock(1), which is util-linux and absent from
# macOS, where this harness also runs. The lock lives on the open file
# description that the redirection shares with perl, so it survives perl
# exiting and is released only when this shell closes descriptor 9. Swap in
# flock(1) if the harness ever stops supporting macOS.
_acquire_conformance_lease() {
    # The group's redirection discards exec's own message and is restored with
    # the group; the descriptor exec opens outlives it.
    if ! { exec 9>>"$CONFORMANCE_LEASE_FILE"; } 2>/dev/null; then
        echo "ERROR: cannot open the conformance lease at ${CONFORMANCE_LEASE_FILE}." >&2
        echo "       Set DITTOFS_CONFORMANCE_LEASE to a writable path." >&2
        exit 1
    fi

    local rc=0
    perl -e 'use Fcntl ":flock"; exit(flock(STDIN, LOCK_EX|LOCK_NB) ? 0 : 1)' <&9 || rc=$?
    if [[ "$rc" -eq 0 ]]; then
        printf 'pid %s  %s  started %s\n' \
            "$$" "$_conformance_lease_owner" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" \
            >"$CONFORMANCE_LEASE_FILE"
        return 0
    fi

    # No perl is the safe direction — the run is refused rather than admitted
    # blind — but reporting it as a held lease sends the reader hunting for a
    # run that does not exist.
    if [[ "$rc" -eq 127 ]]; then
        echo "ERROR: taking the conformance lease needs perl, which is not on PATH." >&2
        exit 1
    fi

    local holder
    holder="$(head -1 "$CONFORMANCE_LEASE_FILE" 2>/dev/null)"
    cat >&2 <<EOF

ERROR: another SMB conformance run holds the lease at ${CONFORMANCE_LEASE_FILE}.
       Holder: ${holder:-unknown (it has not recorded itself yet)}

The harness publishes fixed host ports and the suites are timing-sensitive, so
only one run may be in flight at a time, in any checkout and in either mode.

Wait for that run to finish; the lease is released by the kernel, so there is
nothing to clean up by hand. If the run above was killed, the lease is still
held by whatever it left behind — the native server \`--mode local\` starts, or
the client container. Find what that is with:
    lsof ${CONFORMANCE_LEASE_FILE}

EOF
    exit 1
}

# require_exclusive_stack — admit this run, or abort naming what blocks it, and
# hold admission for the run's duration.
#
# Every live stack conflicts, this checkout's own included. Nothing of this run
# exists yet when the check runs, so a container carrying our project name is a
# stack left behind by an earlier run, and adopting it is not a shortcut: the
# server is already bootstrapped, so the second run's bootstrap fails partway
# through on an existing admin password or an existing store, which is the same
# unexplained mid-run collapse this check exists to replace.
#
# Every live stack conflicts, this checkout's own included. Nothing of this run
# exists yet when the check runs, so a container carrying our project name is a
# stack left behind by an earlier run, and adopting it is not a shortcut: the
# server is already bootstrapped, so the second run's bootstrap fails partway
# through on an existing admin password or an existing store, which is the same
# unexplained mid-run collapse this check exists to replace.
#
# The container scan below cannot make this decision on its own: two runs from
# the same checkout derive the same COMPOSE_PROJECT_NAME, so both can read an
# empty table and the second `docker compose up` then JOINS the first run's
# project rather than losing a port. Either EXIT trap would afterwards run
# `down -v` on the shared project and destroy the other run's server — the
# interference this check exists to prevent, arriving through the cleanup. And
# STACK_OWNED does not help there: it records that this process called `up`, not
# that it created anything.
#
# So the claim is an atomic one. `mkdir` either creates the directory or fails,
# on every POSIX filesystem, with no dependency on flock being available.
#
# ponytail: a directory under TMPDIR holding the owner's PID, reaped when that
# PID is gone. Enough to serialize runs on one machine, which is all the ports
# and the project name are shared across; reach for a real lock manager only if
# these ever need to coordinate across hosts.
_stack_claim_dir() {
    printf '%s/dittofs-%s.claim' "${TMPDIR:-/tmp}" "$1"
}

claim_exclusive_stack() {
    local dir owner
    dir="$(_stack_claim_dir "${COMPOSE_PROJECT_NAME}")"

    if mkdir "$dir" 2>/dev/null; then
        echo "$$" > "${dir}/pid"
        STACK_CLAIM_DIR="$dir"
        return 0
    fi

    owner="$(cat "${dir}/pid" 2>/dev/null || true)"

    # An empty pid is a claim being taken RIGHT NOW, not an abandoned one: the
    # winner of the mkdir above writes its pid a moment later. Reaping on that
    # basis reintroduces exactly the race the claim exists to remove — the
    # winner would have its directory deleted out from under it and both runs
    # would proceed against one Compose project.
    #
    # decision: a claim whose owner is a live PID is refused, and one whose owner
    # is gone is reaped and retried ONCE. Anything else — an empty pid, or a
    # second failure after reaping — is refused and left for a human, because
    # the alternative is guessing about a directory another process may be in
    # the middle of creating. The cost is that a run killed between mkdir and
    # the pid write leaves a claim nothing reclaims automatically; the refusal
    # prints the one command that clears it. Revisit if that is ever seen in
    # practice rather than reasoned about.
    # The reap needs a lock of its own. Two runs can read the same dead PID and
    # both decide to reap: the first recreates the claim, is preempted before
    # writing its pid, and the second's rm -rf then deletes it — leaving both
    # holding what each believes is an exclusive claim, which is the collision
    # this whole mechanism exists to prevent. mkdir on a second directory picks
    # one reaper; the other refuses below.
    #
    # A reap lock left behind by a crash costs a refusal, never a double claim,
    # so it is not itself reaped.
    if [[ -n "$owner" ]] && ! kill -0 "$owner" 2>/dev/null; then
        if mkdir "${dir}.reap" 2>/dev/null; then
            # Re-read under the reap lock: the owner may have been replaced
            # between the check above and here, and reaping a live claim is the
            # same collision by a slower route.
            owner="$(cat "${dir}/pid" 2>/dev/null || true)"
            if [[ -n "$owner" ]] && ! kill -0 "$owner" 2>/dev/null; then
                rm -rf "$dir"
                if mkdir "$dir" 2>/dev/null; then
                    echo "$$" > "${dir}/pid"
                    STACK_CLAIM_DIR="$dir"
                    rmdir "${dir}.reap" 2>/dev/null || true
                    return 0
                fi
            fi
            rmdir "${dir}.reap" 2>/dev/null || true
        fi
        owner="$(cat "${dir}/pid" 2>/dev/null || true)"
    fi

    # Same reason the container-scan refusal writes one: this exits from inside
    # the graded step, and without a verdict the common runner renders the exit
    # status as "1 new failure(s)" for a run in which no test started.
    if [[ -n "${DITTOFS_RESULTS_DIR:-}" && -d "${DITTOFS_RESULTS_DIR}" ]]; then
        echo "refused 0 0 0" > "${DITTOFS_RESULTS_DIR}/verdict"
    fi

    cat >&2 <<EOF

ERROR: another run already holds this stack (Compose project ${COMPOSE_PROJECT_NAME},
       claimed by PID ${owner:-a run that has not finished claiming it}).

The stack publishes fixed host ports and the suites are timing-sensitive, so
only one may run at a time from a given checkout. Wait for that run to finish.
If you are sure no such process exists, remove the claim:
    rm -rf ${dir}

EOF
    exit 1
}

release_exclusive_stack() {
    [[ -n "${STACK_CLAIM_DIR:-}" ]] && rm -rf "${STACK_CLAIM_DIR}"
    STACK_CLAIM_DIR=""
}

# decision: admission is the lease plus this scan; the two answer different
# questions and neither substitutes for the other. The lease admits one run at
# a time across every checkout — which is what the claim cannot do, since it is
# keyed on a per-checkout project name — and it outlives a killed harness for as
# long as anything that harness started still holds the ports. What the lease
# cannot see is a stack that outlived the run that made it: a `--keep` stack, or
# one merely stopped, carries no live process and so holds no lease, yet it
# still owns the bootstrapped volume. The scan below is that case.
#
# The scan stays advisory and deliberately not atomic with the compose up that
# follows, which is now acceptable because the lease carries the atomicity:
# two checkouts starting together cannot both hold it. The scan is left
# non-atomic rather than turned into a second lock because it reads docker
# state, and a lock over that would be held across the compose up below.
require_exclusive_stack() {
    _acquire_conformance_lease

    # A docker that cannot be reached reports nothing rather than aborting the
    # run here; the first compose command will fail with a better message.
    # -a, not just running: a kept stack that a daemon restart or a manual
    # `docker stop` left stopped still owns the bootstrapped volume, and the
    # `docker compose up` below would adopt it and fail partway through on an
    # existing admin password or store — the same unexplained mid-run collapse
    # this check replaces. A stopped project is a blocker too, and the remedy
    # printed below is the same `down -v` either way.
    local live
    live="$(docker ps -a \
        --format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}' \
        2>/dev/null |
        awk -F'|' '$1 ~ /^smb-conformance/ { print; exit }' || true)"
    if [[ -z "$live" ]]; then
        # `docker compose down` without -v removes the containers and keeps the
        # labeled volumes, so a project can still own a bootstrapped store with
        # no container left to find it by. A run that adopts that store fails
        # partway through bootstrap on an existing admin password — the same
        # unexplained mid-run collapse, reached with the container table empty.
        local vols
        vols="$(docker volume ls \
            --format '{{.Label "com.docker.compose.project"}}' \
            2>/dev/null |
            awk '$1 ~ /^smb-conformance/ { print; exit }' || true)"
        [[ -z "$vols" ]] && return 0
        live="${vols}|"
    fi

    local project="${live%%|*}" workdir="${live#*|}"

    # This refusal exits from inside the graded step, so without a verdict of
    # its own the common runner falls back to rendering the exit status as
    # "N new failure(s)" — reporting a regression for a run in which no test
    # ever started. The counts are zero because nothing was graded.
    if [[ -n "${DITTOFS_RESULTS_DIR:-}" && -d "${DITTOFS_RESULTS_DIR}" ]]; then
        echo "refused 0 0 0" > "${DITTOFS_RESULTS_DIR}/verdict"
    fi

    cat >&2 <<EOF

ERROR: an SMB conformance stack already exists from ${workdir:-an unknown directory}
       (Compose project ${project}).

The stack publishes fixed host ports and the suites are timing-sensitive, so
only one may run at a time — including a stack an earlier run left behind with
--keep, and one that is merely stopped, neither of which a new run can reuse.
Wait for that run to finish, or clear it from anywhere with:
    docker rm -f \$(docker ps -aq --filter label=com.docker.compose.project=${project})
    docker volume rm \$(docker volume ls -q --filter label=com.docker.compose.project=${project})
    docker network rm \$(docker network ls -q --filter label=com.docker.compose.project=${project})

Both queries go by label rather than by name or Compose file, because
neither of those is reliable here: `docker compose` has to run from the
directory holding the compose file, and `docker rm` does not expand globs,
so a name pattern silently removes nothing. The label also covers the
one-off containers `docker compose run` creates, which `down -v` does not
reap and which block every retry until they go.

EOF
    exit 1
}
