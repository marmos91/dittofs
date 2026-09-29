#!/usr/bin/env bash
# DittoFS canary: one end-to-end pass against a running server, through the Linux
# kernel SMB client (CIFS). It checks every step of the data path:
#   write files -> read them back -> list them -> wait for the upload -> the new
#   objects are in the S3 bucket -> evict the server's caches and read again from
#   the remote -> delete the files -> GC with grace 0 -> the objects are gone.
# The canary's prefix is measured with `rclone size` (object count and bytes) at four
# checkpoints: before writing (must be 0), after the upload, after the delete (objects
# stay until GC), and after GC (must be 0 again).
# The files are random, so every chunk is unique: repeated content is held by the
# dedup adoption guard for up to an hour after a delete, which GC grace 0 does not
# override (a delay, not a leak), and would fail this check for a known reason.
#
# Runs inside the canary container (privileged, host network), from run-canary.sh.
# Configuration comes from the environment (the env file setup-canary.sh writes):
#   CANARY_API, CANARY_SMB_HOST, CANARY_SMB_PORT, CANARY_SHARE, CANARY_SMB_USER,
#   CANARY_SMB_PASS, CANARY_OPS_USER, CANARY_OPS_PASS (an admin: GC and evict are
#   admin operations), CANARY_S3_BUCKET, CANARY_S3_PREFIX, and an rclone remote named
#   "canarys3" (RCLONE_CONFIG_CANARYS3_*) for reading the bucket.
# Exit 0 and a final "CANARY PASS" line, or exit 1 and "CANARY FAIL step=...".
set -uo pipefail
API="${CANARY_API:-http://127.0.0.1:8080}"
SMB_HOST="${CANARY_SMB_HOST:-127.0.0.1}"
SMB_PORT="${CANARY_SMB_PORT:-12445}"
SHARE="${CANARY_SHARE:-/canary}"
SIZES_MIB="${CANARY_SIZES_MIB:-1 4 16 33}"
UPLOAD_TIMEOUT="${CANARY_UPLOAD_TIMEOUT:-300}"
GC_TIMEOUT="${CANARY_GC_TIMEOUT:-120}"
BUCKET="${CANARY_S3_BUCKET:?}"; PREFIX="${CANARY_S3_PREFIX:?}"
: "${CANARY_SMB_USER:?}" "${CANARY_SMB_PASS:?}" "${CANARY_OPS_USER:?}" "${CANARY_OPS_PASS:?}"
DFSCTL="${CANARY_DFSCTL:-/canary/bin/dfsctl}"
# The host's rclone when setup copied one (the same binary the operators use), else the image's.
RCLONE="${CANARY_RCLONE:-/canary/bin/rclone}"; [[ -x "$RCLONE" ]] || RCLONE=rclone
OUT="${CANARY_OUT:-/out}"
MNT=/mnt/canary
WORK="$(mktemp -d)"
RUN="run-$(date -u +%Y%m%dT%H%M%SZ)-$$"
export XDG_CONFIG_HOME="$WORK/dfsctl"
T0=$SECONDS STEP=start
declare -A T S_COUNT S_BYTES S_BLOCKS S_BBYTES SRV

