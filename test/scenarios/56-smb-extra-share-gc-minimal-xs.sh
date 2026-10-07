#!/usr/bin/env bash
# The smallest check for A-08: a second block store on md (/extra, own bucket) beside setup.sh's
# /test. Each round deletes one file and runs GC on /extra; its bucket must shrink. A-08 failed it
# whenever GC visited /test's remote first. 55-smb-extra-share-gc-xs.sh is the same setup with the full checks.
rclone mkdir s3:extra
dfsctl store block add --name s3-extra --type s3 --config "$(jq -c '.bucket = "extra"' /etc/dittofs-s3.json)"
dfsctl share create --name /extra --metadata md --block-store s3-extra --default-permission none
dfsctl share permission grant /extra --user tester --level read-write
mkdir /tmp/up
for i in 1 2 3 4 5 6 7; do echo "$i" >"/tmp/up/f$i"; done
smbclient //127.0.0.1/extra -c 'lcd /tmp/up; prompt; mput f*'
dfsctl system drain-uploads
for i in 1 2 3 4 5 6 7; do before="$(rclone size --json s3:extra | jq .count)"; smbclient //127.0.0.1/extra -c "del f$i"; dfsctl store block gc /extra --grace-period 0 -o json >/dev/null; test "$(rclone size --json s3:extra | jq .count)" -lt "$before"; done
