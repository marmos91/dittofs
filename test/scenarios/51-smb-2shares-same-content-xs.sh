#!/usr/bin/env bash
# A-07: two shares on different block stores but one metadata store. A file
# written to /export, then the same content to /cubbit: /cubbit's bucket must get the data, and
# /cubbit must read it back after its caches are evicted. Fails today: the upload is skipped
# (the content is already known) and the cold read on /cubbit returns an I/O error.

rclone mkdir s3:export
dfsctl store block add --name s3-export --type s3 --config "$(jq -c '.bucket = "export"' /etc/dittofs-s3.json)"
dfsctl share create --name /export --metadata md --block-store s3-export --default-permission none
dfsctl share permission grant /export --user tester --level read-write
rclone mkdir s3:cubbit
dfsctl store block add --name s3-cubbit --type s3 --config "$(jq -c '.bucket = "cubbit"' /etc/dittofs-s3.json)"
dfsctl share create --name /cubbit --metadata md --block-store s3-cubbit --default-permission none
dfsctl share permission grant /cubbit --user tester --level read-write

head -c 4M /dev/urandom >/tmp/d.bin
(cd /tmp && sha256sum d.bin >/tmp/d.sha256)
smbclient //127.0.0.1/export -c 'lcd /tmp; put d.bin'
dfsctl system drain-uploads
test "$(rclone size --json s3:export | jq .count)" -gt 0
smbclient //127.0.0.1/cubbit -c 'lcd /tmp; put d.bin'
dfsctl system drain-uploads
test "$(rclone size --json s3:cubbit | jq .count)" -gt 0
dfsctl store block evict --share /cubbit
mkdir /tmp/back
smbclient //127.0.0.1/cubbit -c 'lcd /tmp/back; get d.bin'
(cd /tmp/back && sha256sum -c --quiet /tmp/d.sha256)
