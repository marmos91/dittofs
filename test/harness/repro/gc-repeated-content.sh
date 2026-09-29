#!/usr/bin/env bash
# Reproduction for #2909 ("GC: objects of deleted files are never removed from the S3
# remote"), testing one suspected mechanism:
#   the carver's dedup oracle only knows hashes that are already remote-durable, so a
#   chunk repeated inside ONE carve pass is packed again; CommitBlock sets
#   LiveChunkCount = len(commits) (duplicates included) while GC decrements once per
#   distinct dead hash, so such a block never reaches 0 and is never deleted.
#
# Runs its own dfs (API 18080, NFS 22049, badger metadata, a fresh Localstack bucket),
# so it does not collide with dt suites on 8080/12049. No root.
#   A (control): 16 MiB of unique random data  -> write, sync, delete, GC grace 0
#   B (repeat):  one random 8 MiB block x4     -> write, sync, delete, GC grace 0
# Expected if GC is correct: every blocks/ object created by A and by B is gone.
set -euo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${DITTOFS_REPO:-$(git -C "$HARNESS" rev-parse --show-toplevel)}"
# shellcheck source=lib.sh
source "$HARNESS/repro/lib.sh"
ST="$HARNESS/state/gcrepro"
MNT="$HOME/mnt/dittofs-gc"
API=http://127.0.0.1:18080
NFSPORT=22049
BUCKET="gcrepro-$(date +%s)"
PW=gcrepro-admin-password-123
export XDG_CONFIG_HOME="$ST/dfsctl"
CTL=("$ST/bin/dfsctl")

cleanup() {
    is_mounted "$MNT" && { nfs_umount "$MNT" || true; }
    [[ -n "${PID:-}" ]] && kill "$PID" 2>/dev/null && wait "$PID" 2>/dev/null || true
}
trap cleanup EXIT

s3keys() { curl -s "http://127.0.0.1:4566/$BUCKET?list-type=2&prefix=blocks/" | grep -oE '<Key>[^<]+</Key>' | sed -E 's/<\/?Key>//g' | sort || true; }
stat_of() { "${CTL[@]}" store block stats --share /gc -o json | jq -r ".totals.$1"; }
wait_synced() {
    for _ in $(seq 1 120); do
        [[ "$(stat_of unsynced_bytes)" == 0 && "$(stat_of pending_uploads)" == 0 ]] && return 0
        sleep 1
    done
    echo "upload did not settle" >&2; return 1
}
gc_now() { "${CTL[@]}" store block gc /gc --grace-period 0 >/dev/null; }

rm -rf "$ST"; mkdir -p "$ST" "$MNT"
mkdir -p "$ST/bin"; (cd "$REPO" && go build -o "$ST/bin/dfs" ./cmd/dfs && go build -o "$ST/bin/dfsctl" ./cmd/dfsctl)
cat >"$ST/config.yaml" <<EOF
logging: {level: INFO, format: text, output: stdout}
shutdown_timeout: 10s
database: {type: sqlite, sqlite: {path: "$ST/controlplane.db"}}
controlplane: {port: 18080, jwt: {secret: "gcrepro-secret-key-at-least-32-characters"}}
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
# A fresh dfs also enables SMB on 12445, the port the dt SMB suites need; this repro
# only uses NFS, so free it.
"${CTL[@]}" adapter disable smb >/dev/null 2>&1 || true
for _ in $(seq 1 30); do nc -z 127.0.0.1 "$NFSPORT" 2>/dev/null && break; sleep 1; done
nfs3_mount 127.0.0.1:/gc "$MNT" "$NFSPORT"
echo "server up: $API, NFS :$NFSPORT, bucket $BUCKET, mount $MNT"

phase() { # phase NAME FILEPATH
    local name="$1" src="$2" before after created left
    before="$(s3keys)"
    cp "$src" "$MNT/$name.bin"
    wait_synced
    after="$(s3keys)"
    local chunks; chunks="$(stat_of blocks_total)"
    created="$(comm -13 <(echo "$before") <(echo "$after") | grep -c . || true)"
    rm "$MNT/$name.bin"
    sleep 2
    gc_now
    left="$(comm -12 <(comm -13 <(echo "$before") <(echo "$after")) <(s3keys) | grep -c . || true)"
    printf '%-9s %6s MiB  chunk rows: %-4s blocks/ objects created: %-3s  still present after delete + GC(grace 0): %s\n' \
        "$name" "$(( $(wc -c <"$src") / 1048576 ))" "$chunks" "$created" "$left"
    echo "$name $created $left" >>"$ST/result"
}

head -c 16777216 /dev/urandom >"$ST/unique.bin"
head -c 8388608 /dev/urandom >"$ST/a.bin"
cat "$ST/a.bin" "$ST/a.bin" "$ST/a.bin" "$ST/a.bin" >"$ST/repeat.bin"
phase control "$ST/unique.bin"
phase repeat "$ST/repeat.bin"
head -c 67108864 /dev/zero >"$ST/zeros.bin"
pre_zeros="$(s3keys)"
phase zeros "$ST/zeros.bin"
zkeys="$(comm -13 <(echo "$pre_zeros") <(s3keys))"
for pass in 2 3; do gc_now; echo "  zeros: after GC pass $pass: $(comm -12 <(echo "$zkeys") <(s3keys) | grep -c . || true) of the leftover still present"; done
"${CTL[@]}" store block gc /gc --reconcile >/dev/null; gc_now
echo "  zeros: after GC --reconcile + GC(grace 0): $(comm -12 <(echo "$zkeys") <(s3keys) | grep -c . || true) still present"
echo "  audit-refcounts: $("${CTL[@]}" store block audit-refcounts /gc -o json 2>/dev/null | jq -c . 2>/dev/null | cut -c1-300 || echo n/a)"
echo "gc last run: $("${CTL[@]}" store block gc-status /gc -o json 2>/dev/null | jq -c '{objects_swept, bytes_freed, error_count}' 2>/dev/null || echo n/a)"
curl -s -o /dev/null -X DELETE "http://127.0.0.1:4566/$BUCKET" || true
