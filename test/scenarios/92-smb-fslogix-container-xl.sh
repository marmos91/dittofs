#!/usr/bin/env bash
# An FSLogix Office container, the first enterprise customer's workload. Each user's Outlook data
# lives in one VHDX on the SMB share (ODFC_<user>.vhdx, beside a .vhdx.metadata file under 1 MB):
# about 10 GB for a 5 GB mailbox, growing up to FSLogix's default cap of 30 GB. Windows mounts it at
# sign-in and writes in place through one handle for the whole session.
# Here: a 10 GB container at rest (put, offloaded, evicted); a session that reads the metadata file,
# then through one handle writes 2000 random 64 KiB blocks in place and grows the container to 30 GB
# in 1 MiB writes; then sign-out: shrunk to 24 GB by a truncate, as FSLogix's compaction ends, then
# drained, evicted, and both files read cold, byte for byte against the local copies, which took the
# same writes. The 10 GB input is kept as CACHE/inputs/10GB.testfile and reused.
# It takes about 30 min, mostly the session's writes and the cold read: run with SCENARIO_TIMEOUT=3600.
F=/inputs/10GB.testfile
VHDX=smb://127.0.0.1/test/ODFC_tester.vhdx

# Disk for the input, the session's local copy, the local tier and SeaweedFS, and 10 GB more
test "$(stat -c %s "$F" 2>/dev/null)" = 10000000000 || rm -f "$F"
test $(($(df -B1 --output=avail /inputs | tail -1) + $(stat -c %s "$F" 2>/dev/null || echo 0))) -gt 110000000000
test -f "$F" || head -c 10GB /dev/urandom >"$F"

# At rest: the container and its metadata file on the share, offloaded and evicted
cp "$F" /tmp/ODFC_tester.vhdx
head -c 700K /dev/urandom >/tmp/ODFC_tester.vhdx.metadata
smbclient //127.0.0.1/test -c 'lcd /tmp; put ODFC_tester.vhdx; put ODFC_tester.vhdx.metadata'
dfsctl system drain-uploads --timeout 60m
dfsctl store block evict --share /test

# A session: the metadata file read at sign-in, then one handle that writes 2000 random 4 KiB-aligned
# 64 KiB blocks in place and grows the container to 30 GB. The local copy takes the same bytes first.
smbclient //127.0.0.1/test -c 'get ODFC_tester.vhdx.metadata -' | cmp - /tmp/ODFC_tester.vhdx.metadata
awk 'BEGIN { srand(42); for (i = 0; i < 2000; i++) printf "%d 65536\n", int(rand() * 2441390) * 4096 }' >/tmp/overwrite.plan
awk 'BEGIN { for (o = 10000000000; o < 30000000000; o += 1048576) printf "%d %d\n", o, (30000000000 - o < 1048576 ? 30000000000 - o : 1048576) }' >/tmp/grow.plan
while read -r o n; do head -c "$n" /dev/urandom | dd of=/tmp/ODFC_tester.vhdx bs=64K seek="$o" oflag=seek_bytes conv=notrunc status=none; done </tmp/overwrite.plan
head -c 20GB /dev/urandom >>/tmp/ODFC_tester.vhdx
cat /tmp/overwrite.plan /tmp/grow.plan | smb-pwrite "$VHDX" /tmp/ODFC_tester.vhdx

# Sign-out: FSLogix's compaction ends with a truncate (SetEndOfFile) that gives the freed tail back;
# here it cuts the container to 24 GB. Then drained and evicted, and both files read cold.
smb-truncate "$VHDX" 24000000000
truncate -s 24000000000 /tmp/ODFC_tester.vhdx
dfsctl system drain-uploads --timeout 60m
dfsctl store block evict --share /test
dfsctl store block stats --share /test -o json | jq -c '.totals | {unsynced_bytes, pending_uploads, local_disk_used}'
rclone size --json s3:dittofs/test/
smbclient //127.0.0.1/test -c 'get ODFC_tester.vhdx.metadata -' | cmp - /tmp/ODFC_tester.vhdx.metadata
smbclient //127.0.0.1/test -c 'get ODFC_tester.vhdx -' | cmp - /tmp/ODFC_tester.vhdx
