#!/usr/bin/env bash
# Issue #2910: a large sequential SMB write completes at the client while the object store holds a
# fraction of it. Two S3 block stores are present on the one metadata store, /test on bucket dittofs
# and /cubbit on bucket cubbit, and only /test is used: a 125 GB file goes to it. After the drain the
# dittofs bucket must hold all of it, the cubbit bucket must be unchanged, and a cold read must match
# the file byte for byte. The file is kept on disk as CACHE/inputs/125GB.testfile and reused; it is
# made only when missing. Run with SCENARIO_TIMEOUT=10800.
F=/inputs/125GB.testfile

rclone mkdir s3:cubbit
dfsctl store block add --name s3-cubbit --type s3 --config "$(jq -c '.bucket = "cubbit"' /etc/dittofs-s3.json)"
dfsctl share create --name /cubbit --metadata md --block-store s3-cubbit --default-permission none
dfsctl share permission grant /cubbit --user tester --level read-write
rclone lsf -R --files-only --format ps s3:cubbit >/tmp/cubbit0.lst

# Disk for three copies (the input, the local tier's, SeaweedFS's) and 10 GB; a kept input counts as room
test "$(stat -c %s "$F" 2>/dev/null)" = 125000000000 || rm -f "$F"
test $(($(df -B1 --output=avail /inputs | tail -1) + $(stat -c %s "$F" 2>/dev/null || echo 0))) -gt 385000000000
test -f "$F" || head -c 125GB /dev/urandom >"$F"

smbclient //127.0.0.1/test -c "put $F 125GB.testfile"
dfsctl system drain-uploads --timeout 120m
dfsctl store block stats --share /test -o json | jq -c '.totals | {unsynced_bytes, pending_uploads, local_disk_used}'
rclone size --json s3:dittofs/test/ | tee /tmp/test.json
rclone lsf -R --files-only --format ps s3:cubbit | tee /tmp/cubbit.lst
test "$(jq .bytes /tmp/test.json)" -ge "$(stat -c %s "$F")"
cmp /tmp/cubbit.lst /tmp/cubbit0.lst
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'get 125GB.testfile -' | cmp - "$F"
