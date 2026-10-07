#!/usr/bin/env bash
# Truncating an uploaded file gives the cut tail back to the bucket once GC runs: the refs past the
# new size are dropped, a block left with no live chunk is retired, and its object deleted (RFC 0 §7,
# RFC 9 §1 and §3.1). The single-share case of what 57-smb-2shares-compaction-l.sh checks across
# shares, and the step 92-smb-fslogix-container-xl.sh leaves to GC after a container shrinks.
# f.bin, 64 MiB random, is put over SMB and drained; cut to 16 MiB with an SMB SET_INFO end-of-file,
# drained again and GC run: the bucket must fall back to within 24 MiB of where it started (the kept
# 16 MiB, plus at most two blocks still holding the cut's boundary), and f.bin must read back cold
# as its first 16 MiB.
rclone size --json s3:dittofs/test | tee /tmp/base.json
head -c 64M /dev/urandom >/tmp/f.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put f.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test | tee /tmp/full.json

# Cut to 16 MiB, drained, GC
smb-truncate smb://127.0.0.1/test/f.bin 16777216
head -c 16M /tmp/f.bin >/tmp/want.bin
dfsctl system drain-uploads
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc.json
for _ in $(seq 1 30); do test "$(rclone size --json s3:dittofs/test | jq .bytes)" -le $(($(jq .bytes /tmp/base.json) + 24 * 1024 * 1024)) && break; sleep 1; done
rclone size --json s3:dittofs/test | tee /tmp/after.json

# The kept part, read cold
dfsctl store block evict --share /test
mkdir /tmp/back
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get f.bin'

# The full file reached the bucket (the control), the cut tail left it, the kept part is intact
test "$(jq .bytes /tmp/full.json)" -ge $(($(jq .bytes /tmp/base.json) + 64 * 1024 * 1024))
test "$(jq .bytes /tmp/after.json)" -le $(($(jq .bytes /tmp/base.json) + 24 * 1024 * 1024))
cmp /tmp/back/f.bin /tmp/want.bin
