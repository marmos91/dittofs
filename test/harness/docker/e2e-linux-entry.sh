#!/usr/bin/env bash
# Runs inside the Linux e2e runner container, from the repo root (bind-mounted at the
# same path as on the host). Starts rpcbind for the NLM/portmapper cases, then runs
# the suite through the repo's CI wrapper, which writes output to $E2E_LOG (a file,
# never a pipe) and grades it.
#   e2e-linux-entry.sh <go test args...>
set -uo pipefail
# On exit: first drop the NFS mounts the suite leaves under TMPDIR (hard mounts to
# stopped servers; any stat below them blocks, find included). Then, on a Linux
# engine, which keeps uid 0 on bind-mounted files, hand the log and what the suite
# wrote back to the caller (fix-owner.sh is a no-op on Docker Desktop).
finish() {
    local m
    awk -v p="${TMPDIR%/}/" 'index($2, p) == 1 {print $2}' /proc/mounts | while read -r m; do
        umount -f -l "$m" 2>/dev/null || true
    done
    /fix-owner.sh "${E2E_LOG:-}" "$PWD" "${TMPDIR:-}" 2>/dev/null || true
}
trap finish EXIT
rpcbind -w 2>/dev/null || rpcbind 2>/dev/null || echo "[e2e-linux] rpcbind did not start; NLM cases will skip" >&2
.github/scripts/run-e2e.sh go test -tags=e2e -count=1 -v -timeout 30m "$@"
