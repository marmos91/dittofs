#!/usr/bin/env bash
# Two shares on one block store and one metadata store (RFC 6 §2.6, RFC 9 §3.6; MS-02): dedup spans the
# shared namespace, so GC on one share must not free what the other still references. /a and /b each get
# a distinct file, and the same shared.bin. Deleting /a's distinct file and running GC on /a frees about its
# size; deleting /a's shared.bin then frees nothing. /b's files read back cold from S3 after both.

dfsctl share create --name /a --metadata md --block-store s3 --default-permission none
dfsctl share permission grant /a --user tester --level read-write
dfsctl share create --name /b --metadata md --block-store s3 --default-permission none
dfsctl share permission grant /b --user tester --level read-write

mkdir /tmp/a /tmp/b /tmp/back
head -c 8M /dev/urandom >/tmp/a/only.bin
head -c 8M /dev/urandom >/tmp/b/only.bin
head -c 8M /dev/urandom >/tmp/a/shared.bin
cp /tmp/a/shared.bin /tmp/b/shared.bin
(cd /tmp/b && sha256sum only.bin shared.bin >/tmp/b.sha256)
smbclient //127.0.0.1/a -c 'lcd /tmp/a; put only.bin; put shared.bin'
dfsctl system drain-uploads
smbclient //127.0.0.1/b -c 'lcd /tmp/b; put only.bin; put shared.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs | tee /tmp/s0.json

smbclient //127.0.0.1/a -c 'del only.bin'
dfsctl store block gc /a --grace-period 0 -o json | tee /tmp/gc1.json
dfsctl store block evict --share /b
rclone size --json s3:dittofs | tee /tmp/s1.json
smbclient //127.0.0.1/b -c 'lcd /tmp/back; get only.bin; get shared.bin'
(cd /tmp/back && sha256sum -c /tmp/b.sha256) && echo "b after gc1: ok" || echo "b after gc1: FAILED"

smbclient //127.0.0.1/a -c 'del shared.bin'
dfsctl store block gc /a --grace-period 0 -o json | tee /tmp/gc2.json
dfsctl store block evict --share /b
rclone size --json s3:dittofs | tee /tmp/s2.json
rm /tmp/back/*
smbclient //127.0.0.1/b -c 'lcd /tmp/back; get only.bin; get shared.bin'

(cd /tmp/back && sha256sum -c /tmp/b.sha256)
FREED1=$(($(jq .bytes /tmp/s0.json) - $(jq .bytes /tmp/s1.json)))
echo "gc1 freed $FREED1 bucket bytes, reported $(jq .bytes_freed /tmp/gc1.json)"
test "$FREED1" -ge $((8 * 1024 * 1024))
test "$FREED1" -lt $((9 * 1024 * 1024))
test "$(jq .bytes_freed /tmp/gc1.json)" -eq "$FREED1"
test "$(jq .bytes /tmp/s2.json)" -eq "$(jq .bytes /tmp/s1.json)"
test "$(jq .bytes_freed /tmp/gc2.json)" -eq 0
