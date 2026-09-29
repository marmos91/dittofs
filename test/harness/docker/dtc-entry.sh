#!/usr/bin/env bash
# Entry point of the dtc container. The repo's scripts talk to their services on
# localhost (Localstack :4566, Postgres :5432, the integration Postgres :15432,
# PyKMIP :5696). Those services are sibling containers publishing on the Docker host.
# On a bridge network (Docker Desktop) the host is host.docker.internal, so forward
# each port; sharing the host's network (DT_HOSTNET=1, a Linux engine), they are
# already on localhost.
set -uo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${DT_HOSTNET:-}" != 1 ]]; then
    for p in 4566 5432 15432 5696; do
        socat "TCP-LISTEN:$p,bind=127.0.0.1,fork,reuseaddr" "TCP:host.docker.internal:$p" >/dev/null 2>&1 &
    done
fi
# On a Linux engine, hand files the run created in the checkout (its logs and state,
# test results, git index entries) back to the caller. A no-op unless DT_FIX_OWNER=1.
# /tmp/dtc only at the top: the e2e leftovers below it are removed by dt.
fix_owner() {
    "$HARNESS/docker/fix-owner.sh" "${DITTOFS_REPO:-}" "$HARNESS" ${DT_GIT_DIR:+"$DT_GIT_DIR"}
    find /tmp/dtc -maxdepth 1 -uid 0 -exec chown -h "${DT_HOST_UID:-0}:${DT_HOST_GID:-0}" {} + 2>/dev/null || true
}
trap 'exit 130' INT
trap 'exit 143' TERM
[[ "${DT_FIX_OWNER:-}" == 1 ]] && trap fix_owner EXIT
if [[ "${1:-}" == shell ]]; then
    # Not a login shell: Debian's /etc/profile would reset PATH and drop the image's
    # Go and pjdfstest.
    shift
    bash "$@"
else
    "$HARNESS/bin/dt" "$@"
fi
