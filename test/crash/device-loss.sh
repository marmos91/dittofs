#!/usr/bin/env bash
# Device-loss crash rig: does an acknowledged write ever come back as the right
# size with zero content?
#
# Runs a DittoFS server whose metadata store and journal both live on
# an ext4 filesystem over dm-flakey. Mid-write the table is swapped to
# drop_writes, so only bytes that genuinely reached the device survive - the
# power-cut model. `kill -9` does not reproduce this: the page cache outlives
# process death.
#
# A writer appends self-identifying 4096-byte records over SMB (cache=none, so
# every write() is answered by the server) and logs each acknowledged record to
# a file on a separate disk, fsynced. After the switch the server is killed, the
# device is restored to what survived, the server restarts and the file is read
# back.
#
# Verdict: acknowledged records must read back correct or be missing (short
# file). A record inside the file's own size that reads as zeros is the bug.
# Records the writer fsynced must read back byte-exact; records written since
# the last fsync may be missing, but must never read as zeros.
#
# Requires: root, Linux, dm-flakey, cifs-utils, python3.
#
#   sudo ./device-loss.sh /path/to/dfs /path/to/dfsctl [write-seconds] [commit-every]

set -u

DFS="${1:?path to dfs binary required}"
DFSCTL="${2:?path to dfsctl binary required}"
SECONDS_WRITING="${3:-15}"
COMMIT_EVERY="${4:-64}"   # fsync every N records; 0 never commits

export PATH="/usr/sbin:/sbin:/usr/bin:/bin:$PATH"

WORK=/var/tmp/dfs-crash
DEV=dfscrash
IMG=$WORK/backing.img
DATA=$WORK/data          # mount point of the flaky filesystem
ACKS=$WORK/acks.log      # on the root disk, outside the flaky device
COMMITTED=$WORK/committed.log  # highest record the client fsynced
API=18099; SMBP=12455
# Credentials for a server that exists for the length of one run: generated so
# nothing quotable is checked in, alphanumeric so they survive the cifs mount
# option string.
rand() { head -c "$1" /dev/urandom | base64 | tr -dc 'A-Za-z0-9'; }
PW=$(rand 24); SECRET=$(rand 48)
RECORD=4096

log() { echo "[crash] $*"; }

# kill_server kills the recorded server, but only once the PID is confirmed to
# be this rig's dfs binary: a pidfile left behind by an interrupted run names a
# PID the kernel may since have handed to something else, and this runs as root.
# It then waits for the process to go, because the fds it holds on the flaky
# filesystem are what would keep the restore remount busy.
kill_server() {
  local pid
  pid=$(cat "$WORK/dfs.pid" 2>/dev/null) || return 0
  [ -n "$pid" ] || return 0
  [ "$(readlink -f "/proc/$pid/exe" 2>/dev/null)" = "$(readlink -f "$DFS")" ] || return 0
  kill -9 "$pid" 2>/dev/null
  for _ in {1..100}; do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 0.1
  done
  return 1
}

# umount_smb detaches lazily: the server it talks to has just been killed, so a
# plain unmount would sit waiting for a reply that is never coming. Nothing here
# holds the block device, so a deferred teardown cannot reach it.
umount_smb() { umount -l "$WORK/smb" 2>/dev/null; }

# umount_data does a real unmount, retried while the dying server releases its
# fds. A lazy one would detach the mount point and return while writeback is
# still pending, and that pending work could then land on the healed device
# after the table is restored - the crash model must not leak across that line.
# Retrying also keeps a slow teardown from being read as a filesystem that did
# not survive.
umount_data() {
  for _ in {1..100}; do
    mountpoint -q "$DATA" || return 0
    umount "$DATA" 2>/dev/null && return 0
    sleep 0.1
  done
  return 1
}

cleanup() {
  pkill -9 -f "$WORK/writer.py" 2>/dev/null
  kill_server
  umount_smb
  umount_data
  dmsetup remove -f "$DEV" 2>/dev/null
  losetup -j "$IMG" 2>/dev/null | cut -d: -f1 | xargs -r losetup -d 2>/dev/null
}

# Cleanup on every exit path, including an interrupt mid-run: a leaked mount,
# dm device or server would otherwise be inherited by the next run.
trap cleanup EXIT INT TERM

fail() { log "FAIL: $*"; exit 1; }

table() { echo "0 $SECTORS $*"; }

# --noflush --nolockfs: no dirty page is allowed to reach the device while the
# table is being swapped, so the crash point is exactly the suspend.
retable() {
  dmsetup suspend --noflush --nolockfs "$DEV" &&
    dmsetup reload "$DEV" --table "$(table "$@")" &&
    dmsetup resume "$DEV"
}

