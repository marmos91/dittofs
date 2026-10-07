#!/usr/bin/env bash
# With S3 hanging (it accepts connections and never answers), every client call must still end at a
# deadline: an operation with none of its own gets 30 s, so that every wait below it ends (RFC 17
# §4.3, RFC 0 §10.3). Checked with 15 s of margin, each call timed: a cold read over SMB and over
# NFS, a small write, listings from both protocols and a dfsctl stats call. Both cold reads must
# fail, as S3 holds the only copy, with no wrong bytes; once S3 resumes, the file reads again.
# Fails today: both cold reads end after 60 s, the bound set by DefaultDemandFetchTimeout
# (pkg/block/engine/types.go), past the 30 s. They fail cleanly with no wrong bytes. The write, the
# listings and dfsctl answer at once. It takes about 2.5 min.
NFS='nfs://127.0.0.1/test'
OPT='?version=4&uid=1000&gid=1000'

head -c 64M /dev/urandom >/tmp/a.bin
head -c 1M /dev/urandom >/tmp/b.bin
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
dfsctl store block evict --share /test

# S3 paused: each call is timed between two timestamps
s3 pause
date +%s >/tmp/t0
timeout 300 smbclient //127.0.0.1/test -c 'get a.bin /tmp/smb.bin' >/tmp/smb.out 2>&1 || echo "exit $?" >/tmp/smb.rc
date +%s >/tmp/t1
timeout 300 nfs-cp "$NFS/a.bin$OPT" /tmp/nfs.bin >/tmp/nfs.out 2>&1 || echo "exit $?" >/tmp/nfs.rc
date +%s >/tmp/t2
timeout 300 smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin' >/tmp/put.out 2>&1 || echo "exit $?" >/tmp/put.rc
date +%s >/tmp/t3
timeout 300 smbclient //127.0.0.1/test -c 'ls' >/tmp/ls.out 2>&1 || echo "exit $?" >/tmp/ls.rc
timeout 300 nfs-ls "$NFS$OPT" >>/tmp/ls.out 2>&1 || echo "exit $?" >>/tmp/ls.rc
date +%s >/tmp/t4
timeout 300 dfsctl store block stats --share /test >/tmp/stats.out 2>&1 || echo "exit $?" >/tmp/stats.rc
date +%s >/tmp/t5
touch /tmp/smb.bin /tmp/nfs.bin

# S3 resumed: the file reads again and the drain completes
s3 resume
for _ in $(seq 1 120); do smbclient //127.0.0.1/test -c 'get a.bin /tmp/back.bin' >/dev/null 2>&1 && break; sleep 1; done
timeout 300 dfsctl system drain-uploads

# Outcomes first, then the checks: calls within 45 s, listings and stats 10 s, cold reads failed
cat /tmp/smb.out /tmp/nfs.out /tmp/put.out /tmp/ls.out
cat /tmp/*.rc 2>/dev/null || true
paste -d ' ' /tmp/t0 /tmp/t1 /tmp/t2 /tmp/t3 /tmp/t4 /tmp/t5
test $(($(cat /tmp/t1) - $(cat /tmp/t0))) -le 45
test $(($(cat /tmp/t2) - $(cat /tmp/t1))) -le 45
test $(($(cat /tmp/t3) - $(cat /tmp/t2))) -le 45
test $(($(cat /tmp/t4) - $(cat /tmp/t3))) -le 10
test $(($(cat /tmp/t5) - $(cat /tmp/t4))) -le 10
test -s /tmp/smb.rc
test -s /tmp/nfs.rc
cmp -n "$(stat -c %s /tmp/smb.bin)" /tmp/smb.bin /tmp/a.bin
cmp -n "$(stat -c %s /tmp/nfs.bin)" /tmp/nfs.bin /tmp/a.bin
cmp /tmp/back.bin /tmp/a.bin
