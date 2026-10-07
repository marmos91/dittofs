#!/usr/bin/env bash
# One share, the /test that setup.sh creates (bucket dittofs, prefix test/), with a small and a large
# file: delete the small one, GC, delete the large one, GC; checked: GC's numbers and the bucket. The
# single-share control for 52-smb-2shares-small-large-gc-xs.sh: with one block store on the metadata store,
# GC deletes what it sweeps, so this passes where the two-share scenario fails (A-08).

printf 'Hello world\n' >/tmp/test.txt
cp /usr/local/bin/dfsctl /tmp/large.bin
(cd /tmp && sha256sum large.bin >/tmp/large.sha256)
smbclient //127.0.0.1/test -c 'lcd /tmp; put test.txt; put large.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test >/tmp/test0.json
test "$(jq .count /tmp/test0.json)" -gt 0
test "$(jq .bytes /tmp/test0.json)" -ge "$(stat -c %s /tmp/large.bin)"

# Small file: GC frees only its data; the large file stays whole
smbclient //127.0.0.1/test -c 'del test.txt'
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc1.json
test "$(jq .objects_swept /tmp/gc1.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc1.json)" -lt $((1024 * 1024))
test "$(jq .hashes_marked /tmp/gc1.json)" -gt 0
test "$(rclone size --json s3:dittofs/test | jq .count)" -lt "$(jq .count /tmp/test0.json)"
dfsctl store block evict --share /test
mkdir /tmp/back
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get large.bin'
(cd /tmp/back && sha256sum -c --quiet /tmp/large.sha256)

# Large file: GC frees the rest and the bucket is empty. Nothing else is on the metadata store, so
# GC marks no hashes at all.
smbclient //127.0.0.1/test -c 'del large.bin'
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc2.json
test "$(jq .objects_swept /tmp/gc2.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc2.json)" -ge "$(stat -c %s /tmp/large.bin)"
test "$(jq .hashes_marked /tmp/gc2.json)" -eq 0
for _ in $(seq 1 60); do test "$(rclone size --json s3:dittofs/test | jq .count)" -eq 0 && break; sleep 2; done
test "$(rclone size --json s3:dittofs/test | jq .count)" -eq 0
test "$(dfsctl store block stats --share /test -o json | jq .totals.blocks_total)" -eq 0
