#!/usr/bin/env bash
# Two shares on two buckets (/export on s3-export, /cubbit on s3-cubbit), each with its
# own small and large file (the same content on both is A-07). Share by share: delete the small one, GC,
# delete the large one, GC; checked: GC's numbers, the share's bucket, the other bucket never changing.
# Guards A-08 (GC reports objects swept but deletes nothing), which failed it at the first GC's bucket
# check.

rclone mkdir s3:export
dfsctl store block add --name s3-export --type s3 --config "$(jq -c '.bucket = "export"' /etc/dittofs-s3.json)"
dfsctl share create --name /export --metadata md --block-store s3-export --default-permission none
dfsctl share permission grant /export --user tester --level read-write
rclone mkdir s3:cubbit
dfsctl store block add --name s3-cubbit --type s3 --config "$(jq -c '.bucket = "cubbit"' /etc/dittofs-s3.json)"
dfsctl share create --name /cubbit --metadata md --block-store s3-cubbit --default-permission none
dfsctl share permission grant /cubbit --user tester --level read-write

mkdir /tmp/e /tmp/c
printf 'Hello world\n' >/tmp/e/test.txt
cp /usr/local/bin/dfsctl /tmp/e/large.bin
printf 'Hello cubbit\n' >/tmp/c/test.txt
cp /usr/local/bin/dfs /tmp/c/large.bin
(cd /tmp/e && sha256sum large.bin >/tmp/e.sha256)
(cd /tmp/c && sha256sum large.bin >/tmp/c.sha256)
smbclient //127.0.0.1/export -c 'lcd /tmp/e; put test.txt; put large.bin'
smbclient //127.0.0.1/cubbit -c 'lcd /tmp/c; put test.txt; put large.bin'
dfsctl system drain-uploads
rclone size --json s3:export >/tmp/export0.json
rclone size --json s3:cubbit >/tmp/cubbit0.json
rclone lsf -R --files-only --format ps s3:cubbit >/tmp/cubbit0.lst
test "$(jq .count /tmp/export0.json)" -gt 0
test "$(jq .count /tmp/cubbit0.json)" -gt 0
test "$(jq .bytes /tmp/export0.json)" -ge "$(stat -c %s /tmp/e/large.bin)"

# /export, small file: GC frees only its data; the large file stays whole; /cubbit's bucket is untouched
smbclient //127.0.0.1/export -c 'del test.txt'
dfsctl store block gc /export --grace-period 0 -o json | tee /tmp/gc1.json
test "$(jq .objects_swept /tmp/gc1.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc1.json)" -lt $((1024 * 1024))
test "$(jq .hashes_marked /tmp/gc1.json)" -gt 0
test "$(rclone size --json s3:export | jq .count)" -lt "$(jq .count /tmp/export0.json)"
rclone lsf -R --files-only --format ps s3:cubbit | cmp - /tmp/cubbit0.lst
dfsctl store block evict --share /export
mkdir /tmp/export
smbclient //127.0.0.1/export -c 'lcd /tmp/export; get large.bin'
(cd /tmp/export && sha256sum -c --quiet /tmp/e.sha256)

# /export, large file: GC frees the rest, the bucket is empty; /cubbit's bucket is untouched. GC marks
# hashes across the metadata store both shares use, so it still marks /cubbit's: fewer than before.
smbclient //127.0.0.1/export -c 'del large.bin'
dfsctl store block gc /export --grace-period 0 -o json | tee /tmp/gc2.json
test "$(jq .objects_swept /tmp/gc2.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc2.json)" -ge "$(stat -c %s /tmp/e/large.bin)"
test "$(jq .hashes_marked /tmp/gc2.json)" -lt "$(jq .hashes_marked /tmp/gc1.json)"
for _ in $(seq 1 60); do test "$(rclone size --json s3:export | jq .count)" -eq 0 && break; sleep 2; done
test "$(rclone size --json s3:export | jq .count)" -eq 0
rclone lsf -R --files-only --format ps s3:cubbit | cmp - /tmp/cubbit0.lst

# /cubbit, small file, then large file, the same way
smbclient //127.0.0.1/cubbit -c 'del test.txt'
dfsctl store block gc /cubbit --grace-period 0 -o json | tee /tmp/gc3.json
test "$(jq .objects_swept /tmp/gc3.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc3.json)" -lt $((1024 * 1024))
test "$(jq .hashes_marked /tmp/gc3.json)" -gt 0
test "$(rclone size --json s3:cubbit | jq .count)" -lt "$(jq .count /tmp/cubbit0.json)"
dfsctl store block evict --share /cubbit
mkdir /tmp/cubbit
smbclient //127.0.0.1/cubbit -c 'lcd /tmp/cubbit; get large.bin'
(cd /tmp/cubbit && sha256sum -c --quiet /tmp/c.sha256)
smbclient //127.0.0.1/cubbit -c 'del large.bin'
dfsctl store block gc /cubbit --grace-period 0 -o json | tee /tmp/gc4.json
test "$(jq .objects_swept /tmp/gc4.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc4.json)" -ge "$(stat -c %s /tmp/c/large.bin)"
test "$(jq .hashes_marked /tmp/gc4.json)" -eq 0
for _ in $(seq 1 60); do test "$(rclone size --json s3:cubbit | jq .count)" -eq 0 && break; sleep 2; done
test "$(rclone size --json s3:cubbit | jq .count)" -eq 0
test "$(dfsctl store block stats --share /export -o json | jq .totals.blocks_total)" -eq 0
test "$(dfsctl store block stats --share /cubbit -o json | jq .totals.blocks_total)" -eq 0
