#!/usr/bin/env bash
# Two block stores on the same bucket and prefix (RFC 6 §2.6, App A; RFC 9 §5.3): they are one remote
# namespace under two names, so GC on one can delete the other's objects. The second store must be refused.
# If it is accepted, /x and /y get a file each; deleting /x's file, GC on /x and a reclaim must leave /y's
# objects and data intact.
# Fails today at the last check: the second store is accepted. /y's data survives (GC reports 8 objects
# swept, the bucket keeps its 4).

rclone mkdir s3:dittofs/shared
dfsctl store block add --name s3-x --type s3 --config "$(jq -c '.bucket = "dittofs" | .prefix = "shared/"' /etc/dittofs-s3.json)"
dfsctl store block add --name s3-y --type s3 --config "$(jq -c '.bucket = "dittofs" | .prefix = "shared/"' /etc/dittofs-s3.json)" && Y=accepted || Y=refused
echo "second store on the same bucket and prefix: $Y"
if test "$Y" = accepted; then
  dfsctl share create --name /x --metadata md --block-store s3-x --default-permission none
  dfsctl share permission grant /x --user tester --level read-write
  dfsctl share create --name /y --metadata md --block-store s3-y --default-permission none
  dfsctl share permission grant /y --user tester --level read-write
  mkdir /tmp/x /tmp/y /tmp/back
  head -c 8M /dev/urandom >/tmp/x/x.bin
  head -c 8M /dev/urandom >/tmp/y/y.bin
  (cd /tmp/y && sha256sum y.bin >/tmp/y.sha256)
  smbclient //127.0.0.1/x -c 'lcd /tmp/x; put x.bin'
  smbclient //127.0.0.1/y -c 'lcd /tmp/y; put y.bin'
  dfsctl system drain-uploads
  rclone size --json s3:dittofs/shared | tee /tmp/s0.json
  smbclient //127.0.0.1/x -c 'del x.bin'
  dfsctl store block gc /x --grace-period 0 -o json
  dfsctl store block reclaim
  rclone size --json s3:dittofs/shared | tee /tmp/s1.json
  dfsctl store block evict --share /y
  smbclient //127.0.0.1/y -c 'lcd /tmp/back; get y.bin'
  test "$(jq .bytes /tmp/s1.json)" -ge "$(stat -c %s /tmp/y/y.bin)"
  (cd /tmp/back && sha256sum -c /tmp/y.sha256)
fi
test "$Y" = refused
