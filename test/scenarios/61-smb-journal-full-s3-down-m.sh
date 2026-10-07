#!/usr/bin/env bash
# A share with a 1 GiB journal and its S3 down: writes fill the journal until one is refused. The
# refusal must be "no space" (NT_STATUS_DISK_FULL), not an I/O error and not a hang (RFC 0 §10.3).
# Deleting and truncating must still work while it is full, because records without bytes are
# never refused (RFC 1 §7). Once S3 is back the backlog must drain by itself, without a dfsctl
# step (RFC 0 §10.2), and every accepted file must read back cold.
# Fails today: ten files fit and the 11th is refused after 60 s with NT_STATUS_IO_TIMEOUT, not
# NT_STATUS_DISK_FULL. The server logs "journal: local store full" as an IOError. 24 MiB of the
# refused file stay written. The rest holds: while full, the delete (15 s) and the truncate
# succeed, and once S3 is back the backlog drains by itself within seconds and the files read back
# cold. It takes about 2 min.
rclone mkdir s3:small
dfsctl store block add --name s3-small --type s3 --config "$(jq -c '.bucket = "small"' /etc/dittofs-s3.json)"
dfsctl share create --name /small --metadata md --block-store s3-small --default-permission none --journal-size 1GiB
dfsctl share permission grant /small --user tester --level read-write
s3 stop

# 100 MiB files, each new random data, until one is refused (at most 15, each within 5 min). smbclient
# waits 45 s for a reply (-t 45): the server may hold a write for its 30 s deadline, past smbclient's 20 s
for i in $(seq 1 15); do head -c 100M /dev/urandom >"/tmp/f$i.bin"; timeout 300 smbclient -t 45 //127.0.0.1/small -c "lcd /tmp; put f$i.bin" >"/tmp/put$i.out" 2>&1 || { echo "$i exit $?" >/tmp/refused; break; }; done
dfsctl store block stats --share /small -o json | jq -c '.totals | {local_disk_used, local_disk_max, unsynced_bytes}' | tee /tmp/full.json

# Full: a delete and a truncate must succeed
timeout 300 smbclient //127.0.0.1/small -c 'del f1.bin' >/tmp/del.out 2>&1 || echo "exit $?" >/tmp/del.rc
timeout 300 smb-truncate smb://127.0.0.1/small/f2.bin 1048576 >/tmp/trunc.out 2>&1 || echo "exit $?" >/tmp/trunc.rc

# S3 back: the backlog must drain without dfsctl (polled for up to 5 min), then reads come from S3
s3 start
for _ in $(seq 1 300); do dfsctl store block stats --share /small -o json | jq -e '.totals.unsynced_bytes == 0 and .totals.pending_uploads == 0' >/dev/null && break; sleep 1; done
dfsctl store block stats --share /small -o json | jq -c '.totals | {unsynced_bytes, pending_uploads}' | tee /tmp/drained.json
dfsctl store block evict --share /small
mkdir /tmp/back
smbclient //127.0.0.1/small -c 'lcd /tmp/back; prompt; mget f*.bin'
head -c 1M /tmp/f2.bin >/tmp/f2-truncated.bin

# Outcomes first, then the checks
cat /tmp/refused /tmp/put*.out | grep -v '^putting file'
cat /tmp/del.out /tmp/trunc.out /tmp/*.rc 2>/dev/null || true
ls -l /tmp/back
test -s /tmp/refused
grep -q NT_STATUS_DISK_FULL "/tmp/put$(cut -d' ' -f1 /tmp/refused).out"
test ! -e /tmp/del.rc
test ! -e /tmp/trunc.rc
test "$(jq .unsynced_bytes /tmp/drained.json)" -eq 0
test ! -e /tmp/back/f1.bin
cmp /tmp/back/f2.bin /tmp/f2-truncated.bin
for i in $(seq 3 $(($(cut -d' ' -f1 /tmp/refused) - 1))); do cmp "/tmp/back/f$i.bin" "/tmp/f$i.bin"; done
