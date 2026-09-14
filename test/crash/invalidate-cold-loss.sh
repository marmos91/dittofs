#!/usr/bin/env bash
# Cold-read loss rig: does an evicted range whose block store can no longer
# supply it read back as zeros?
#
# Once the syncer has mirrored a range to the block store, eviction drops the
# journal's copy and the block store holds the only one. If that copy then goes
# away, the read has nothing to serve and must say so. Handing the range back
# zero-filled at its recorded length is the bug: the right size, the wrong
# bytes, and no error at any layer for a client to notice.
#
# The rig makes the block store lose the data on purpose. A `memory` block
# store lives inside the server process, so a restart empties it while the
# journal and the metadata store persist on disk. That leaves exactly the state
# under test — manifest rows describing chunks, no journal copy, no block store
# copy — without touching any file the server owns.
#
#   1. write self-identifying 4096-byte records over SMB
#   2. wait until the syncer reports nothing unsynced
#   3. read one record back byte-exact: proof it was ever really there
#   4. evict, so the block store holds the only copy
#   5. restart, emptying the block store
#   6. read the same record again
#
# Verdict: step 6 must fail. Zeros — or any other wrong bytes — at full record
# length is the bug. The correct bytes mean the copy this run set out to
# destroy was never the only one, so nothing was ever at risk and the run
# graded nothing: that is reported as INCONCLUSIVE, not as a pass.
#
# Requires: root, Linux, cifs-utils, python3.
#
#   sudo ./invalidate-cold-loss.sh /path/to/dfs /path/to/dfsctl

set -u

DFS="${1:?path to dfs binary required}"
DFSCTL="${2:?path to dfsctl binary required}"

export PATH="/usr/sbin:/sbin:/usr/bin:/bin:$PATH"

WORK=/var/tmp/dfs-invalidate
DATA=$WORK/data
API=18098
SMBP=12456
RECORD=4096
RECORDS=1024             # 4 MiB
TARGET_REC=200           # the record the verdict is read from

rand() { head -c "$1" /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "$1"; }
PW=$(rand 24); SECRET=$(rand 48)

log() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }

# The server is killed before the mount is torn down, so the unmount is lazy: a
# plain umount would block on a reply that is never coming.
umount_smb() { umount -l "$WORK/smb" 2>/dev/null; }

start_server() {
  DITTOFS_ADMIN_INITIAL_PASSWORD=$PW setsid "$DFS" start --foreground \
    --config "$WORK/config.yaml" --pid-file "$WORK/dfs.pid" \
    --log-file "$WORK/dfs.log" >>"$WORK/dfs.out" 2>&1 &
  for _ in {1..150}; do
    curl -sf "http://127.0.0.1:$API/health" >/dev/null 2>&1 && return 0
    sleep 0.3
  done
  return 1
}

# A graceful stop, because shutdown flushes are honest here: the loss under
# test is the block store's, and no shutdown can write an in-memory block store
# back to disk.
stop_server() {
  local pid
  pid=$(cat "$WORK/dfs.pid" 2>/dev/null) || return 0
  [ -n "$pid" ] || return 0
  kill -TERM "$pid" 2>/dev/null
  for _ in {1..150}; do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.2; done
  kill -9 "$pid" 2>/dev/null
  return 0
}

mount_smb() {
  for _ in {1..40}; do
    mount -t cifs "//127.0.0.1/cold" "$WORK/smb" \
      -o "port=$SMBP,username=admin,password=$PW,vers=3.1.1,cache=none" 2>/dev/null && return 0
    sleep 0.5
  done
  return 1
}

dctl() { XDG_CONFIG_HOME="$WORK/cfg" "$DFSCTL" "$@"; }

cleanup() {
  umount_smb
  stop_server
}
trap cleanup EXIT INT TERM

# --- setup -----------------------------------------------------------------
cleanup
rm -rf "$WORK"; mkdir -p "$WORK/cfg" "$DATA/meta" "$DATA/blocks" "$WORK/smb"

cat > "$WORK/config.yaml" <<CFG
logging: {level: INFO, format: text, output: $WORK/dfs.log}
controlplane:
  host: 127.0.0.1
  port: $API
  jwt: {secret: "$SECRET"}
database: {type: sqlite, sqlite: {path: "$WORK/controlplane.db"}}
# dirty_expire forces the carve promptly, so the records reach the block store
# (and become evictable) without the run waiting out the default age.
blockstore:
  journal:
    path: $DATA/blocks
    dirty_expire: 2s
CFG

start_server || fail "server never became ready"
dctl login --server "http://127.0.0.1:$API" --username admin --password "$PW" >/dev/null \
  || fail "login"
dctl store metadata add --name meta --type badger --db-path "$DATA/meta" >/dev/null \
  || fail "metadata store"

# The block store is in-memory precisely so that a restart destroys it. It is
# the only copy the run is allowed to take away: the journal and the metadata
# store stay on disk and are never touched.
dctl store block add --name blk --type memory >/dev/null || fail "block store"