log() { printf '%s %-12s %s\n' "$(date '+%F %T')" "$STEP" "$*"; }
step() { T[$STEP]=$((SECONDS - ${T_START:-$T0})); STEP="$1"; T_START=$SECONDS; log "begin"; }
result() { # result STATUS MESSAGE
    local status="$1" msg="$2" timings=""
    T[$STEP]=$((SECONDS - ${T_START:-$T0}))
    for k in "${!T[@]}"; do timings+="\"$k\":${T[$k]},"; done
    mkdir -p "$OUT" 2>/dev/null
    local sizes="" srv="" c
    for c in before written deleted gc; do
        [[ -n "${S_COUNT[$c]:-}" ]] && sizes+="\"$c\":{\"objects\":${S_COUNT[$c]},\"bytes\":${S_BYTES[$c]},\"blocks\":${S_BLOCKS[$c]},\"block_bytes\":${S_BBYTES[$c]}},"
        [[ -n "${SRV[$c]:-}" ]] && srv+="\"$c\":${SRV[$c]},"
    done
    printf '{"time":"%s","run":"%s","status":"%s","step":"%s","message":%s,"share":"%s","files":%s,"bytes":%s,"uploaded_objects":%s,"chunks_swept":%s,"rclone_size":{%s},"server_blocks":{%s},"seconds":{%s"total":%s}}\n' \
        "$(date -u +%FT%TZ)" "$RUN" "$status" "$STEP" "$(jq -Rn --arg m "$msg" '$m')" "$SHARE" \
        "${NFILES:-0}" "${NBYTES:-0}" "${NNEW:-0}" "${SWEPT:-null}" "${sizes%,}" "${srv%,}" "$timings" "$((SECONDS - T0))" |
        tee -a "$OUT/history.jsonl" >"$OUT/last.json" 2>/dev/null || true
}
fail() {
    log "FAILED: $*"
    result FAIL "$*"
    echo "CANARY FAIL step=$STEP run=$RUN: $*"
    exit 1
}
cleanup() {
    [[ -d "$MNT/$RUN" ]] && rm -rf "${MNT:?}/$RUN" 2>/dev/null
    mountpoint -q "$MNT" && { umount "$MNT" 2>/dev/null || umount -l "$MNT" 2>/dev/null; }
    rm -rf "$WORK"
}
trap cleanup EXIT
sha() { sha256sum "$1" | cut -c1-64; }
s3keys() { "$RCLONE" lsf -R --files-only "canarys3:$BUCKET/$PREFIX" 2>/dev/null | sort; }
s3sizes() { "$RCLONE" lsl "canarys3:$BUCKET/$PREFIX" 2>/dev/null | awk '{print $NF, $1}' | sort; }
# checkpoint NAME: `rclone size` of the canary's prefix and of its blocks/ directory
# (the S3 block store writes nothing else there), and the server's block counts for the
# share, recorded and logged. rclone runs here, not in $(...) of a helper, so fail exits.
checkpoint() {
    local j jb c b bc bb s
    j="$("$RCLONE" size --json "canarys3:$BUCKET/$PREFIX" 2>"$WORK/size.err")" &&
        jb="$("$RCLONE" size --json "canarys3:$BUCKET/${PREFIX}blocks/" 2>"$WORK/size.err")" ||
        fail "rclone size failed: $(tr '\n' ' ' <"$WORK/size.err")"
    c="$(jq -r .count <<<"$j")" b="$(jq -r .bytes <<<"$j")" bc="$(jq -r .count <<<"$jb")" bb="$(jq -r .bytes <<<"$jb")"
    S_COUNT[$1]="$c" S_BYTES[$1]="$b" S_BLOCKS[$1]="$bc" S_BBYTES[$1]="$bb"
    log "rclone size canarys3:$BUCKET/$PREFIX [$1]: $c object(s), $b bytes ($(numfmt --to=iec-i --suffix=B "$b")); blocks/: $bc block(s), $bb bytes"
    ((bc == c)) || log "note: $((c - bc)) object(s) outside blocks/, where the S3 block store writes nothing"
    s="$("$DFSCTL" store block stats --share "$SHARE" -o json 2>/dev/null | jq -c '.totals | {blocks_remote, blocks_local, blocks_total}')"
    if [[ -n "$s" ]]; then SRV[$1]="$s"; log "server [$1]: $(jq -r 'to_entries | map("\(.key)=\(.value)") | join(" ")' <<<"$s")"; fi
}
stat_of() { "$DFSCTL" store block stats --share "$SHARE" -o json 2>/dev/null | jq -r ".totals.$1 // empty"; }

log "run $RUN: share $SHARE via //$SMB_HOST:$SMB_PORT, remote $BUCKET/$PREFIX, files (MiB): $SIZES_MIB"

step preflight
curl -sf "$API/health" >/dev/null || fail "DittoFS API $API is not answering"
"$DFSCTL" login --server "$API" --username "$CANARY_OPS_USER" --password "$CANARY_OPS_PASS" >/dev/null 2>&1 ||
    fail "dfsctl login as $CANARY_OPS_USER failed"
