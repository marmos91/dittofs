#!/usr/bin/env bash
# Issue #2906: with two shares on one metadata store, per-share views walk every share's files.
# /export gets a 16 MiB file; /cubbit, created after it on its own bucket, stays empty: no files or
# blocks in its stats, offline-safe (its start seeds /export's ranges, which the offline check then
# counts as /cubbit's), and a warm that fetches nothing. Not covered: reconcile (a deleted, open file).

rclone mkdir s3:export
dfsctl store block add --name s3-export --type s3 --config "$(jq -c '.bucket = "export"' /etc/dittofs-s3.json)"
dfsctl share create --name /export --metadata md --block-store s3-export --default-permission none
dfsctl share permission grant /export --user tester --level read-write

head -c 16M /dev/urandom >/tmp/e.bin
smbclient //127.0.0.1/export -c 'lcd /tmp; put e.bin'
dfsctl system drain-uploads

rclone mkdir s3:cubbit
dfsctl store block add --name s3-cubbit --type s3 --config "$(jq -c '.bucket = "cubbit"' /etc/dittofs-s3.json)"
dfsctl share create --name /cubbit --metadata md --block-store s3-cubbit --default-permission none

dfsctl store block stats --share /export -o json | jq -c '.totals | {file_count, blocks_total}'
dfsctl store block stats --share /cubbit -o json | jq -c '.totals | {file_count, blocks_total}'
dfsctl share show /cubbit | grep 'Offline Safe'
dfsctl share warm /cubbit --watch >/tmp/warm.out 2>&1 || true
cat /tmp/warm.out

test "$(dfsctl store block stats --share /cubbit -o json | jq '.totals.file_count + .totals.blocks_total')" -eq 0
test "$(dfsctl share show /cubbit | grep 'Offline Safe' | awk '{print $3}')" = yes
test -z "$(grep -i failed /tmp/warm.out)"
