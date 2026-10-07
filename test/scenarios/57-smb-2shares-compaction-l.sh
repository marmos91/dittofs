#!/usr/bin/env bash
# GC compaction on a metadata store that serves shares on several remotes: setup's /test (bucket
# dittofs), /export (bucket export) and /cubbit (bucket cubbit) all keep md. Each GC runs a pass,
# then compaction, for every remote, in map order, and a compaction pass that is not the block's
# own finds no object for it. The compactor deletes nothing (RFC 9 §4, draft), so every live byte
# must still read back cold after a reclaim, which deletes the objects that have no record.
# Each round puts a 16 MiB file on /export in 64 KiB-minimum chunks, packed into 4 MiB blocks,
# cuts it to 6000000 bytes, which leaves one block about a third live, and runs GC. Four rounds, as
# /export's pass comes first in about a third of the runs. It takes about 6 min, for the 5 min grace.
# Failed before PR #2919: the other remotes' compaction took the partly live block for the husk of
# an interrupted compaction and dropped its record, and the reclaim deleted the object. Every file
# whose round had such a pass first stopped reading there (NT_STATUS_UNEXPECTED_IO_ERROR; 4 of 4 at
# this ratio, 3 of 4 at 0.99), and the server logged "CAS object missing for live FileChunk". It
# passed with compaction off, and passes since that PR, which skips compaction for such a store.

rclone mkdir s3:export
dfsctl store block add --name s3-export --type s3 --config "$(jq -c '.bucket = "export"' /etc/dittofs-s3.json)"
dfsctl share create --name /export --metadata md --block-store s3-export --default-permission none
dfsctl share permission grant /export --user tester --level read-write
rclone mkdir s3:cubbit
dfsctl store block add --name s3-cubbit --type s3 --config "$(jq -c '.bucket = "cubbit"' /etc/dittofs-s3.json)"
dfsctl share create --name /cubbit --metadata md --block-store s3-cubbit --default-permission none

# Compaction for blocks less than half live, the 5 min grace (its floor), 64 KiB minimum chunks
dfs-server stop
dfs-server start DITTOFS_GC_COMPACTION_LIVE_RATIO=0.5 DITTOFS_GC_GRACE_PERIOD=5m DITTOFS_BLOCKSTORE_JOURNAL_CHUNK_SIZE=65536

mkdir /tmp/f /tmp/want /tmp/back
for k in 1 2 3 4; do head -c 16M /dev/urandom >/tmp/f/f$k; head -c 6000000 /tmp/f/f$k >/tmp/want/f$k; done
for k in 1 2 3 4; do
    smbclient //127.0.0.1/export -c "lcd /tmp/f; put f$k"
    dfsctl system drain-uploads
    smb-truncate "smb://127.0.0.1/export/f$k" 6000000
    dfsctl system drain-uploads
    dfsctl store block gc /export --grace-period 0 -o json >/tmp/gc$k.json
done

# Past the grace for the newest object: the reclaim, then every file read cold
sleep 310
dfsctl store block reclaim -o json | tee /tmp/reclaim.json
dfsctl store block evict --share /export
for k in 1 2 3 4; do smbclient //127.0.0.1/export -c "get f$k /tmp/back/f$k" || true; done

# Outcomes first, then the checks: the cut swept many chunks (the chunk size took effect), every
# file reads back whole
for k in 1 2 3 4; do echo "f$k: $(jq -c '{objects_swept, bytes_freed}' /tmp/gc$k.json), $(cmp /tmp/back/f$k /tmp/want/f$k 2>&1 && echo reads back)"; done
test "$(jq .objects_swept /tmp/gc1.json)" -gt 10
cmp /tmp/back/f1 /tmp/want/f1
cmp /tmp/back/f2 /tmp/want/f2
cmp /tmp/back/f3 /tmp/want/f3
cmp /tmp/back/f4 /tmp/want/f4
