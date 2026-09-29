#!/usr/bin/env bash
# Host side of the DittoFS canary, for cron (as root):
#   */15 * * * * root /path/to/test/harness/canary/run-canary.sh
# One pass, by CANARY_IMPL:
#   go     (default) TestLiveCanary_SMB (test/e2e/live), in the Linux dev container
#          (bin/dtc) run as the checkout's owner, with the DITTOFS_E2E_LIVE_* variables
#          from the env file passed through the environment, never on a command line;
#   shell  canary.sh in the canary image.
# Writes logs/canary-<ts>.log, status.txt (the last verdict line), last.json and
# history.jsonl under CANARY_STATE, and keeps 14 days of logs. It also writes the last
# pass as Prometheus metrics (metrics.jq) to CANARY_METRICS_DIR (CANARY_STATE/metrics),
# for node_exporter's textfile collector (see monitoring/). Exits like the pass (0 PASS,
# 1 FAIL); 0 without running if the previous pass is still going.
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
METRICS_DIR="${CANARY_METRICS_DIR:-$STATE/metrics}"
log="$STATE/logs/canary-$(date +%Y%m%d-%H%M%S).log"
touch "$STATE/.pass-start"

# finish RC: the metrics, the log rotation, the exit. The metrics come from last.json
# when this pass wrote it (newer than .pass-start), else from a bare FAIL record. The file
# is written whole and renamed into place, so a scrape never reads half of it, and is
# world-readable (node_exporter runs as nobody; it holds no secrets).
finish() {
    local src="$STATE/last.json" tmp
    if [[ ! "$src" -nt "$STATE/.pass-start" ]]; then
        src="$(mktemp)"
        jq -n --arg t "$(date -u +%FT%TZ)" --arg s "${CANARY_SHARE:-$(sed -n 's/^CANARY_SHARE=//p' "$ENV_FILE")}" \
            '{status: "FAIL", time: $t, share: $s}' >"$src"
    fi
    install -d -m 0755 "$METRICS_DIR"
    tmp="$(mktemp "$METRICS_DIR/.dittofs_canary.XXXXXX")"
    if jq -r --arg impl "$IMPL" -f "$HERE/metrics.jq" "$src" >"$tmp"; then
        chmod 0644 "$tmp" && mv -f "$tmp" "$METRICS_DIR/dittofs_canary.prom"
    else
        rm -f "$tmp"; echo "canary: metrics not written" >&2
    fi
    [[ "$src" == "$STATE/last.json" ]] || rm -f "$src"
    find "$STATE/logs" -name 'canary-*.log' -mtime +14 -delete 2>/dev/null
    exit "$1"
}

if [[ "$IMPL" == go ]]; then
    REPO="$(cd "$HERE/../../.." && pwd)"
    owner="$(stat -c %U "$REPO")"
    # /tmp/dtc is the path dtc shares with its container: the result comes back
    # through it, and two binaries go in. The server's own dfsctl (the client that
    # matches the server; the test would otherwise build this checkout's), and the
    # operators' rclone that setup copied, for `rclone size` at each checkpoint.
    if [[ ! -x "$STATE/bin/rclone" ]]; then
        : >"$log"
        echo "CANARY FAIL impl=go: $STATE/bin/rclone missing (run setup-canary.sh with CANARY_RCLONE_BIN)" | tee "$STATE/status.txt" >>"$log"
        finish 1
    fi
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
        jq -r '"CANARY \(.status) run=\(.run) impl=go \(.rclone // "rclone ?") files=\(.files) bytes=\(.bytes) rclone_size(before/written/deleted/gc)=\([.rclone_size.before, .rclone_size.written, .rclone_size.deleted, .rclone_size.gc] | map(.objects // "-") | join("/")) objects \([.rclone_size.before, .rclone_size.written, .rclone_size.deleted, .rclone_size.gc] | map(.blocks // "-") | join("/")) blocks seconds=\(.seconds.total | floor)" + (if .status == "FAIL" then " failed_step=\(.step)" else "" end)' \
            "$result" >"$STATE/status.txt"
    else
        echo "CANARY FAIL impl=go: the test wrote no result (build or setup failure; see $log)" >"$STATE/status.txt"
        ((rc == 0)) && rc=1
    fi
    rm -f "$result" "$dfsctl" "$rclone"
    finish "$rc"
fi

# Rebuild the image when the Dockerfile changes.
want="$(sha256sum "$HERE/Dockerfile" | cut -c1-16)"
have="$(docker image inspect -f '{{index .Config.Labels "canary.dockerfile"}}' "$IMAGE" 2>/dev/null || true)"
if [[ "$want" != "$have" ]]; then
    if ! docker build -q --label "canary.dockerfile=$want" -t "$IMAGE" -f "$HERE/Dockerfile" "$HERE" >"$log" 2>&1; then
        echo "CANARY FAIL impl=shell: canary image build failed (see $log)" >"$STATE/status.txt"
        finish 2
    fi
fi

docker run --rm --privileged --network host --name "dittofs-canary-$$" --env-file "$ENV_FILE" \
    -v /etc/localtime:/etc/localtime:ro \
    -v "$HERE/canary.sh:/canary/canary.sh:ro" -v "$STATE/bin:/canary/bin:ro" \
    -v "$STATE:/out" "$IMAGE" /canary/canary.sh >"$log" 2>&1
rc=$?
tail -1 "$log" >"$STATE/status.txt"
finish "$rc"