"$DFSCTL" share show "$SHARE" >/dev/null 2>&1 || fail "share $SHARE not found"
"$RCLONE" lsf "canarys3:$BUCKET" --max-depth 1 >/dev/null 2>&1 || fail "cannot list bucket $BUCKET"
log "$("$RCLONE" version 2>/dev/null | head -1) at $RCLONE; $("$DFSCTL" version 2>/dev/null | head -1)"

step mount
mkdir -p "$MNT"
if ! mountpoint -q "$MNT"; then
    cred="$WORK/smb.cred"
    (umask 077; printf 'username=%s\npassword=%s\n' "$CANARY_SMB_USER" "$CANARY_SMB_PASS" >"$cred")
    # cache=none: every read goes to the server, so the reads below test DittoFS, not
    # the client's page cache.
    mount -t cifs "//$SMB_HOST/${SHARE#/}" "$MNT" \
        -o "port=$SMB_PORT,credentials=$cred,vers=3.1.1,cache=none,uid=0,gid=0" 2>"$WORK/mount.err" ||
        fail "mount.cifs failed: $(tr '\n' ' ' <"$WORK/mount.err")"
fi
touch "$MNT/.canary-probe" 2>/dev/null && rm -f "$MNT/.canary-probe" || fail "the SMB mount is not writable"

step leftovers
left=0
for d in "$MNT"/run-*; do [[ -e "$d" ]] && { rm -rf "$d" && left=$((left + 1)); }; done
((left > 0)) && log "removed $left run dir(s) a failed earlier run left behind"

step before
checkpoint before
if ((S_COUNT[before] != 0)); then
    # A failed earlier pass can leave objects (its files were removed above): GC once.
    log "not empty before writing; running GC (grace 0) to clear what an earlier pass left"
    "$DFSCTL" store block gc "$SHARE" --grace-period 0 >/dev/null 2>&1 || fail "the cleanup GC failed"
    for ((i = 0; i < GC_TIMEOUT; i += 2)); do checkpoint before; ((S_COUNT[before] == 0)) && break; sleep 2; done
    ((S_COUNT[before] == 0)) || fail "the canary prefix holds ${S_COUNT[before]} object(s) before writing (should be 0)"
fi
s3keys >"$WORK/before" || fail "listing the remote failed"

step write
mkdir "$MNT/$RUN" || fail "mkdir $RUN failed"
NFILES=0 NBYTES=0
for m in $SIZES_MIB; do
    f="f${m}MiB.bin"
    head -c $((m * 1048576)) /dev/urandom >"$WORK/$f"
    sha "$WORK/$f" >"$WORK/$f.sha"
    cp "$WORK/$f" "$MNT/$RUN/$f" || fail "writing $f failed"
    NFILES=$((NFILES + 1)) NBYTES=$((NBYTES + m * 1048576))
done
sync
log "wrote $NFILES files, $NBYTES bytes"

step read
# Copy, then hash: hashing the mount directly would hide an I/O error (a partial read
# hashes fine and exits 0).
for m in $SIZES_MIB; do
    f="f${m}MiB.bin"
    cp "$MNT/$RUN/$f" "$WORK/read" 2>"$WORK/read.err" || fail "reading $f failed: $(tr '\n' ' ' <"$WORK/read.err")"
    [[ "$(sha "$WORK/read")" == "$(cat "$WORK/$f.sha")" ]] || fail "$f read back with different content"
done
log "all $NFILES files read back identical"

step list
for m in $SIZES_MIB; do
    f="f${m}MiB.bin"
    size="$(stat -c %s "$MNT/$RUN/$f" 2>/dev/null)" || fail "$f missing from the listing"
    [[ "$size" == $((m * 1048576)) ]] || fail "$f lists as $size bytes, wrote $((m * 1048576))"
done
[[ "$(find "$MNT/$RUN" -type f | wc -l)" == "$NFILES" ]] || fail "the directory holds a different number of files"
log "listing matches: $NFILES files with the right sizes"

step upload
for ((i = 0; i < UPLOAD_TIMEOUT; i += 2)); do
    [[ "$(stat_of unsynced_bytes)" == 0 && "$(stat_of pending_uploads)" == 0 ]] && break
    sleep 2
