#!/usr/bin/env bash
# Entry point of the dtc container. The suites talk to their services on
# localhost (Localstack :4566, the integration Postgres :15432,
# PyKMIP :5696). Those services are sibling containers publishing on the Docker host.
# On a bridge network (Docker Desktop) the host is host.docker.internal, so forward
# each port; sharing the host's network (DT_HOSTNET=1, a Linux engine), they are
# already on localhost.
set -uo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
if [[ "${DT_HOSTNET:-}" != 1 ]]; then
    for p in 4566 15432 5696; do
        socat "TCP-LISTEN:$p,bind=127.0.0.1,fork,reuseaddr" "TCP:host.docker.internal:$p" >/dev/null 2>&1 &
    done
fi
# A Linux engine keeps uid 0 on files a root container writes into bind mounts,
# so hand what the run created in the checkout (results, state, git index
# entries) back to the caller, DT_HOST_UID:DT_HOST_GID. dtc sets DT_FIX_OWNER=1
# only there: Docker Desktop maps ownership itself. -xdev: never descend into
# another file system, such as a mount a test left behind.
fix_owner() {
    local p
    for p in "${DITTOFS_REPO:-}" "$HARNESS" ${DT_GIT_DIR:+"$DT_GIT_DIR"}; do
        [[ -n "$p" && -e "$p" ]] || continue
        find "$p" -xdev -uid 0 -print0 2>/dev/null |
            xargs -0 -r chown -h "$DT_HOST_UID:${DT_HOST_GID:-$DT_HOST_UID}" 2>/dev/null || true
    done
}
trap 'exit 130' INT
trap 'exit 143' TERM
[[ "${DT_FIX_OWNER:-}" == 1 && -n "${DT_HOST_UID:-}" ]] && trap fix_owner EXIT
if [[ "${1:-}" == shell ]]; then
    # Not a login shell: Debian's /etc/profile would reset PATH and drop the image's
    # Go and pjdfstest.
    shift
    bash "$@"
else
    "$HARNESS/bin/dt" "$@"
fi
