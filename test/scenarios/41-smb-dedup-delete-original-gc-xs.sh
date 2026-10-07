#!/usr/bin/env bash
# Deleting a file whose chunks other files share (RFC 0 §8.3 I3, §3.1; G-02, BS-07). a.bin, 32 MiB random;
# b.bin, a copy; c.bin, a's first 16 MiB plus 16 MiB new: the bucket holds about 48 MiB. Deleting a.bin and
# running GC must free nothing b.bin or c.bin still reference, and both read back cold from S3.
# Each file is drained before the next one: dedup only skips chunks already uploaded. The same again
# with a2, b2 and c2, the server restarted between the delete and the GC: the adoption guard that
# keeps a just-deduped chunk from GC lives in memory.

head -c 32M /dev/urandom >/tmp/a.bin
cp /tmp/a.bin /tmp/b.bin
(head -c 16M /tmp/a.bin && head -c 16M /dev/urandom) >/tmp/c.bin
(cd /tmp && sha256sum b.bin c.bin >/tmp/bc.sha256)
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/test -c 'lcd /tmp; put c.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test | tee /tmp/before.json
smbclient //127.0.0.1/test -c 'del a.bin'
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc.json
rclone size --json s3:dittofs/test | tee /tmp/after.json
dfsctl store block evict --share /test
mkdir /tmp/back
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get b.bin; get c.bin'

head -c 32M /dev/urandom >/tmp/a2.bin
cp /tmp/a2.bin /tmp/b2.bin
(head -c 16M /tmp/a2.bin && head -c 16M /dev/urandom) >/tmp/c2.bin
(cd /tmp && sha256sum b2.bin c2.bin >/tmp/bc2.sha256)
smbclient //127.0.0.1/test -c 'lcd /tmp; put a2.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/test -c 'lcd /tmp; put b2.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/test -c 'lcd /tmp; put c2.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test | tee /tmp/before2.json
smbclient //127.0.0.1/test -c 'del a2.bin'
dfs-server stop
dfs-server start
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc2.json
rclone size --json s3:dittofs/test | tee /tmp/after2.json
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get b2.bin; get c2.bin'

test "$(jq .bytes /tmp/before.json)" -ge $((48 * 1024 * 1024))
test "$(jq .bytes /tmp/before.json)" -lt $((64 * 1024 * 1024))
test "$(jq .bytes /tmp/after.json)" -eq "$(jq .bytes /tmp/before.json)"
(cd /tmp/back && sha256sum -c /tmp/bc.sha256)
test "$(($(jq .bytes /tmp/before2.json) - $(jq .bytes /tmp/after.json)))" -ge $((48 * 1024 * 1024))
test "$(($(jq .bytes /tmp/before2.json) - $(jq .bytes /tmp/after.json)))" -lt $((64 * 1024 * 1024))
test "$(jq .bytes /tmp/after2.json)" -eq "$(jq .bytes /tmp/before2.json)"
(cd /tmp/back && sha256sum -c /tmp/bc2.sha256)
