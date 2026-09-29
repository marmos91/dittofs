#!/usr/bin/env bash
# Host side of the DittoFS canary: one pass of canary.sh in a fresh privileged
# container (host network, so it reaches the server on localhost). For cron:
#   */15 * * * * root /path/to/test/harness/canary/run-canary.sh
# Writes logs/canary-<ts>.log, status.txt (the last run's verdict line), last.json
# and history.jsonl under CANARY_STATE, and keeps 14 days of logs. Exits like the
# pass (0 PASS, 1 FAIL); 0 without running if the previous pass is still going.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="${CANARY_ENV:-/etc/dittofs-canary/canary.env}"
STATE="${CANARY_STATE:-/srv/dittofs-canary}"   # logs, status, the dfsctl binary
IMAGE=dittofs-canary:latest
[[ -r "$ENV_FILE" ]] || { echo "canary: $ENV_FILE not readable (run setup-canary.sh)" >&2; exit 2; }
[[ -x "$STATE/bin/dfsctl" ]] || { echo "canary: $STATE/bin/dfsctl missing (run setup-canary.sh)" >&2; exit 2; }
mkdir -p "$STATE/logs"

exec 9>"$STATE/.lock"
flock -n 9 || { echo "canary: the previous pass is still running" >&2; exit 0; }

# Rebuild the image when the Dockerfile changes.
want="$(sha256sum "$HERE/Dockerfile" | cut -c1-16)"
have="$(docker image inspect -f '{{index .Config.Labels "canary.dockerfile"}}' "$IMAGE" 2>/dev/null || true)"
if [[ "$want" != "$have" ]]; then
    docker build -q --label "canary.dockerfile=$want" -t "$IMAGE" -f "$HERE/Dockerfile" "$HERE" >/dev/null ||
        { echo "canary: image build failed" >&2; exit 2; }
fi

log="$STATE/logs/canary-$(date +%Y%m%d-%H%M%S).log"
docker run --rm --privileged --network host --name "dittofs-canary-$$" --env-file "$ENV_FILE" \
    -v /etc/localtime:/etc/localtime:ro \
    -v "$HERE/canary.sh:/canary/canary.sh:ro" -v "$STATE/bin/dfsctl:/canary/bin/dfsctl:ro" \
    -v "$STATE:/out" "$IMAGE" /canary/canary.sh >"$log" 2>&1
rc=$?
tail -1 "$log" >"$STATE/status.txt"
find "$STATE/logs" -name 'canary-*.log' -mtime +14 -delete 2>/dev/null
exit "$rc"
