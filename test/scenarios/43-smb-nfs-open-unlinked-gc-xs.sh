#!/usr/bin/env bash
# A file deleted while still open is not released until it is closed (RFC 7 §4.2, App A §4.4). f, 32 MiB
# random, is held open over NFSv4 by nfs-hold, deleted over SMB, GC run and the cache evicted: a read
# through the open handle must still return f. After the close and a second GC the bucket returns to
# its baseline.
# Fails today (#2927) at the read: GC sweeps nothing while f is open, but after the evict the open handle reads
# 32 MiB of zeros.

N='?version=4&uid=1000&gid=1000'
head -c 32M /dev/urandom >/tmp/f
rclone size --json s3:dittofs/test | tee /tmp/base.json
smbclient //127.0.0.1/test -c 'lcd /tmp; put f'
dfsctl system drain-uploads
dfsctl store block evict --share /test
mkfifo /tmp/hold
nfs-hold "nfs://127.0.0.1/test/f$N" </tmp/hold >/tmp/held &
H=$!
exec 3>/tmp/hold
smbclient //127.0.0.1/test -c 'del f'
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc1.json
dfsctl store block evict --share /test
echo >&3
exec 3>&-
wait "$H" && echo "read through the open handle: ok" || echo "read through the open handle: FAILED"
cmp /tmp/held /tmp/f && echo "content: matches" || echo "content: differs"
stat -c '%s bytes read' /tmp/held
od -An -tx1 -N16 /tmp/held
cmp -s /tmp/held <(head -c "$(stat -c %s /tmp/held)" /dev/zero) && echo "all zeros" || echo "not all zeros"
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc2.json
for _ in $(seq 1 60); do test "$(rclone size --json s3:dittofs/test | jq .bytes)" -eq "$(jq .bytes /tmp/base.json)" && break; sleep 2; done
rclone size --json s3:dittofs/test | tee /tmp/end.json

cmp /tmp/held /tmp/f
test "$(jq .bytes /tmp/end.json)" -eq "$(jq .bytes /tmp/base.json)"
