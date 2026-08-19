#!/usr/bin/env bash
# Brings up a PyKMIP server in Docker, provisions the keys the KMIP
# interop tests need, and writes the DITTOFS_TEST_KMIP_* environment the
# gated tests in pkg/block/encryption/keyprovider read.
#
#   ./test/kmip/start-pykmip.sh [certs-dir] [env-file]
#
# Defaults write certificates to ./test/kmip/certs and the environment to
# stdout. Tear down afterwards with: docker rm -f dittofs-pykmip
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CERTS="${1:-$HERE/certs}"
ENV_FILE="${2:-/dev/stdout}"
CONTAINER=dittofs-pykmip
IMAGE=python:3.11-slim

CERTS="$("$HERE/gen-certs.sh" "$CERTS")"
CERTS="$(cd "$CERTS" && pwd)"

docker rm -f "$CONTAINER" >/dev/null 2>&1 || true

# Retry the pull: a transient registry hiccup should not fail the job.
for attempt in 1 2 3 4 5; do
  if docker pull "$IMAGE"; then break; fi
  if [ "$attempt" = 5 ]; then echo "docker pull $IMAGE failed after 5 attempts" >&2; exit 1; fi
  echo "docker pull $IMAGE failed (attempt $attempt), retrying..." >&2
  sleep $((attempt * 5))
done

docker run -d --name "$CONTAINER" -p 5696:5696 \
  -v "$CERTS:/certs:ro" \
  -v "$HERE:/work:ro" \
  "$IMAGE" \
  sh -c 'pip install --no-cache-dir pykmip && mkdir -p /etc/pykmip/policies && exec pykmip-server -f /work/server.conf' \
  >/dev/null

# pip install then server start; the port is the readiness signal.
for i in $(seq 1 60); do
  if docker exec "$CONTAINER" python -c 'import socket,sys; s=socket.socket(); sys.exit(s.connect_ex(("127.0.0.1",5696)))' 2>/dev/null; then
    break
  fi
  if [ "$i" = 60 ]; then
    echo "PyKMIP did not start in time" >&2
    docker logs "$CONTAINER" >&2
    exit 1
  fi
  sleep 2
done

{
  echo "DITTOFS_TEST_KMIP=1"
  echo "DITTOFS_TEST_KMIP_ENDPOINT=127.0.0.1:5696"
  echo "DITTOFS_TEST_KMIP_CERT=$CERTS/client-cert.pem"
  echo "DITTOFS_TEST_KMIP_KEY=$CERTS/client-key.pem"
  echo "DITTOFS_TEST_KMIP_CA=$CERTS/ca-cert.pem"
  docker exec "$CONTAINER" python /work/provision.py
} >>"$ENV_FILE"
