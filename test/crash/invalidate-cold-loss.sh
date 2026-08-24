#!/usr/bin/env bash
# Cold-marker durability rig: does a range demoted by a failed corrupt-read heal
# survive the eviction of the segment that still holds it?
#
# The journal demotes a range to cold when a warm read finds its on-disk record
# corrupt, then re-fetches it from the remote store. When that re-fetch fails
# (remote unreachable) the range stays cold in memory while its record is still
# live in a segment on disk. Eviction retires that segment. If the demotion was
# never written to cold.log, a restart finds neither a record nor a marker and
# the range reads back as zeros — right size, no error at any layer.
#
# The rig drives that exact sequence against a real server over SMB:
#
#   1. write self-identifying 4096-byte records, let carve upload them
#   2. restart clean, so every record is warm on disk and no cache is holding it
#   3. corrupt one record's payload in place while the server runs, so the next
#      read of it fails its body CRC with the interval already in the index
#   4. stop the remote, so the heal's re-fetch fails and the range stays cold
#   5. read it — the read must fail loudly; that is the demotion firing
#   6. evict, retiring the segment that held the only copy
#   7. detach the remote tier, restart, and probe the demoted range
#
# Step 7 is what makes the loss observable. While a remote store is attached,
# the engine reconciles a hole against the FileChunk manifest and the bytes come
# back either way — the lost marker costs only a needless fetch. Detached, a
# range that kept its marker fails closed and a range that lost it reads zeros.
#
# Verdict: the demoted range must refuse the read, exactly as the neighbouring
# range that kept its marker does. The demoted range reading back as zeros while
# its neighbour refuses is the bug.
#
# Requires: root, Linux, cifs-utils, python3, a minio binary.
#
# sudo ./invalidate-cold-loss.sh /path/to/dfs /path/to/dfsctl [/path/to/minio]

set -u

DFS="${1:?path to dfs binary required}"
DFSCTL="${2:?path to dfsctl binary required}"
MINIO="${3:-/root/minio}"

export PATH="/usr/sbin:/sbin:/usr/bin:/bin:$PATH"

WORK=/var/tmp/dfs-invalidate
DATA=$WORK/data
BUCKET=dfs
API=18098
SMBP=12456
S3PORT=19000
RECORD=4096
RECORDS=1024          # 4 MiB
TARGET_REC=200        # the record whose on-disk payload gets corrupted

rand() { head -c "$1" /dev/urandom | base64 | tr -dc 'A-Za-z0-9' | head -c "$1"; }
PW=$(rand 24); SECRET=$(rand 48)
S3KEY=minioadmin; S3SECRET=minioadmin

log() { echo "[$(date +%H:%M:%S)] $*"; }
fail() { log "FAIL: $*"; exit 1; }

kill_pidfile() {
  local f=$1 pid
  pid=$(cat "$f" 2>/dev/null) || return 0
  [ -n "$pid" ] || return 0
  kill -9 "$pid" 2>/dev/null
  for _ in {1..100}; do kill -0 "$pid" 2>/dev/null || return 0; sleep 0.1; done
  return 1
}

# The server is killed before its mount is torn down, so the unmount is lazy:
# a plain umount would block on a reply that is never coming.
umount_smb() { umount -l "$WORK/smb" 2>/dev/null; }

start_minio() {
  mkdir -p "$WORK/minio/$BUCKET"
  MINIO_ROOT_USER=$S3KEY MINIO_ROOT_PASSWORD=$S3SECRET \
    setsid "$MINIO" server --address "127.0.0.1:$S3PORT" "$WORK/minio" \
    >"$WORK/minio.log" 2>&1 &
  echo $! > "$WORK/minio.pid"
  for _ in {1..100}; do
    curl -sf "http://127.0.0.1:$S3PORT/minio/health/live" >/dev/null 2>&1 && return 0
    sleep 0.2
  done
  return 1
}
stop_minio() { kill_pidfile "$WORK/minio.pid"; }

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
# Graceful stop, so shutdown flushes whatever it flushes; the bug under test is
# a lost durable marker, not a lost write, so a clean stop is the honest one.
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
  kill_pidfile "$WORK/dfs.pid"
  stop_minio
}
trap cleanup EXIT INT TERM

