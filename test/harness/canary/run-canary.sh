#!/usr/bin/env bash
# Host side of the DittoFS canary, for cron (as root):
#   */15 * * * * root /path/to/test/harness/canary/run-canary.sh
# One pass, by CANARY_IMPL:
#   go     (default) TestLiveCanary_SMB (test/e2e/live), in the Linux dev container
#          (bin/dtc) run as the checkout's owner, with the DITTOFS_E2E_LIVE_* variables
#          from the env file passed through the environment, never on a command line;
#   shell  canary.sh in the canary image.
# Writes logs/canary-<ts>.log, status.txt (the last verdict line), last.json and
# history.jsonl under CANARY_STATE, and keeps 14 days of logs. Exits like the pass
# (0 PASS, 1 FAIL); 0 without running if the previous pass is still going.
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

IMPL="${CANARY_IMPL:-go}"
log="$STATE/logs/canary-$(date +%Y%m%d-%H%M%S).log"

if [[ "$IMPL" == go ]]; then
    REPO="$(cd "$HERE/../../.." && pwd)"
    owner="$(stat -c %U "$REPO")"
    # /tmp/dtc is the path dtc shares with its container: the result comes back
    # through it, and two binaries go in. The server's own dfsctl (the client that
    # matches the server; the test would otherwise build this checkout's), and the
    # operators' rclone that setup copied, for `rclone size` at each checkpoint.
    [[ -x "$STATE/bin/rclone" ]] || { echo "CANARY FAIL impl=go: $STATE/bin/rclone missing (run setup-canary.sh with CANARY_RCLONE_BIN)" >"$STATE/status.txt"; exit 1; }
    [[ -d /tmp/dtc ]] || install -d -o "$owner" -g "$owner" /tmp/dtc
    result="/tmp/dtc/live-canary-$$.json" dfsctl="/tmp/dtc/live-canary-$$.dfsctl" rclone="/tmp/dtc/live-canary-$$.rclone"
    rm -f "$result"; install -m 0755 "$STATE/bin/dfsctl" "$dfsctl"; install -m 0755 "$STATE/bin/rclone" "$rclone"
    set -a
    # shellcheck disable=SC1090  # the env file setup-canary.sh writes
    . "$ENV_FILE"
    set +a
    export DITTOFS_E2E_LIVE_RESULT="$result" DITTOFS_E2E_LIVE_DFSCTL="$dfsctl" DITTOFS_E2E_LIVE_RCLONE="$rclone"
    # Root-only: a failing dfsctl call's error, in the test output, carries its args.
    (umask 077; echo "# rclone: $STATE/bin/rclone, sha256 $(sha256sum "$STATE/bin/rclone" | cut -c1-16)" >"$log")
    # setpriv keeps the environment (sudo would reset it): the secrets stay out of argv.
    (cd "$REPO" && setpriv --reuid="$owner" --regid="$owner" --init-groups env HOME="$(getent passwd "$owner" | cut -d: -f6)" \
        "$REPO/test/harness/bin/dtc" shell -c 'go test -tags=e2e -count=1 -v -run "^TestLiveCanary_SMB$" ./test/e2e/live/') \
        </dev/null >>"$log" 2>&1
    rc=$?
    if [[ -s "$result" ]]; then
        jq -c . "$result" | tee -a "$STATE/history.jsonl" >"$STATE/last.json"
        jq -r '"CANARY \(.status) run=\(.run) impl=go \(.rclone // "rclone ?") files=\(.files) bytes=\(.bytes) rclone_size(before/written/deleted/gc)=\([.rclone_size.before, .rclone_size.written, .rclone_size.deleted, .rclone_size.gc] | map(.objects // "-") | join("/")) objects seconds=\(.seconds.total | floor)" + (if .status == "FAIL" then " failed_step=\(.step)" else "" end)' \
            "$result" >"$STATE/status.txt"
    else
        echo "CANARY FAIL impl=go: the test wrote no result (build or setup failure; see $log)" >"$STATE/status.txt"
        ((rc == 0)) && rc=1
    fi
    rm -f "$result" "$dfsctl" "$rclone"
    find "$STATE/logs" -name 'canary-*.log' -mtime +14 -delete 2>/dev/null
    exit "$rc"
fi

# Rebuild the image when the Dockerfile changes.
want="$(sha256sum "$HERE/Dockerfile" | cut -c1-16)"
have="$(docker image inspect -f '{{index .Config.Labels "canary.dockerfile"}}' "$IMAGE" 2>/dev/null || true)"
if [[ "$want" != "$have" ]]; then
    docker build -q --label "canary.dockerfile=$want" -t "$IMAGE" -f "$HERE/Dockerfile" "$HERE" >/dev/null ||
        { echo "canary: image build failed" >&2; exit 2; }
fi

docker run --rm --privileged --network host --name "dittofs-canary-$$" --env-file "$ENV_FILE" \
    -v /etc/localtime:/etc/localtime:ro \
    -v "$HERE/canary.sh:/canary/canary.sh:ro" -v "$STATE/bin:/canary/bin:ro" \
    -v "$STATE:/out" "$IMAGE" /canary/canary.sh >"$log" 2>&1
rc=$?
tail -1 "$log" >"$STATE/status.txt"
find "$STATE/logs" -name 'canary-*.log' -mtime +14 -delete 2>/dev/null
exit "$rc"
