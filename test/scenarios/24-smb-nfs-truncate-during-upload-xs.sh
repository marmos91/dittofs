#!/usr/bin/env bash
# A truncate while the upload is in flight never brings the cut bytes back (RFC 8 §8.3, RFC 6 §6.2;
# FO-06). With S3 paused, f (10 MiB random) is written over SMB, so its upload stalls, and cut to 5 MiB
# over NFS. S3 resumes, the uploads drain, f grows back to 10 MiB over NFS. Read cold over SMB and NFS,
# twice: the first 5 MiB are the original, the rest zeros.

N='?version=4&uid=1000&gid=1000'
mkdir /tmp/back
head -c 10M /dev/urandom >/tmp/f
head -c 5M /tmp/f >/tmp/want
head -c 5M /dev/zero >>/tmp/want
s3 pause
timeout 120 smbclient //127.0.0.1/test -c 'lcd /tmp; put f'
nfs-truncate "nfs://127.0.0.1/test/f$N" 5242880
s3 resume
dfsctl system drain-uploads
nfs-truncate "nfs://127.0.0.1/test/f$N" 10485760
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get f smb1'
nfs-cp "nfs://127.0.0.1/test/f$N" /tmp/back/nfs1
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get f smb2'
nfs-cp "nfs://127.0.0.1/test/f$N" /tmp/back/nfs2
for F in /tmp/back/*; do cmp "$F" /tmp/want && echo "$F: ok" || echo "$F: differs"; done

for F in /tmp/back/*; do cmp "$F" /tmp/want; done