# The SMB listener can bind a moment after `adapter enable` returns, and again
# after a restart, so the mount is retried rather than raced. cache=none is what
# makes each write() wait for the server's answer.
mount_smb() {
  for _ in {1..40}; do
    mount -t cifs //127.0.0.1/crash "$WORK/smb" \
      -o "port=$SMBP,username=admin,password=$PW,vers=3.1.1,cache=none" 2>/dev/null && return 0
    sleep 0.5
  done
  return 1
}

start_server() {
  DITTOFS_ADMIN_INITIAL_PASSWORD=$PW setsid "$DFS" start --foreground \
    --config "$WORK/config.yaml" --pid-file "$WORK/dfs.pid" \
    --log-file "$WORK/dfs.log" >"$WORK/dfs.out" 2>&1 &
  for _ in {1..100}; do
    curl -sf "http://127.0.0.1:$API/health" >/dev/null 2>&1 && return 0
    sleep 0.3
  done
  return 1
}

dctl() { XDG_CONFIG_HOME="$WORK/cfg" "$DFSCTL" "$@"; }

# --- setup -----------------------------------------------------------------
cleanup
rm -rf "$WORK"; mkdir -p "$WORK/cfg" "$DATA" "$WORK/smb"

truncate -s 4G "$IMG"
LOOP=$(losetup -f --show "$IMG") || fail "losetup"
SECTORS=$(blockdev --getsz "$LOOP")
dmsetup create "$DEV" --table "$(table linear "$LOOP" 0)" || fail "dmsetup create"
mkfs.ext4 -q -F "/dev/mapper/$DEV" || fail "mkfs"
mount "/dev/mapper/$DEV" "$DATA" || fail "mount"
mkdir -p "$DATA/meta" "$DATA/blocks"

# DIRTY_EXPIRE_SECONDS overrides the journal's dirty-age fsync ceiling, so a run
# can bound the non-durable window well inside its own write window. Unset
# leaves the shipped default in place.
JOURNAL_DIRTY_EXPIRE=""
if [ -n "${DIRTY_EXPIRE_SECONDS:-}" ]; then
    JOURNAL_DIRTY_EXPIRE="
    dirty_expire: ${DIRTY_EXPIRE_SECONDS}s"
fi

cat > "$WORK/config.yaml" <<CFG
logging: {level: INFO, format: text, output: $WORK/dfs.log}
controlplane:
  host: 127.0.0.1
  port: $API
  jwt: {secret: "$SECRET"}
database: {type: sqlite, sqlite: {path: "$WORK/controlplane.db"}}
blockstore:
  journal:
    path: $DATA/blocks$JOURNAL_DIRTY_EXPIRE
CFG

start_server || fail "server never became ready"
dctl login --server "http://127.0.0.1:$API" --username admin --password "$PW" >/dev/null || fail "login"
dctl store metadata add --name meta --type badger --db-path "$DATA/meta" >/dev/null || fail "metadata store"
# The written data lives in the journal on the flaky device; the block store is
# mandatory but holds nothing the device loss can take away, so it stays in
# memory rather than adding a second failure domain to the rig.
dctl store block add --name blk --type memory >/dev/null || fail "block store"
dctl share create --name /crash --metadata meta --block-store blk --default-permission read-write >/dev/null || fail "share create"
dctl adapter enable smb --port $SMBP >/dev/null || fail "smb adapter"

mount_smb || fail "cifs mount"
log "server up, share mounted"

# --- writer ----------------------------------------------------------------
# Every write() is answered by the server (cache=none), and only then is the
# record logged as acknowledged. The ack log is fsynced to the root disk, so it
# survives the device loss the data does not.
cat > "$WORK/writer.py" <<'PY'
import hashlib, os, sys
path, acks, committed = sys.argv[1:4]
record, every = int(sys.argv[4]), int(sys.argv[5])
f = open(path, "wb", buffering=0)
a = open(acks, "w")
c = open(committed, "w")
i = 0
while True:
    head = ("REC%08d" % i).encode()
    body = head + hashlib.sha256(head).digest()
    f.write(body + b"\xa5" * (record - len(body)))
    a.write("%d\n" % i); a.flush(); os.fsync(a.fileno())
    if every and (i + 1) % every == 0:
        os.fsync(f.fileno())
        c.seek(0); c.write("%d\n" % i); c.truncate(); c.flush(); os.fsync(c.fileno())
    i += 1
PY

