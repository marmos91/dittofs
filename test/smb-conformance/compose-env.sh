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
# suites contending for the same CPU. Serial is the intended mode, so the
# second run refuses immediately and names the checkout holding the stack
# rather than failing to bind a port somewhere in the middle of bootstrap.

# ponytail: cksum is POSIX and present everywhere the harness runs; the value
# only has to separate a handful of checkouts on one machine, and two that
# collided would be caught by the exclusivity check below rather than silently
# sharing a stack. Reach for a real hash only if that ever bites.
_compose_repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
COMPOSE_PROJECT_NAME="smb-conformance-$(printf '%s' "$_compose_repo_root" | cksum | cut -d' ' -f1)"
export COMPOSE_PROJECT_NAME
unset _compose_repo_root

# require_exclusive_stack — abort when a conformance stack from another
# checkout is live, naming the directory it was started from.
require_exclusive_stack() {
    # A docker that cannot be reached reports nothing rather than aborting the
    # run here; the first compose command will fail with a better message.
    local other
    other="$(docker ps \
        --format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}' \
        2>/dev/null |
        awk -F'|' -v me="$COMPOSE_PROJECT_NAME" \
            '$1 ~ /^smb-conformance/ && $1 != me { print; exit }' || true)"
    [[ -z "$other" ]] && return 0

    local project="${other%%|*}" workdir="${other#*|}"
    cat >&2 <<EOF

ERROR: an SMB conformance stack is already running from ${workdir:-an unknown directory}
       (Compose project ${project}).

The stack publishes fixed host ports and the suites are timing-sensitive,
so only one checkout may run it at a time. Wait for that run to finish, or
stop it with:
    docker compose -p ${project} down -v

EOF
    exit 1
}
