#!/usr/bin/env bash
# A remote block that is missing or altered must make the read fail: a read never returns zeros for
# data that exists (RFC 8 §7.1, §7.7), nor bytes it could not verify (RFC 4 §3.4, RFC 8 §7.2).
# Three files go up one by one, so each one's objects are known. One of a.bin's objects is deleted,
# one of b.bin's is overwritten with random bytes of the same size, and c.bin is left alone. After
# an evict, all three are read over SMB and NFS.
# Passes today. Both damaged files fail at the damaged block, SMB with
# NT_STATUS_UNEXPECTED_IO_ERROR and NFS with an I/O error. The bytes before it are intact, and
# c.bin reads whole.
NFS='nfs://127.0.0.1/test'
OPT='?version=4&uid=1000&gid=1000'

head -c 32M /dev/urandom >/tmp/a.bin
head -c 32M /dev/urandom >/tmp/b.bin
head -c 32M /dev/urandom >/tmp/c.bin
rclone lsf -R --files-only s3:dittofs/test/ | sort >/tmp/objects0
smbclient //127.0.0.1/test -c 'lcd /tmp; put a.bin'
dfsctl system drain-uploads
rclone lsf -R --files-only s3:dittofs/test/ | sort >/tmp/objects1
smbclient //127.0.0.1/test -c 'lcd /tmp; put b.bin'
dfsctl system drain-uploads
rclone lsf -R --files-only s3:dittofs/test/ | sort >/tmp/objects2
smbclient //127.0.0.1/test -c 'lcd /tmp; put c.bin'
dfsctl system drain-uploads
comm -13 /tmp/objects0 /tmp/objects1 | tee /tmp/a.objects
comm -13 /tmp/objects1 /tmp/objects2 | tee /tmp/b.objects

# Damage: one of a.bin's objects goes, one of b.bin's is replaced by noise of the same size
rclone deletefile "s3:dittofs/test/$(head -1 /tmp/a.objects)"
rclone cat "s3:dittofs/test/$(head -1 /tmp/b.objects)" | wc -c >/tmp/b.size
head -c "$(cat /tmp/b.size)" /dev/urandom | rclone rcat "s3:dittofs/test/$(head -1 /tmp/b.objects)"
dfsctl store block evict --share /test

# Reads: a.bin and b.bin must fail, c.bin must not; each one's output is kept to compare
for f in a b c; do timeout 300 smbclient //127.0.0.1/test -c "get $f.bin /tmp/smb-$f.bin" >"/tmp/smb-$f.out" 2>&1 || echo "exit $?" >"/tmp/smb-$f.rc"; done
for f in a b c; do timeout 300 nfs-cat "$NFS/$f.bin$OPT" >"/tmp/nfs-$f.bin" 2>"/tmp/nfs-$f.out" || echo "exit $?" >"/tmp/nfs-$f.rc"; done
touch /tmp/smb-a.bin /tmp/smb-b.bin /tmp/smb-c.bin

# Outcomes first, then the checks: no byte read may differ from the original at its offset
cat /tmp/smb-?.out /tmp/nfs-?.out
for f in /tmp/*.rc; do echo "$f $(cat "$f")"; done
stat -c '%n %s' /tmp/smb-?.bin /tmp/nfs-?.bin
for f in a b c; do cmp -n "$(stat -c %s "/tmp/smb-$f.bin")" "/tmp/smb-$f.bin" "/tmp/$f.bin"; done
for f in a b c; do cmp -n "$(stat -c %s "/tmp/nfs-$f.bin")" "/tmp/nfs-$f.bin" "/tmp/$f.bin"; done
test -e /tmp/smb-a.rc
test -e /tmp/nfs-a.rc
test -e /tmp/smb-b.rc
test -e /tmp/nfs-b.rc
cmp /tmp/smb-c.bin /tmp/c.bin
cmp /tmp/nfs-c.bin /tmp/c.bin
