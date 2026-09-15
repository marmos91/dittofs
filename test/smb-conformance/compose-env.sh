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
# decision: admission is not atomic with the `docker compose up` that follows,
# so two runs starting in the same instant can both read an empty table and then
# race for the published ports. The loser fails at bind — loud, and attributable
# to a port rather than to the server under test, which is the outcome this
# check is for. A host-level lock would close the window; take one only if that
# race is ever actually observed, since it adds a lock file to reap after a
# killed run.
require_exclusive_stack() {
    # A docker that cannot be reached reports nothing rather than aborting the
    # run here; the first compose command will fail with a better message.
    local live
    live="$(docker ps \
        --format '{{.Label "com.docker.compose.project"}}|{{.Label "com.docker.compose.project.working_dir"}}' \
        2>/dev/null |
        awk -F'|' '$1 ~ /^smb-conformance/ { print; exit }' || true)"
    [[ -z "$live" ]] && return 0

    local project="${live%%|*}" workdir="${live#*|}"
    cat >&2 <<EOF

ERROR: an SMB conformance stack is already running from ${workdir:-an unknown directory}
       (Compose project ${project}).

The stack publishes fixed host ports and the suites are timing-sensitive, so
only one may run at a time — including a stack an earlier run left behind with
--keep, which a new run cannot reuse. Wait for that run to finish, or stop it
with:
    docker compose -p ${project} down -v

EOF
    exit 1
}