# --- setup -----------------------------------------------------------------
cleanup
rm -rf "$WORK"; mkdir -p "$WORK/cfg" "$DATA/meta" "$DATA/blocks" "$WORK/smb"

start_minio || fail "minio never became ready"

cat > "$WORK/config.yaml" <<CFG
logging: {level: INFO, format: text, output: $WORK/dfs.log}
controlplane:
  host: 127.0.0.1
  port: $API
  jwt: {secret: "$SECRET"}
database: {type: sqlite, sqlite: {path: "$WORK/controlplane.db"}}
CFG

start_server || fail "server never became ready"
dctl login --server "http://127.0.0.1:$API" --username admin --password "$PW" >/dev/null \
  || fail "login"
dctl store metadata add --name meta --type badger --db-path "$DATA/meta" >/dev/null \
  || fail "metadata store"

# dirty_expire_seconds forces carve to run promptly, so the records are synced to
# the remote (and their segments evictable) without waiting on the default age.
dctl store block local add --name local --type fs \
  --config "{\"path\": \"$DATA/blocks\", \"dirty_expire_seconds\": 2}" >/dev/null \
  || fail "local block store"

dctl store block remote add --name s3 --type s3 \
  --config "{\"bucket\": \"$BUCKET\", \"region\": \"us-east-1\", \"endpoint\": \"http://127.0.0.1:$S3PORT\", \"access_key_id\": \"$S3KEY\", \"secret_access_key\": \"$S3SECRET\", \"allow_private_endpoint\": true}" >/dev/null \
  || fail "remote block store"

dctl share create --name /cold --metadata meta --local local \
  --remote s3 --default-permission read-write >/dev/null || fail "share"
dctl adapter enable smb --port $SMBP >/dev/null || fail "smb adapter"
mount_smb || fail "cifs mount"

# --- write -----------------------------------------------------------------
cat > "$WORK/records.py" <<'PY'
import hashlib, os, sys

def marker(i):
    head = ("REC%08d" % i).encode()
    return head + hashlib.sha256(head).digest()

def body(i, record):
    m = marker(i)
    return m + b"\xa5" * (record - len(m))

if sys.argv[1] == "write":
    path, record, count = sys.argv[2], int(sys.argv[3]), int(sys.argv[4])
    with open(path, "wb", buffering=0) as f:
        for i in range(count):
            f.write(body(i, record))
        os.fsync(f.fileno())
elif sys.argv[1] == "corrupt":
    # Flip a byte of the target record's filler as it sits in a segment. The
    # record's body CRC covers the payload, so the next warm read of it fails
    # verification with the interval already live in the index — which is the
    # only way to reach the demotion this rig tests.
    segdir, m = sys.argv[2], marker(int(sys.argv[3]))
    segs = []
    for root, _, names in os.walk(segdir):
        segs += [os.path.join(root, n) for n in names if n.endswith(".seg")]
    for p in sorted(segs):
        with open(p, "r+b") as f:
            blob = f.read()
            at = blob.find(m)
            if at < 0:
                continue
            spot = at + len(m)
            f.seek(spot)
            f.write(bytes([blob[spot] ^ 0xFF]))
            f.flush()
            os.fsync(f.fileno())
        print("corrupted %s at %d" % (p, spot))
        sys.exit(0)
    sys.exit("marker for record %s not found in any segment" % sys.argv[3])
PY

python3 "$WORK/records.py" write "$WORK/smb/cold.bin" "$RECORD" "$RECORDS" || fail "write"
log "wrote $((RECORD * RECORDS)) bytes"

# Wait for carve to push every byte to the remote: the demotion under test only
# fires on a SYNCED interval, and only a fully-synced segment is evictable.
for _ in {1..120}; do
  up=$(find "$WORK/minio/$BUCKET" -type f 2>/dev/null | wc -l)
  [ "$up" -gt 0 ] && break
  sleep 1
done
[ "$up" -gt 0 ] || fail "carve never uploaded anything to the remote"
sleep 8
log "remote holds $(find "$WORK/minio/$BUCKET" -type f | wc -l) objects"

# --- restart clean, so every read below goes to disk ------------------------
umount_smb
stop_server
start_server || fail "server did not come back"
mount_smb || fail "cifs remount"

