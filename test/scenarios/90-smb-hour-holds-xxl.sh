#!/usr/bin/env bash
# What only shows after an hour, with the hour put to use (BS-08, TR-05, BS-13). /bin and /j have their
# own metadata stores. GC on one share also runs a pass on every other remote, but with separate
# metadata stores each pass reads only its own markers, so none can delete through the wrong remote (A-08).
#   1. Repeated content (G-02): 64 MiB of zeros, one chunk repeated. Right after the delete, GC with
#      grace 0 still leaves the blocks dedup adopted, since an adoption keeps a block from GC for an
#      hour (dedupAdoptionMaxAge). An hour after the write, GC must have freed them all.
#   2. The recycle bin's policies (TR-05): /bin excludes *.tmp, holds at most 3 MiB and keeps entries
#      for a day. An excluded name is deleted for good at once. a, b, c and d (1 MiB each) are deleted
#      in that order; the reaper's first pass, an hour after the server starts, must evict a alone,
#      the oldest, and keep b, c and d, which are younger than a day.
#   3. Journal GC against eviction (BS-13, #2908): /j has a 1 GiB journal. For up to 20 rounds in 50
#      min, 16 writers put a 64 MiB file each at once, so writes evict, then 12 of the files are
#      deleted, so the 30 s journal GC repacks while the next round evicts. /j's Local Disk Used,
#      sampled every 5 s, must never go negative, after each round it must equal the bytes of its
#      journal's .seg files, and with S3 up no write may fail (RFC 0 §10: a write waits for space).
#      Each round writes new random content, so no write dedups against an earlier one.
# Fails today at 1 and 3. 1: the zeros go up as 3 or 4 objects and GC only ever scans one of them;
# it frees that one on its first pass after the hour, and the others are never scanned, so they stay
# (32 MiB in the hour-long run). 3: from the second to fifth round on, every write times out
# (NT_STATUS_IO_TIMEOUT) with S3 up: the journal sits at its cap with nothing unsynced and nothing
# evictable. 2 passes, and so does 3's accounting.
# It takes about 62 min: run with SCENARIO_TIMEOUT=5400.

J=/root/.local/state/dittofs/blocks/shares/j/journal
date +%s >/tmp/t0

# 1. Repeated content, written and deleted first, so its hour runs alongside the rest
rclone size --json s3:dittofs/test | tee /tmp/zeros-base.json
head -c 64M /dev/zero >/tmp/zeros.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put zeros.bin'
dfsctl system drain-uploads
date +%s >/tmp/zeros-written
rclone size --json s3:dittofs/test | tee /tmp/zeros-full.json
smbclient //127.0.0.1/test -c 'del zeros.bin'
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/zeros-gc1.json
rclone size --json s3:dittofs/test | tee /tmp/zeros-held.json

# 2. The recycle bin
rclone mkdir s3:bin
dfsctl store metadata add --name md-bin --type badger --db-path /tmp/md-bin
dfsctl store block add --name s3-bin --type s3 --config "$(jq -c '.bucket = "bin"' /etc/dittofs-s3.json)"
dfsctl share create --name /bin --metadata md-bin --block-store s3-bin --default-permission none --enable-trash --trash-exclude '*.tmp' --trash-max-size 3145728 --trash-retention-days 1
dfsctl share permission grant /bin --user tester --level read-write
mkdir /tmp/bin
for F in x.tmp a.bin b.bin c.bin d.bin; do head -c 1M /dev/urandom >"/tmp/bin/$F"; done
smbclient //127.0.0.1/bin -c 'lcd /tmp/bin; prompt; mput *'
smbclient //127.0.0.1/bin -c 'del x.tmp'
for F in a b c d; do smbclient //127.0.0.1/bin -c "del $F.bin"; sleep 1; done
dfsctl trash list /bin -o json | jq -r '.[].path' | sort | paste -sd ' ' | tee /tmp/bin-before.txt
smbclient //127.0.0.1/bin -c 'ls' | tee /tmp/bin-root.txt

