#!/bin/sh
# A file written over the share mounted at $1 reaches the S3 bucket as a
# block, which the listing at $2, {bucket}, shows. Reads can be served from
# the server's local tier, so only the bucket shows that S3 is in the data
# path. The file stays until a block of it lands, since one deleted before
# the syncer uploads it never reaches S3.
set -eu
dd if=/dev/urandom of="$1/s3-check.bin" bs=1M count=1 2>/dev/null
tries=30
until curl -sf "$2" | grep -q '<Key>[^<]*/blocks/'; do
	tries=$((tries - 1))
	[ "$tries" -gt 0 ] || { echo "FAIL: no block reached the bucket in 30s"; exit 1; }
	sleep 1
done
rm "$1/s3-check.bin"
echo "PASS: a block reached the S3 bucket"
