#!/usr/bin/env bash
# With the S3 store down, a read of data that is only in S3 must fail, and never return zeros or
# other wrong bytes (RFC 0 §10, RFC 8 §7.5). The outage must show in the stats within 3 min, a write
# must still be accepted, and once S3 is back everything recovers with no dfsctl step (RFC 0 §10.2).
# Passes today: the SMB read fails after 60 s with NT_STATUS_IO_TIMEOUT, the NFS read at once, and
# remote_healthy turns false about 90 s into the outage (three failed probes).
# It takes about 95 s, mostly waiting.
NFS='nfs://127.0.0.1/test'
OPT='?version=4&uid=1000&gid=1000'

head -c 64M /dev/urandom >/tmp/a.bin
head -c 1M /dev/urandom >/tmp/b.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
dfsctl store block evict --share /test
rclone size --json s3:dittofs/test/ | tee /tmp/before.json

# S3 down: both reads must fail; whatever bytes they return must match a.bin; a write is accepted
s3 stop
timeout 300 smbclient //127.0.0.1/test -c 'get a.bin /tmp/smb.bin' >/tmp/smb.out 2>&1 || echo "exit $?" >/tmp/smb.rc
timeout 300 nfs-cp "$NFS/a.bin$OPT" /tmp/nfs.bin >/tmp/nfs.out 2>&1 || echo "exit $?" >/tmp/nfs.rc
touch /tmp/smb.bin /tmp/nfs.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin'
for _ in $(seq 1 180); do dfsctl store block stats --share /test -o json | jq -e '.totals.remote_healthy == false' >/dev/null && break; sleep 1; done
dfsctl store block stats --share /test -o json | jq -c '.totals | {remote_healthy, outage_duration_seconds}' | tee /tmp/down.json

# S3 back: the read works again by itself, the drain completes, b.bin reaches the bucket
s3 start
for _ in $(seq 1 120); do smbclient //127.0.0.1/test -c 'get a.bin /tmp/back.bin' >/dev/null 2>&1 && break; sleep 1; done
dfsctl system drain-uploads
dfsctl store block stats --share /test -o json | jq -c '.totals | {remote_healthy, outage_duration_seconds}' | tee /tmp/up.json
rclone size --json s3:dittofs/test/ | tee /tmp/after.json

# Outcomes first, then the checks
cat /tmp/smb.out /tmp/nfs.out
cat /tmp/smb.rc /tmp/nfs.rc
stat -c '%n %s' /tmp/smb.bin /tmp/nfs.bin /tmp/back.bin
test -s /tmp/smb.rc
test -s /tmp/nfs.rc
cmp -n "$(stat -c %s /tmp/smb.bin)" /tmp/smb.bin /tmp/a.bin
cmp -n "$(stat -c %s /tmp/nfs.bin)" /tmp/nfs.bin /tmp/a.bin
test "$(jq .remote_healthy /tmp/down.json)" = false
test "$(jq .remote_healthy /tmp/up.json)" = true
cmp /tmp/back.bin /tmp/a.bin
test "$(jq .bytes /tmp/after.json)" -ge "$(($(jq .bytes /tmp/before.json) + 1048576))"
