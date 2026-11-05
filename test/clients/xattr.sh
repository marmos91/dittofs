#!/bin/sh
# Over the share mounted at $1, which mount_smbfs maps extended attributes to
# SMB named streams for: a user.* attribute and a com.apple.* one, as Finder
# tags are, read back as written, and the first is still there once the
# share is mounted again, from $SMB_URL.
set -eu
f=$1/xattr-test.txt
fail() { echo "FAIL: $*"; exit 1; }

echo xattr-body >"$f"
xattr -w user.dittofs.test hello-xattr "$f"
xattr -w com.apple.metadata:_kMDItemUserTags taggz "$f"
[ "$(xattr -p user.dittofs.test "$f")" = hello-xattr ] || fail "the user xattr reads back other content"
[ "$(xattr -p com.apple.metadata:_kMDItemUserTags "$f")" = taggz ] || fail "the com.apple xattr reads back other content"

umount "$1"
mount_smbfs -N -o nobrowse "$SMB_URL" "$1"
[ "$(xattr -p user.dittofs.test "$f")" = hello-xattr ] || fail "the user xattr is lost after the remount"
xattr -l "$f"
rm "$f"
echo "PASS: xattr round-trip + remount"
