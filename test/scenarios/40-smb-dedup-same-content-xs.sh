#!/usr/bin/env bash
# Deduplication on /test: content-defined chunks whose hash has already been uploaded are not
# uploaded again (chunk sizes: RFC 2). a.bin goes up first; b.bin, the same bytes under another
# name, must add almost nothing to the bucket; c.bin, new random bytes of the same size, must add at
# least its size (the control that the bucket size sees new data). b.bin then reads back intact from S3.
# Dedup only skips chunks already uploaded, so each file is drained before the next one is written.

head -c 16M /dev/urandom >/tmp/a.bin
cp /tmp/a.bin /tmp/b.bin
head -c 16M /dev/urandom >/tmp/c.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test >/tmp/after-a.json
smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test >/tmp/after-b.json
smbclient //127.0.0.1/test -c 'lcd /tmp; put c.bin'
dfsctl system drain-uploads
rclone size --json s3:dittofs/test >/tmp/after-c.json
dfsctl store block evict --share /test
mkdir /tmp/back
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get b.bin'

# Outcomes first, then the checks
cat /tmp/after-a.json /tmp/after-b.json /tmp/after-c.json
test "$(jq .bytes /tmp/after-a.json)" -ge "$(stat -c %s /tmp/a.bin)"
test "$(($(jq .bytes /tmp/after-b.json) - $(jq .bytes /tmp/after-a.json)))" -lt $((1024 * 1024))
test "$(($(jq .bytes /tmp/after-c.json) - $(jq .bytes /tmp/after-b.json)))" -ge "$(stat -c %s /tmp/c.bin)"
cmp /tmp/b.bin /tmp/back/b.bin
