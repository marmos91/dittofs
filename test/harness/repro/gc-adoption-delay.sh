#!/usr/bin/env bash
# Is the zero-file "leak" of #2909 permanent, or a delay?
#
# The carve dedup path records an adoption in gc's dedupSweepGuard, and an adoption
# blocks the sweep from reclaiming that hash for dedupAdoptionMaxAge (1h), whatever
# --grace-period says. A file with repeated content (zeros) has its repeats deduped,
# so right after deletion a GC reclaims nothing for it. Auto-GC is on by default
# (every 15 min, cmd/dfs/commands/start.go). If the hold is the whole story, the
# block disappears on the first auto-GC after the hour; if it stays, it is a leak.
#
# Runs its own dfs (API 18081, NFS 22050), writes 64 MiB of zeros over NFS, deletes
# it, runs one manual GC at grace 0, then only watches the bucket every 5 min for
# 90 min, touching nothing. Holds state/gcdelay.lock so `dt cleanup` waits for it.
set -euo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${DITTOFS_REPO:-$(git -C "$HARNESS" rev-parse --show-toplevel)}"
# shellcheck source=lib.sh
source "$HARNESS/repro/lib.sh"
ST="$HARNESS/state/gcdelay"
LOCKD="$HARNESS/state/gcdelay.lock"
MNT="$HOME/mnt/dittofs-gcdelay"
API=http://127.0.0.1:18081
NFSPORT=22050
BUCKET="gcdelay-$(date +%s)"
PW=gcdelay-admin-password-123
WATCH_MIN="${WATCH_MIN:-80}"
SUITE_LOCK="$HARNESS/state/suite.lock"
export XDG_CONFIG_HOME="$ST/dfsctl"

mkdir "$LOCKD" 2>/dev/null || { echo "another gc-adoption-delay run holds $LOCKD" >&2; exit 2; }
echo "gc-adoption-delay" >"$LOCKD/owner"; echo "$$" >"$LOCKD/pid"; dt_where >"$LOCKD/where"
# Also hold the host-suite lock: test/posix/setup-posix.sh (used by dt pynfs/posix)
# runs `pkill -f "dfs start"`, which would kill this experiment's server. The first
# run of this experiment was invalidated exactly that way.
if ! mkdir "$SUITE_LOCK" 2>/dev/null; then rm -rf "$LOCKD"; echo "a dt suite is running ($SUITE_LOCK)" >&2; exit 2; fi
echo "gc-adoption-delay" >"$SUITE_LOCK/owner"; echo "$$" >"$SUITE_LOCK/pid"; dt_where >"$SUITE_LOCK/where"
cleanup() {
    is_mounted "$MNT" && { nfs_umount "$MNT" || true; }
    [[ -n "${PID:-}" ]] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null || true
    rm -rf "$LOCKD" "$SUITE_LOCK"
}
trap cleanup EXIT

s3keys() { curl -s "http://127.0.0.1:4566/$BUCKET?list-type=2&prefix=blocks/" | grep -oE '<Key>[^<]+</Key>' | sed -E 's/<\/?Key>//g' | sort || true; }
CTL=("$ST/bin/dfsctl")
stat_of() { "${CTL[@]}" store block stats --share /gc -o json | jq -r ".totals.$1"; }

rm -rf "$ST"; mkdir -p "$ST/bin" "$MNT"
(cd "$REPO" && go build -o "$ST/bin/dfs" ./cmd/dfs && go build -o "$ST/bin/dfsctl" ./cmd/dfsctl)
cat >"$ST/config.yaml" <<EOF
logging: {level: INFO, format: text, output: stdout}
shutdown_timeout: 10s
database: {type: sqlite, sqlite: {path: "$ST/controlplane.db"}}
controlplane: {port: 18081, jwt: {secret: "gcdelay-secret-key-at-least-32-characters"}}
blockstore: {journal: {path: "$ST/blocks"}}
EOF
DITTOFS_ADMIN_INITIAL_PASSWORD="$PW" "$ST/bin/dfs" start --foreground --config "$ST/config.yaml" \
    --pid-file "$ST/dfs.pid" >"$ST/server.log" 2>&1 &
PID=$!
for _ in $(seq 1 60); do curl -sf "$API/health" >/dev/null && break; sleep 1; done
"${CTL[@]}" login --server "$API" --username admin --password "$PW" >/dev/null
curl -s -o /dev/null -X PUT "http://127.0.0.1:4566/$BUCKET"
"${CTL[@]}" store metadata add --name md --type badger --config "{\"db_path\":\"$ST/metadata\"}" >/dev/null
"${CTL[@]}" store block add --name s3 --type s3 --config "{\"bucket\":\"$BUCKET\",\"region\":\"us-east-1\",\"endpoint\":\"http://localhost:4566\",\"force_path_style\":true,\"access_key_id\":\"test\",\"secret_access_key\":\"test\",\"allow_private_endpoint\":true}" >/dev/null
"${CTL[@]}" share create --name /gc --metadata md --block-store s3 --default-permission read-write >/dev/null
"${CTL[@]}" adapter enable nfs --port "$NFSPORT" >/dev/null
# A fresh dfs also enables SMB on 12445 (the dt SMB suites' port); only NFS is used here.
"${CTL[@]}" adapter disable smb >/dev/null 2>&1 || true
for _ in $(seq 1 30); do nc -z 127.0.0.1 "$NFSPORT" 2>/dev/null && break; sleep 1; done
nfs3_mount 127.0.0.1:/gc "$MNT" "$NFSPORT"

t0=$(date +%s)
ts() { printf '%s (+%3d min)' "$(date '+%H:%M:%S')" "$(( ($(date +%s) - t0) / 60 ))"; }
head -c 67108864 /dev/zero >"$MNT/zeros.bin"
for _ in $(seq 1 120); do [[ "$(stat_of unsynced_bytes)" == 0 && "$(stat_of pending_uploads)" == 0 ]] && break; sleep 1; done
echo "$(ts) wrote 64 MiB zeros; blocks/ objects: $(s3keys | grep -c . || true)"
rm "$MNT/zeros.bin"; sleep 2
"${CTL[@]}" store block gc /gc --grace-period 0 >/dev/null
echo "$(ts) deleted + manual GC (grace 0); blocks/ objects: $(s3keys | grep -c . || true)"

for ((m = 5; m <= WATCH_MIN; m += 5)); do
    sleep 300
    "${CTL[@]}" login --server "$API" --username admin --password "$PW" >/dev/null 2>&1 || true  # tokens last 15 min
    echo "$(ts) blocks/ objects: $(s3keys | grep -c . || true)   last GC: $("${CTL[@]}" store block gc-status /gc -o json 2>/dev/null | jq -c '{started_at, objects_swept, bytes_freed}' 2>/dev/null)   server alive: $(kill -0 "$PID" 2>/dev/null && echo yes || echo NO)"
done
left=$(s3keys | grep -c . || true)
echo "$(ts) RESULT: $left blocks/ object(s) left after ${WATCH_MIN} min of auto-GC ($( ((left == 0)) && echo 'reclaimed: a delay, not a leak' || echo 'NOT reclaimed: a leak'))"
curl -s -o /dev/null -X DELETE "http://127.0.0.1:4566/$BUCKET" || true