python3 "$WORK/writer.py" "$WORK/smb/crash.bin" "$ACKS" "$COMMITTED" "$RECORD" "$COMMIT_EVERY" &
WRITER=$!
sleep "$SECONDS_WRITING"

# --- device loss -----------------------------------------------------------
# Stop the writer first, so every record in the ack log was answered while the
# device was still healthy. drop_writes then silently discards everything
# written from here on.
kill -STOP $WRITER

# --- pressure guard ---------------------------------------------------------
# This rig reads a missing or zeroed record as data loss, which is only honest
# while the journal never had a legitimate reason to drop one itself. It does
# have one: eviction, gated on the journal's disk footprint against its disk
# budget. An evicted record's only remaining copy is the block store's, and
# this rig's block store is in-memory and dies with the killed server — so a
# run that wrote enough to provoke eviction loses records for a reason that has
# nothing to do with the device, and looks exactly like the bug being hunted.
#
# The write window and the record rate are the caller's, the budget is the
# server's, and their product is what decides whether the run stayed clear. So
# the guard measures instead of guessing, and refuses the run rather than
# grading one it cannot read. It runs with the writer stopped, on the footprint
# the device loss is about to freeze. A budget the server does not report is
# also a refusal: a guard that cannot see the ceiling cannot certify the run
# stayed under it.
dctl store block stats --share /crash -o json > "$WORK/stats.json" || fail "block store stats"
python3 - "$WORK/stats.json" <<'PY' || fail "cannot certify the journal stayed clear of eviction: shorten the write window (argument 3) or give the share a larger journal"
import json, sys

t = json.load(open(sys.argv[1]))["totals"]
used, budget = t["local_disk_used"], t["local_disk_max"]
if budget <= 0:
    sys.exit("[crash] pressure: journal reports no disk budget, so headroom cannot be checked")
# Half the budget: the writer is stopped, but a rollup still in flight can move
# the footprint after this reading, and eviction must stay out of reach of that
# drift too.
if used * 2 > budget:
    sys.exit("[crash] pressure: journal holds %d bytes of a %d-byte disk budget"
             % (used, budget))
print("[crash] pressure guard clear: journal holds %d bytes of %d (%.1f%%)"
      % (used, budget, 100.0 * used / budget))
PY

retable flakey "$LOOP" 0 0 60 1 drop_writes || fail "device loss"
log "device lost after $(wc -l < "$ACKS") acknowledged records"

kill -9 $WRITER 2>/dev/null; wait $WRITER 2>/dev/null
kill_server || fail "server would not die"
umount_smb
umount_data || fail "flaky filesystem would not unmount"

# --- restore what survived -------------------------------------------------
retable linear "$LOOP" 0 || fail "restore device"
mount "/dev/mapper/$DEV" "$DATA" || fail "remount (filesystem did not survive)"

start_server || fail "server never came back"
mount_smb || fail "cifs remount"

# --- verify ----------------------------------------------------------------
cat > "$WORK/verify.py" <<'PY'
import hashlib, sys
path, acks, committed = sys.argv[1:4]
record = int(sys.argv[4])
acked = sum(1 for _ in open(acks))
last_committed = int(open(committed).read().strip() or -1)
data = open(path, "rb").read()
print("acked=%d committed=%d acked_bytes=%d actual_bytes=%d"
      % (acked, last_committed + 1, acked * record, len(data)))
bad, lost, broken_promise, first_bad, last_bad = 0, 0, 0, None, None
for i in range(acked):
    chunk = data[i * record:(i + 1) * record]
    head = ("REC%08d" % i).encode()
    body = head + hashlib.sha256(head).digest()
    want = body + b"\xa5" * (record - len(body))
    if chunk == want:
        continue
    if i <= last_committed:
        broken_promise += 1
    if len(chunk) < record:
        # Short file: the write is simply gone. Fine for an uncommitted record,
        # a broken durability promise for a committed one.
        lost += 1
        continue
    kind = "ZEROS" if chunk == b"\x00" * record else "GARBAGE"
    bad += 1
    if first_bad is None:
        first_bad = (i, kind)
    last_bad = (i, kind)
print("lost_to_short_file=%d bad=%d committed_records_broken=%d first_bad=%s last_bad=%s"
      % (lost, bad, broken_promise, first_bad, last_bad))
print("VERDICT=%s" % ("FAIL" if bad or broken_promise else "PASS"))
PY

python3 "$WORK/verify.py" "$WORK/smb/crash.bin" "$ACKS" "$COMMITTED" "$RECORD" | tee "$WORK/result.txt"
grep -q "VERDICT=PASS" "$WORK/result.txt"
