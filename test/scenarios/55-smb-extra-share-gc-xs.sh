#!/usr/bin/env bash
# setup.sh's /test (bucket dittofs) stays empty; one extra share /extra on its own bucket, on the same
# metadata store md, gets all the data: six small files and a large one. Each file is deleted and GC'd
# in its own round, and each round's GC must shrink /extra's bucket. The single-store control is
# 42-smb-1share-small-large-gc-xs.sh, which passes; 56-smb-extra-share-gc-minimal-xs.sh is the smallest reproduction.
# Guards A-08: `dfsctl store block gc /extra` runs GC on every remote, not only /extra's. With one
# metadata store the synced-hash index is shared, so when /test's remote ran first, its reclaimer
# deleted the dead block from bucket dittofs (where it is not), dropped the block record and the synced
# marker, and counted it as swept; /extra's object was never deleted. Which remote runs first follows
# map iteration order, so a single round can pick the right one and pass by luck; one round per file
# makes a lucky pass of every round unlikely.

rclone mkdir s3:extra
dfsctl store block add --name s3-extra --type s3 --config "$(jq -c '.bucket = "extra"' /etc/dittofs-s3.json)"
dfsctl share create --name /extra --metadata md --block-store s3-extra --default-permission none
dfsctl share permission grant /extra --user tester --level read-write

mkdir /tmp/up
for i in 1 2 3 4 5 6; do printf 'small file %s\n' "$i" >"/tmp/up/s$i.txt"; done
cp /usr/local/bin/dfsctl /tmp/up/large.bin
(cd /tmp/up && sha256sum large.bin >/tmp/large.sha256)
smbclient //127.0.0.1/extra -c 'lcd /tmp/up; prompt; mput s*.txt; put large.bin'
dfsctl system drain-uploads
rclone size --json s3:extra >/tmp/extra0.json
test "$(jq .count /tmp/extra0.json)" -gt 6
test "$(jq .bytes /tmp/extra0.json)" -ge "$(stat -c %s /tmp/up/large.bin)"
test "$(rclone size --json s3:dittofs | jq .count)" -eq 0

# Small files, one per round: GC sweeps something, and /extra's bucket shrinks every time
for i in 1 2 3 4 5 6; do rclone size --json s3:extra >"/tmp/before$i.json"; smbclient //127.0.0.1/extra -c "del s$i.txt"; dfsctl store block gc /extra --grace-period 0 -o json >"/tmp/gc$i.json"; test "$(jq .objects_swept "/tmp/gc$i.json")" -ge 1; test "$(rclone size --json s3:extra | jq .count)" -lt "$(jq .count "/tmp/before$i.json")"; done

# The large file is still whole when read back from the bucket
dfsctl store block evict --share /extra
mkdir /tmp/back
smbclient //127.0.0.1/extra -c 'lcd /tmp/back; get large.bin'
(cd /tmp/back && sha256sum -c --quiet /tmp/large.sha256)

# Large file: GC frees the rest and /extra's bucket is empty
smbclient //127.0.0.1/extra -c 'del large.bin'
dfsctl store block gc /extra --grace-period 0 -o json | tee /tmp/gc7.json
test "$(jq .objects_swept /tmp/gc7.json)" -ge 1
test "$(jq .bytes_freed /tmp/gc7.json)" -ge "$(stat -c %s /tmp/up/large.bin)"
for _ in $(seq 1 60); do test "$(rclone size --json s3:extra | jq .count)" -eq 0 && break; sleep 2; done
test "$(rclone size --json s3:extra | jq .count)" -eq 0
test "$(dfsctl store block stats --share /extra -o json | jq .totals.blocks_total)" -eq 0
