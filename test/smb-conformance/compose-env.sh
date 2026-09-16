# shellcheck shell=bash
# Scopes the conformance Compose stack to the checkout it is run from, and
# refuses to start a second one.
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
# somewhere in the middle of bootstrap. Two runs starting in the same instant
# still reach the port — see the decision recorded at the check itself.

# ponytail: cksum is POSIX and present everywhere the harness runs; the value
# only has to separate a handful of checkouts on one machine. Reach for a real
# hash only if two roots ever collide.
_compose_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_PROJECT_NAME="smb-conformance-$(printf '%s' "$_compose_repo_root" | cksum | cut -d' ' -f1)"
export COMPOSE_PROJECT_NAME
unset _compose_repo_root

# require_exclusive_stack — abort when any conformance stack is live, naming the
# directory it was started from.
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

# decision: this scan is advisory and deliberately not atomic with the compose
# up that follows. It exists for the case the claim cannot see — a stack left by
# an earlier run, or by a DIFFERENT checkout, whose project name is not the one
# claim_exclusive_stack holds. Two different checkouts starting together both
# pass here and then race for the fixed host ports; the loser fails at its bind,
# which is loud and attributable to a port rather than to the server under test.
# Same-checkout concurrency is what the claim covers, and that one is atomic.
# Take a host-wide lock across every checkout only if a port-bind loss is ever
# mistaken for a server fault in practice.
require_exclusive_stack() {
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
