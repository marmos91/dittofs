#!/usr/bin/env bash
# A snapshot pins its blocks against GC (RFC 12 §8.3 S1, §2.8; SN-03). a.bin and b.bin, then snapshot s1,
# then both deleted and GC run: the bucket must keep at least their bytes, and restoring s1 must bring both
# back intact, read cold from S3. Last, removing /test must be refused while it still has a snapshot.
# Fails today at the last check: the share is removed although its snapshot exists. GC sweeps nothing
# and the restore matches.

mkdir /tmp/src /tmp/back
head -c 8M /dev/urandom >/tmp/src/a.bin
head -c 8M /dev/urandom >/tmp/src/b.bin
(cd /tmp/src && sha256sum a.bin b.bin >/tmp/ab.sha256)
smbclient //127.0.0.1/test -c 'lcd /tmp/src; put a.bin; put b.bin'
dfsctl system drain-uploads
dfsctl share snapshot create /test --name s1 -o json | tee /tmp/snap.json
smbclient //127.0.0.1/test -c 'del a.bin; del b.bin'
dfsctl store block gc /test --grace-period 0 -o json
rclone size --json s3:dittofs/test | tee /tmp/after-gc.json
dfsctl share disable /test
dfsctl share snapshot restore /test "$(jq -r .id /tmp/snap.json)" --yes
dfsctl share enable /test
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get a.bin; get b.bin'
(cd /tmp/back && sha256sum -c /tmp/ab.sha256) && echo "restore: ok" || echo "restore: FAILED"
dfsctl share remove /test --force && R=removed || R=refused
echo "share remove with a snapshot: $R"

test "$(jq .bytes /tmp/after-gc.json)" -ge $((16 * 1024 * 1024))
(cd /tmp/back && sha256sum -c /tmp/ab.sha256)
test "$R" = refused
