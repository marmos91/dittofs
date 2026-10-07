#!/usr/bin/env bash
# A crash loses nothing the clients were told was written. With S3 paused, so nothing has been
# uploaded, a file goes in over SMB and one over NFS, a directory is made and one file renamed into
# it. Then the server is killed (SIGKILL) and started again. On start it must re-apply from the
# journal what it had not committed yet, before serving (RFC 0 §10, RFC 8 §4.1). Every name, size
# and byte must be there, the uploads must resume, and the files must read back cold.
# Passes today. SMB serves straight after the restart. NFSv4 opens get NFS4ERR_GRACE for the 90 s
# grace period, which runs in full although the server expects no client to reclaim. It takes
# about 1.5 min.
NFS='nfs://127.0.0.1/test'
OPT='?version=4&uid=1000&gid=1000'

head -c 48M /dev/urandom >/tmp/a.bin
head -c 40M /dev/urandom >/tmp/b.bin
s3 pause
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin; mkdir d; rename a.bin d\a.bin'
nfs-cp /tmp/b.bin "$NFS/b.bin$OPT"
dfsctl store block stats --share /test -o json | jq -c '.totals | {unsynced_bytes, local_disk_used}'

# The crash, then the restart with S3 back
dfs-server kill
s3 resume
dfs-server start

# Outcomes first, then the checks: the names, then the bytes from the journal, then cold from S3
nfs-ls "$NFS$OPT" | tee /tmp/root.txt
nfs-ls "$NFS/d$OPT" | tee /tmp/d.txt
grep ' d$' /tmp/root.txt
grep ' b\.bin$' /tmp/root.txt
grep ' a\.bin$' /tmp/d.txt
test "$(grep -c ' a\.bin$' /tmp/root.txt)" -eq 0
smbclient //127.0.0.1/test -c 'get d\a.bin -' | cmp - /tmp/a.bin
# After an unclean restart NFSv4 is in its grace period, and an OPEN gets NFS4ERR_GRACE until it
# ends. A kernel client retries; this does the same, once a second.
for _ in $(seq 1 180); do nfs-cat "$NFS/b.bin$OPT" >/tmp/b-back.bin 2>/tmp/b-back.err && break; sleep 1; done
cat /tmp/b-back.err
cmp /tmp/b-back.bin /tmp/b.bin
dfsctl system drain-uploads
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'get d\a.bin -' | cmp - /tmp/a.bin
smbclient //127.0.0.1/test -c 'get b.bin -' | cmp - /tmp/b.bin
