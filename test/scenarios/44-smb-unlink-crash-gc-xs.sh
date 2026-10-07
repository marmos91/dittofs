#!/usr/bin/env bash
# A crash right after deletes leaks nothing (RFC 7 §4.3, App A; C-11, BS-05). Three random files are
# written, drained and deleted over SMB, the server is killed at once and restarted, then GC runs:
# the bucket must return to its baseline, and audit-refcounts must find no dangling refs. audit-refcounts
# does not look for leaks (C-11), so the bucket is the leak check.

rclone size --json s3:dittofs/test | tee /tmp/base.json
mkdir /tmp/src
head -c 8M /dev/urandom >/tmp/src/1.bin
head -c 8M /dev/urandom >/tmp/src/2.bin
head -c 8M /dev/urandom >/tmp/src/3.bin
smbclient //127.0.0.1/test -c 'lcd /tmp/src; put 1.bin; put 2.bin; put 3.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test | tee /tmp/full.json
smbclient //127.0.0.1/test -c 'del 1.bin; del 2.bin; del 3.bin'
dfs-server kill
dfs-server start
dfsctl store block gc /test --grace-period 0 -o json | tee /tmp/gc.json
for _ in $(seq 1 60); do test "$(rclone size --json s3:dittofs/test | jq .bytes)" -eq "$(jq .bytes /tmp/base.json)" && break; sleep 2; done
rclone size --json s3:dittofs/test | tee /tmp/end.json
dfsctl store block audit-refcounts /test -o json | tee /tmp/audit.json && echo "audit: clean" || echo "audit: dangling refs"

test "$(jq .bytes /tmp/full.json)" -ge $((24 * 1024 * 1024))
test "$(jq .count /tmp/end.json)" -eq "$(jq .count /tmp/base.json)"
test "$(jq .bytes /tmp/end.json)" -eq "$(jq .bytes /tmp/base.json)"
test "$(jq .result.dangling_refs /tmp/audit.json)" -eq 0
