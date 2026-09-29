#!/usr/bin/env bash
# Entry point of the dtc container. The repo's scripts talk to their services on
# localhost (Localstack :4566, Postgres :5432, the integration Postgres :15432,
# PyKMIP :5696). Those services are sibling containers publishing on the Docker host,
# which this container reaches as host.docker.internal, so forward each port.
set -euo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
for p in 4566 5432 15432 5696; do
    socat "TCP-LISTEN:$p,bind=127.0.0.1,fork,reuseaddr" "TCP:host.docker.internal:$p" >/dev/null 2>&1 &
done
if [[ "${1:-}" == shell ]]; then
    shift
    exec bash -l "$@"
fi
exec "$HARNESS/bin/dt" "$@"
