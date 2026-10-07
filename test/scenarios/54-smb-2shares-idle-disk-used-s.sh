#!/usr/bin/env bash
# Issue #2908: /export's Local Disk Used went negative while /cubbit, on the same metadata store,
# deleted files. /cubbit gets three medium files, then /export is created (its journal is seeded with
# /cubbit's ranges) and gets a large one. Delete the large one, then the medium ones, GC after each:
# each share's Local Disk Used must always equal its journal's .seg sizes, which a restart reports.
J=/root/.local/state/dittofs/blocks/shares

rclone mkdir s3:cubbit
dfsctl store block add --name s3-cubbit --type s3 --config "$(jq -c '.bucket = "cubbit"' /etc/dittofs-s3.json)"
dfsctl share create --name /cubbit --metadata md --block-store s3-cubbit --default-permission none
dfsctl share permission grant /cubbit --user tester --level read-write

mkdir /tmp/e /tmp/c
head -c 768M /dev/urandom >/tmp/e/large.bin
head -c 100M /dev/urandom >/tmp/c/m1.bin
head -c 100M /dev/urandom >/tmp/c/m2.bin
head -c 100M /dev/urandom >/tmp/c/m3.bin
smbclient //127.0.0.1/cubbit -c 'lcd /tmp/c; put m1.bin; put m2.bin; put m3.bin'
dfsctl system drain-uploads

rclone mkdir s3:export
dfsctl store block add --name s3-export --type s3 --config "$(jq -c '.bucket = "export"' /etc/dittofs-s3.json)"
dfsctl share create --name /export --metadata md --block-store s3-export --default-permission none
dfsctl share permission grant /export --user tester --level read-write
smbclient //127.0.0.1/export -c 'lcd /tmp/e; put large.bin'
dfsctl system drain-uploads
test -d "$J/export/journal"
test -d "$J/cubbit/journal"

for STEP in upload export/large.bin cubbit/m1.bin cubbit/m2.bin cubbit/m3.bin; do
  test "$STEP" = upload || smbclient "//127.0.0.1/${STEP%/*}" -c "del ${STEP#*/}"
  test "$STEP" = upload || dfsctl store block gc "/${STEP%/*}" --grace-period 0
  dfsctl store block stats
  test "$(dfsctl store block stats --share /export -o json | jq .totals.local_disk_used)" -eq "$(find "$J/export/journal" -name '*.seg' -printf '%s\n' | awk '{s += $1} END {print s + 0}')"
  test "$(dfsctl store block stats --share /cubbit -o json | jq .totals.local_disk_used)" -eq "$(find "$J/cubbit/journal" -name '*.seg' -printf '%s\n' | awk '{s += $1} END {print s + 0}')"
done
