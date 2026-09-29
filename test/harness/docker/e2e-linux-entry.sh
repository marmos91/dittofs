#!/usr/bin/env bash
# Runs inside the Linux e2e runner container, from the repo root (bind-mounted at the
# same path as on the host). Starts rpcbind for the NLM/portmapper cases, then runs
# the suite through the repo's CI wrapper, which writes output to $E2E_LOG (a file,
# never a pipe) and grades it.
#   e2e-linux-entry.sh <go test args...>
set -uo pipefail
rpcbind -w 2>/dev/null || rpcbind 2>/dev/null || echo "[e2e-linux] rpcbind did not start; NLM cases will skip" >&2
exec .github/scripts/run-e2e.sh go test -tags=e2e -count=1 -v -timeout 30m "$@"