done
((i < UPLOAD_TIMEOUT)) || fail "upload did not finish in ${UPLOAD_TIMEOUT}s (unsynced=$(stat_of unsynced_bytes), pending=$(stat_of pending_uploads))"
s3keys >"$WORK/after"
comm -13 "$WORK/before" "$WORK/after" >"$WORK/new"
NNEW="$(wc -l <"$WORK/new" | tr -d ' ')"
((NNEW > 0)) || fail "no new object appeared under $BUCKET/$PREFIX"
newbytes="$(s3sizes | awk 'NR==FNR {want[$1]; next} ($1 in want) {s += $2} END {print s + 0}' "$WORK/new" -)"
((newbytes >= NBYTES)) || fail "the new objects hold $newbytes bytes, less than the $NBYTES written"
log "uploaded: $NNEW new object(s), $newbytes bytes, in ${i}s"
checkpoint written
((S_COUNT[written] == NNEW)) || fail "rclone size counts ${S_COUNT[written]} object(s), the listing found $NNEW new"
((S_BYTES[written] >= NBYTES)) || fail "rclone size reports ${S_BYTES[written]} bytes, less than the $NBYTES written"

step cold-read
"$DFSCTL" store block evict --share "$SHARE" >/dev/null 2>&1 || fail "dfsctl store block evict failed"
for m in $SIZES_MIB; do
    f="f${m}MiB.bin"
    cp "$MNT/$RUN/$f" "$WORK/read" 2>"$WORK/read.err" || fail "cold read of $f failed: $(tr '\n' ' ' <"$WORK/read.err")"
    [[ "$(sha "$WORK/read")" == "$(cat "$WORK/$f.sha")" ]] || fail "cold read of $f returned different content"
done
log "after evicting the server's caches, all files read back identical from the remote"

step delete
rm -rf "${MNT:?}/$RUN" || fail "deleting the run dir failed"
[[ ! -e "$MNT/$RUN" ]] || fail "the run dir is still listed after delete"
log "deleted"
checkpoint deleted   # expected unchanged: a delete does not remove remote objects; GC does
((S_COUNT[deleted] == S_COUNT[written])) ||
    log "note: ${S_COUNT[written]} -> ${S_COUNT[deleted]} object(s) between the upload and the delete, before any GC"

step gc
gcout="$("$DFSCTL" store block gc "$SHARE" --grace-period 0 -o json 2>&1)" || fail "dfsctl store block gc failed: $gcout"
SWEPT="$(jq -r '.objects_swept // .stats.objects_swept // empty' <<<"$gcout" 2>/dev/null)"
[[ -n "$SWEPT" ]] || SWEPT=null
for ((i = 0; i < GC_TIMEOUT; i += 2)); do
    s3keys >"$WORK/now"
    [[ -z "$(comm -12 "$WORK/new" "$WORK/now")" ]] && break
    sleep 2
done
left="$(comm -12 "$WORK/new" "$WORK/now" | wc -l | tr -d ' ')"
((left == 0)) || fail "$left of the $NNEW uploaded object(s) are still in the bucket ${GC_TIMEOUT}s after GC (swept=$SWEPT)"
log "GC (grace 0) reclaimed $SWEPT dead chunk(s) (GC's objects_swept counts chunks); none of the run's $NNEW block object(s) remain"
checkpoint gc
((S_COUNT[gc] == 0 && S_BYTES[gc] == 0)) || fail "rclone size after GC: ${S_COUNT[gc]} object(s), ${S_BYTES[gc]} bytes (should be 0)"

step finish
result PASS "ok"
echo "CANARY PASS run=$RUN files=$NFILES bytes=$NBYTES rclone_size(before/written/deleted/gc)=${S_COUNT[before]}/${S_COUNT[written]}/${S_COUNT[deleted]}/${S_COUNT[gc]} objects ${S_BLOCKS[before]}/${S_BLOCKS[written]}/${S_BLOCKS[deleted]}/${S_BLOCKS[gc]} blocks ${S_BYTES[before]}/${S_BYTES[written]}/${S_BYTES[deleted]}/${S_BYTES[gc]} bytes chunks_swept=$SWEPT seconds=$((SECONDS - T0))"
