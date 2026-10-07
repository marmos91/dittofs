#!/usr/bin/env bash
# SMB, then NFS: write, read back, list, upload, cold read from S3, delete, GC.
# Random data, new for each protocol: repeated content can stay up to an hour after a delete (G-02).

test "$(rclone size --json s3:dittofs/test/ | jq .count)" -eq 0

mkdir /tmp/smb /tmp/back /tmp/cold
head -c 1M /dev/urandom >/tmp/smb/f1MiB.bin
head -c 4M /dev/urandom >/tmp/smb/f4MiB.bin
head -c 16M /dev/urandom >/tmp/smb/f16MiB.bin
head -c 33M /dev/urandom >/tmp/smb/f33MiB.bin
(cd /tmp/smb && sha256sum *.bin >/tmp/smb.sha256)
smbclient //127.0.0.1/test -c 'mkdir run; cd run; lcd /tmp/smb; prompt off; mput *.bin'

smbclient //127.0.0.1/test -c 'cd run; lcd /tmp/back; prompt off; mget *.bin'
(cd /tmp/back && sha256sum -c --quiet /tmp/smb.sha256)
test "$(smbclient //127.0.0.1/test -c 'ls run/*.bin' | grep -c '\.bin')" -eq 4

dfsctl system drain-uploads
rclone size --json s3:dittofs/test/ >/tmp/written.json
test "$(jq .count /tmp/written.json)" -gt 0
test "$(jq .bytes /tmp/written.json)" -ge $((54 * 1024 * 1024))
rclone lsf -R --files-only s3:dittofs/test/ >/tmp/keys.lst
test -z "$(grep -v '^blocks/' /tmp/keys.lst)"

dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'cd run; lcd /tmp/cold; prompt off; mget *.bin'
(cd /tmp/cold && sha256sum -c --quiet /tmp/smb.sha256)

smbclient //127.0.0.1/test -c 'deltree run'
test "$(smbclient //127.0.0.1/test -c 'ls' | grep -cw run)" -eq 0

dfsctl store block gc /test --grace-period 0
for _ in $(seq 1 60); do rclone size --json s3:dittofs/test/ >/tmp/gc.json && test "$(jq .count /tmp/gc.json)" -eq 0 && break; sleep 2; done
test "$(jq .count /tmp/gc.json)" -eq 0

mkdir /tmp/nfs /tmp/nback /tmp/ncold
head -c 1M /dev/urandom >/tmp/nfs/n1MiB.bin
head -c 4M /dev/urandom >/tmp/nfs/n4MiB.bin
head -c 16M /dev/urandom >/tmp/nfs/n16MiB.bin
head -c 33M /dev/urandom >/tmp/nfs/n33MiB.bin
(cd /tmp/nfs && sha256sum *.bin >/tmp/nfs.sha256)
nfs-io mkdir 'nfs://127.0.0.1/test/run?version=4&uid=1000&gid=1000'
for F in n1MiB.bin n4MiB.bin n16MiB.bin n33MiB.bin; do nfs-cp "/tmp/nfs/$F" "nfs://127.0.0.1/test/run/$F?version=4&uid=1000&gid=1000"; done

for F in n1MiB.bin n4MiB.bin n16MiB.bin n33MiB.bin; do nfs-cp "nfs://127.0.0.1/test/run/$F?version=4&uid=1000&gid=1000" "/tmp/nback/$F"; done
(cd /tmp/nback && sha256sum -c --quiet /tmp/nfs.sha256)
test "$(nfs-ls 'nfs://127.0.0.1/test/run?version=4&uid=1000&gid=1000' | grep -c 'n[0-9]*MiB\.bin')" -eq 4

dfsctl system drain-uploads
rclone size --json s3:dittofs/test/ >/tmp/nwritten.json
test "$(jq .count /tmp/nwritten.json)" -gt 0
test "$(jq .bytes /tmp/nwritten.json)" -ge $((54 * 1024 * 1024))
rclone lsf -R --files-only s3:dittofs/test/ >/tmp/keys.lst
test -z "$(grep -v '^blocks/' /tmp/keys.lst)"

dfsctl store block evict --share /test
for F in n1MiB.bin n4MiB.bin n16MiB.bin n33MiB.bin; do nfs-cp "nfs://127.0.0.1/test/run/$F?version=4&uid=1000&gid=1000" "/tmp/ncold/$F"; done
(cd /tmp/ncold && sha256sum -c --quiet /tmp/nfs.sha256)

for F in n1MiB.bin n4MiB.bin n16MiB.bin n33MiB.bin; do nfs-io unlink "nfs://127.0.0.1/test/run/$F?version=4&uid=1000&gid=1000"; done
nfs-io rmdir 'nfs://127.0.0.1/test/run?version=4&uid=1000&gid=1000'
test "$(nfs-ls 'nfs://127.0.0.1/test?version=4&uid=1000&gid=1000' | grep -cw run)" -eq 0

dfsctl store block gc /test --grace-period 0
for _ in $(seq 1 60); do rclone size --json s3:dittofs/test/ >/tmp/ngc.json && test "$(jq .count /tmp/ngc.json)" -eq 0 && break; sleep 2; done
test "$(jq .count /tmp/ngc.json)" -eq 0