# --- corrupt one record in place, with the server running -------------------
python3 "$WORK/records.py" corrupt "$DATA/blocks" "$TARGET_REC" \
  || fail "could not corrupt the target record"

# --- take the remote away, so the heal's re-fetch fails ---------------------
stop_minio
log "remote stopped"

# The read must fail: the local bytes failed their CRC and there is nothing to
# heal from. That failure IS the demotion firing — it is what leaves the range
# marked cold while its record is still live in a segment on disk.
if dd if="$WORK/smb/cold.bin" of=/dev/null bs=$RECORD skip=$TARGET_REC count=1 2>"$WORK/read.err"; then
  log "WARNING: the corrupt read succeeded; the demotion window may not have opened"
else
  log "corrupt read failed as expected: $(tail -1 "$WORK/read.err")"
fi

# --- evict, retiring the segment that holds the only copy -------------------
dctl store block evict --share /cold --local-only 2>&1 | tail -3
log "evicted"

# --- detach the remote tier from the share ----------------------------------
# A lost cold marker degrades the range to a POSIX hole, and readAtInternal
# reconciles a hole against the FileChunk manifest whenever a remote store is
# configured — which hides the loss for as long as one is. Detaching the remote
# is a supported control-plane edit that rebuilds the engine over the SAME
# journal directory with no remote, and it is what separates the two states:
# a range that kept its marker reads cold and FAILS CLOSED, while a range that
# lost it reads as a hole and is zero-filled with no error at any layer.
# Bring the remote back up first. It only had to be down at the heal, and the
# detach drains uploads through it — against a stopped remote that drain times
# out, the rebind fails, and the read failures below would then be as easily
# blamed on a broken rebind as on a lost marker. With it up the detach is clean,
# and the probes fail only because the share has no remote tier at all.
start_minio || fail "minio did not come back before the detach"

TOKEN=$(python3 -c "
import json
d = json.load(open('$WORK/cfg/dfsctl/config.json'))
print(d['contexts'][d['current_context']]['access_token'])
")
# The hot-reload half of this edit runs a drain against the stopped remote and
# times out; the binding is still cleared, and the server says a restart is
# required — which is the next thing the rig does. So the edit is judged by
# whether the binding actually went away, not by the request's exit status.
curl -s -X PUT "http://127.0.0.1:$API/api/v1/shares/cold" \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"remote_block_store_id": ""}' --max-time 180 >/dev/null

umount_smb
stop_server
start_server || fail "server did not come back after detach"
grep -q "mode=remote-backed" <(tail -40 "$WORK/dfs.log") \
  && fail "share still has a remote tier after the detach"
log "remote tier detached from the share"
mount_smb || fail "cifs remount after detach"

# --- verify -----------------------------------------------------------------
# Every range in the file is remote-only now, so a correct store refuses every
# one of them. NEIGHBOUR is the control: it kept its marker either way, so it
# must fail on both builds — that is what makes the target's result specific to
# the demotion rather than to the detach.
NEIGHBOUR=$((TARGET_REC + 1))
probe() {
  local rec=$1 out
  out=$WORK/probe.$rec
  if dd if="$WORK/smb/cold.bin" of="$out" bs=$RECORD skip="$rec" count=1 2>/dev/null; then
    if [ ! -s "$out" ]; then
      echo "empty"
    elif python3 -c "import sys; sys.exit(0 if open('$out','rb').read().strip(b'\\x00') == b'' else 1)"; then
      echo "ZEROS"
    else
      echo "data"
    fi
  else
    echo "refused"
  fi
}

target=$(probe "$TARGET_REC")
neighbour=$(probe "$NEIGHBOUR")
{
  echo "target_record=$TARGET_REC read=$target"
  echo "neighbour_record=$NEIGHBOUR read=$neighbour"
  # The neighbour proves the store still refuses remote-only ranges at all; the
  # target reading as zeros while it does is the marker that was lost.
  if [ "$neighbour" != "refused" ]; then
    echo "VERDICT=INCONCLUSIVE (control record did not fail closed)"
  elif [ "$target" = "refused" ]; then
    echo "VERDICT=PASS"
  else
    echo "VERDICT=FAIL"
  fi
} | tee "$WORK/result.txt"
grep -q "VERDICT=PASS" "$WORK/result.txt"