dctl share create --name /cold --metadata meta --block-store blk \
  --default-permission read-write >/dev/null || fail "share"
dctl adapter enable smb --port $SMBP >/dev/null || fail "smb adapter"
mount_smb || fail "cifs mount"

# --- write -----------------------------------------------------------------
cat > "$WORK/records.py" <<'PY'
import hashlib, os, sys


def body(i, record):
    head = ("REC%08d" % i).encode()
    m = head + hashlib.sha256(head).digest()
    return m + b"\xa5" * (record - len(m))


if sys.argv[1] == "write":
    path, record, count = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
    with open(path, "wb", buffering=0) as f:
        for i in range(count):
            f.write(body(i, record))
        os.fsync(f.fileno())
elif sys.argv[1] == "classify":
    path, record, i = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
    data = open(path, "rb").read()
    if len(data) < record:
        print("short")
    elif data == body(i, record):
        print("exact")
    elif data == b"\x00" * record:
        print("zeros")
    else:
        print("garbage")
else:
    sys.exit("unknown mode %s" % sys.argv[1])
PY

python3 "$WORK/records.py" write "$WORK/smb/cold.bin" "$RECORD" "$RECORDS" || fail "write"
log "wrote $((RECORD * RECORDS)) bytes"

# --- wait for the block store to hold every byte ---------------------------
# Eviction only drops what is already synced, so a run that evicts too early
# leaves the journal holding the data and destroys nothing.
# Polling is quiet, but the last error is kept rather than discarded: a run
# that never sees a clean reading has to be able to say why.
stats() { dctl store block stats --share /cold -o json 2>"$WORK/stats.err"; }
totals() { python3 -c "import json,sys; print(json.load(sys.stdin)['totals']['$1'])"; }

for _ in {1..120}; do
  s=$(stats) || { sleep 1; continue; }
  unsynced=$(echo "$s" | totals unsynced_bytes) || { sleep 1; continue; }
  remote=$(echo "$s" | totals blocks_remote) || { sleep 1; continue; }
  [ "$unsynced" -eq 0 ] && [ "$remote" -gt 0 ] && break
  sleep 1
done
if [ "${unsynced:-1}" -ne 0 ] || [ "${remote:-0}" -le 0 ]; then
  cat "$WORK/stats.err" 2>/dev/null
  fail "syncer never mirrored the records to the block store (unsynced=${unsynced:-?} remote_blocks=${remote:-?})"
fi
log "block store holds $remote blocks, nothing unsynced"

# --- control: the record is really there ------------------------------------
# Read before anything is destroyed. A record that cannot be read here would
# make every later result meaningless, so this is checked rather than logged.
probe() {
  local rec=$1
  local out=$WORK/probe.$rec
  if dd if="$WORK/smb/cold.bin" of="$out" bs=$RECORD skip="$rec" count=1 2>/dev/null; then
    python3 "$WORK/records.py" classify "$out" "$RECORD" "$rec"
  else
    echo "refused"
  fi
}

pre=$(probe "$TARGET_REC")
[ "$pre" = "exact" ] || fail "target record read back as '$pre' before the loss"
log "target record reads back byte-exact"

# --- evict, so the block store holds the only copy --------------------------
# The verdict rests on the eviction actually happening, so its exit status is
# checked rather than piped away: an eviction that quietly failed would leave
# the record on disk, and the rig would then grade a run in which nothing was
# lost. Both tiers go, read buffer included — a cached copy would answer the
# probe below without ever reaching the block store.
if ! dctl store block evict --share /cold >"$WORK/evict.out" 2>&1; then
  cat "$WORK/evict.out"
  fail "evict"
fi
tail -3 "$WORK/evict.out"
log "evicted"

# --- restart, emptying the block store --------------------------------------
umount_smb
stop_server
start_server || fail "server did not come back"
mount_smb || fail "cifs remount"
log "restarted with an empty block store"

# --- verify -----------------------------------------------------------------
target=$(probe "$TARGET_REC")
size=$(stat -c %s "$WORK/smb/cold.bin" 2>/dev/null || echo 0)
want=$((RECORD * RECORDS))
{
  echo "target_record=$TARGET_REC read=$target file_bytes=$size expected_bytes=$want"
  if [ "$size" -ne "$want" ]; then
    # The file's length is metadata, and metadata was never at risk here. A
    # different length means the run destroyed something it did not mean to,
    # and the content verdict below would be reading the wrong offsets.
    echo "VERDICT=INCONCLUSIVE (file length changed across the restart)"
  elif [ "$target" = "refused" ]; then
    echo "VERDICT=PASS"
  elif [ "$target" = "exact" ]; then
    # Nothing was lost, so nothing was tested: some tier still held the bytes
    # the run set out to take away.
    echo "VERDICT=INCONCLUSIVE (record survived; the block store was not the only copy)"
  else
    echo "VERDICT=FAIL"
  fi
} | tee "$WORK/result.txt"
grep -q "VERDICT=PASS" "$WORK/result.txt"