# 3. Journal rounds, with Local Disk Used sampled every 5 s (in its own bash, so it stays out of the trace)
rclone mkdir s3:journal
dfsctl store metadata add --name md-j --type badger --db-path /tmp/md-j
dfsctl store block add --name s3-j --type s3 --config "$(jq -c '.bucket = "journal"' /etc/dittofs-s3.json)"
dfsctl share create --name /j --metadata md-j --block-store s3-j --default-permission none --journal-size 1GiB
dfsctl share permission grant /j --user tester --level read-write
mkdir /tmp/j
bash -c 'while sleep 5; do dfsctl store block stats --share /j -o json | jq .totals.local_disk_used; done' >/tmp/j-used.txt &
echo $! >/tmp/sampler.pid
R=0
while test "$R" -lt 20 && test "$(date +%s)" -lt $(($(cat /tmp/t0) + 3000)); do
  R=$((R + 1))
  for I in $(seq 1 16); do head -c 64M /dev/urandom >"/tmp/j/f$I.bin"; done
  smbclient //127.0.0.1/j -c 'del *.bin' >/dev/null 2>&1 || true
  P=()
  for I in $(seq 1 16); do (smbclient //127.0.0.1/j -c "lcd /tmp/j; put f$I.bin" >"/tmp/j-put$I.txt" 2>&1 || echo "round $R f$I.bin: $(grep -o 'NT_STATUS_[A-Z_]*' "/tmp/j-put$I.txt" | sort -u | paste -sd ' ')" >>/tmp/j-failed.txt) & P+=($!); done
  wait "${P[@]}"
  smbclient //127.0.0.1/j -c "$(seq -f 'del f%g.bin' 1 16 | awk 'NR % 4' | paste -sd ';')" >/dev/null 2>&1 || true
  dfsctl store block gc /j --grace-period 0 >/dev/null
  sleep 35
  for _ in $(seq 1 12); do U=$(dfsctl store block stats --share /j -o json | jq .totals.local_disk_used); S=$(find "$J" -name '*.seg' -printf '%s\n' | awk '{s += $1} END {print s + 0}'); test "$U" -eq "$S" && break; sleep 5; done
  echo "round $R: local_disk_used $U, segments $S" | tee -a /tmp/j-rounds.txt
done
kill "$(cat /tmp/sampler.pid)"

# The hour: the zeros' adoption expires an hour after their write, the reaper's first pass comes an
# hour after the server started
while test "$(date +%s)" -lt $(($(cat /tmp/zeros-written) + 3630)); do sleep 15; done
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/zeros-gc2.json
rclone size --json s3:dittofs/test | tee /tmp/zeros-end.json
for _ in $(seq 1 60); do test "$(dfsctl trash status /bin -o json | jq .total_bytes)" -le 3145728 && break; sleep 10; done
dfsctl trash status /bin -o json
dfsctl trash list /bin -o json | jq -r '.[].path' | sort | paste -sd ' ' | tee /tmp/bin-after.txt

# Outcomes, then the checks
echo "zeros: bucket $(jq .bytes /tmp/zeros-base.json) before, $(jq .bytes /tmp/zeros-full.json) written, $(jq .bytes /tmp/zeros-held.json) after the first GC, $(jq .bytes /tmp/zeros-end.json) after the hour"
echo "bin: $(cat /tmp/bin-before.txt) before, $(cat /tmp/bin-after.txt) after the reaper"
echo "j: $R rounds, $(grep -vc 'local_disk_used \([0-9]*\), segments \1$' /tmp/j-rounds.txt) unequal; Local Disk Used from $(sort -n /tmp/j-used.txt | head -1) to $(sort -n /tmp/j-used.txt | tail -1); $(cat /tmp/j-failed.txt 2>/dev/null | wc -l) failed writes"
cat /tmp/j-failed.txt 2>/dev/null || true
test "$(cat /tmp/bin-before.txt)" = 'a.bin b.bin c.bin d.bin'
! grep -q 'x\.tmp' /tmp/bin-root.txt
test "$(cat /tmp/bin-after.txt)" = 'b.bin c.bin d.bin'
test "$R" -ge 5
test "$(grep -vc 'local_disk_used \([0-9]*\), segments \1$' /tmp/j-rounds.txt)" -eq 0
test "$(sort -n /tmp/j-used.txt | head -1)" -ge 0
test "$(jq .bytes /tmp/zeros-full.json)" -gt "$(jq .bytes /tmp/zeros-base.json)"
test "$(jq .bytes /tmp/zeros-end.json)" -eq "$(jq .bytes /tmp/zeros-base.json)"
test ! -s /tmp/j-failed.txt
