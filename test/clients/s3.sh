#!/bin/sh
# A file written over the share mounted at $1 reaches the S3 bucket as a
# block, which the listing at $2, {bucket}, shows. Reads can be served from
# the server's local tier, so only the bucket shows that S3 is in the data
# path. The file stays until a block of it lands, since one deleted before
# the syncer uploads it never reaches S3. The blocks are counted before the
# write, since the tests before this one in the cell may have left some. A
# listing that fails stops the check before the write: counted as no blocks,
# it would let a block left by an earlier test pass the check.
set -eu
blocks() { listing=$(curl -sf "$2") && printf '%s\n' "$listing" | grep -o '<Key>[^<]*/blocks/' | wc -l; }
before=$(blocks "$@") || { echo "FAIL: the bucket listing at $2 failed"; exit 1; }
dd if=/dev/urandom of="$1/s3-check.bin" bs=1M count=1 2>/dev/null
tries=30
until now=$(blocks "$@") && [ "$now" -gt "$before" ]; do
	tries=$((tries - 1))
	[ "$tries" -gt 0 ] || { echo "FAIL: no new block reached the bucket in 30s"; exit 1; }
	sleep 1
done
rm "$1/s3-check.bin"
echo "PASS: a block reached the S3 bucket"
