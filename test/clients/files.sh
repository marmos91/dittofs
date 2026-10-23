#!/bin/sh
# Over the share mounted at $1: a file written and read back, a directory
# made, the file listed, then both deleted.
set -eu
cd "$1"
fail() { echo "FAIL: $*"; exit 1; }

echo "hello from $(uname -s)" >file.txt
[ "$(cat file.txt)" = "hello from $(uname -s)" ] || fail "file.txt reads back other content"
echo "PASS: file create and read"

mkdir dir
[ -d dir ] || fail "dir isn't a directory"
echo "PASS: directory create"

printf '%s\n' * | grep -qx file.txt || fail "file.txt isn't listed"
echo "PASS: directory listing"

rm file.txt
rmdir dir
[ ! -e file.txt ] && [ ! -e dir ] || fail "file.txt or dir is still there"
echo "PASS: delete"
