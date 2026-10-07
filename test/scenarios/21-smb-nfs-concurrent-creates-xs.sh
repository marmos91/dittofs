#!/usr/bin/env bash
# Concurrent creates in one directory from both protocols (RFC 0 §9.2 I8, RFC 7 App A; RO-02): 32 smbclient
# and 32 nfs-cp processes each create 20 files in /test/d while an NFS loop chmods the directory. Every
# create must succeed, with no NT_STATUS_IO_DEVICE_ERROR or NFS4ERR_IO, and d must hold exactly the
# 1,280 files, each once, each with its content.

N='?version=4&uid=1000&gid=1000'
mkdir /tmp/src /tmp/back /tmp/log
for p in $(seq 1 32); do for i in $(seq 1 20); do echo "s$p-$i" >/tmp/src/s$p-$i; echo "n$p-$i" >/tmp/src/n$p-$i; done; done
nfs-io mkdir "nfs://127.0.0.1/test/d$N"
(while ! test -e /tmp/stop; do nfs-io chmod 0755 "nfs://127.0.0.1/test/d$N"; nfs-io chmod 0775 "nfs://127.0.0.1/test/d$N"; done) >/tmp/log/chmod 2>&1 &
for p in $(seq 1 32); do (smbclient //127.0.0.1/test -c "cd d; lcd /tmp/src; prompt; mput s$p-*" >/tmp/log/s$p 2>&1; echo "rc=$?" >>/tmp/log/s$p) & W+=($!); done
for p in $(seq 1 32); do (for i in $(seq 1 20); do nfs-cp /tmp/src/n$p-$i "nfs://127.0.0.1/test/d/n$p-$i$N" || echo "n$p-$i failed"; done >/tmp/log/n$p 2>&1) & W+=($!); done
wait "${W[@]}"
touch /tmp/stop
wait
grep -h 'rc=' /tmp/log/s* | sort | uniq -c
grep -hE 'NT_STATUS_|NFS4ERR|failed' /tmp/log/* | sort | uniq -c | head -20 || true
nfs-ls "nfs://127.0.0.1/test/d$N" | awk '{print $NF}' | grep -E '^[sn][0-9]+-[0-9]+$' | sort >/tmp/listing
wc -l </tmp/listing
uniq -d /tmp/listing | head
smbclient //127.0.0.1/test -c 'cd d; lcd /tmp/back; prompt; mget *'

test "$(grep -h 'rc=' /tmp/log/s* | grep -vc 'rc=0')" -eq 0
! grep -qhE 'NT_STATUS_IO_DEVICE_ERROR|NFS4ERR_IO|failed' /tmp/log/*
test "$(wc -l </tmp/listing)" -eq 1280
test -z "$(uniq -d /tmp/listing)"
diff -r /tmp/src /tmp/back
