#!/bin/sh
# Over the share mounted at $1: a file renamed is there under its new name,
# with its content, and no longer under its old one.
set -eu
cd "$1"
fail() { echo "FAIL: $*"; exit 1; }

echo renamed >before.txt
mv before.txt after.txt
[ "$(cat after.txt)" = renamed ] || fail "after.txt doesn't hold what before.txt did"
[ ! -e before.txt ] || fail "before.txt is still there"
rm after.txt
echo "PASS: rename"
