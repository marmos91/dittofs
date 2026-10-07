#!/usr/bin/env bash
# A share with a 1 GiB journal and S3 up takes far more than the journal holds: 5 GiB over SMB, then
# 5 GiB over NFS. The writes must succeed, waiting for space when they must (RFC 0 §10). Local disk
# use, sampled every 5 s, must stay near the cap, which is a bound and not a target (RFC 0 §10.1).
# Both files must read back cold.
# Passes today. The SMB put runs at about 115 MB/s and the NFS copy at about 160 MB/s. Local use
# peaks 1 MB over the 1 GiB cap, and the drain after them takes under a second. Compare #2910 and
# 91-smb-125gb-file-sync-xxl.sh: with the default journal, which holds the whole file unsynced, the same
# host writes at 39 MB/s falling to 14. It takes about 2.5 min.
NFS='nfs://127.0.0.1/small'
OPT='?version=4&uid=1000&gid=1000'

rclone mkdir s3:small
dfsctl store block add --name s3-small --type s3 --config "$(jq -c '.bucket = "small"' /etc/dittofs-s3.json)"
dfsctl share create --name /small --metadata md --block-store s3-small --default-permission none --journal-size 1GiB
dfsctl share permission grant /small --user tester --level read-write
head -c 5G /dev/urandom >/tmp/s.bin
head -c 5G /dev/urandom >/tmp/n.bin

# Local disk use, every 5 s, while both files go in (in its own bash, so it stays out of the trace)
bash -c 'while sleep 5; do dfsctl store block stats --share /small -o json | jq .totals.local_disk_used; done' >/tmp/used.txt &
echo $! >/tmp/sampler.pid
smbclient //127.0.0.1/small -c 'lcd /tmp; put s.bin'
nfs-cp /tmp/n.bin "$NFS/n.bin$OPT"
kill "$(cat /tmp/sampler.pid)"
dfsctl system drain-uploads
dfsctl store block evict --share /small

# Outcomes first, then the checks: the peak within 1.25 GiB, and both cold reads byte for byte
sort -n /tmp/used.txt | tail -1 | tee /tmp/peak.txt
wc -l </tmp/used.txt
test "$(cat /tmp/peak.txt)" -le $((1024 ** 3 * 5 / 4))
smbclient //127.0.0.1/small -c 'get s.bin -' | cmp - /tmp/s.bin
nfs-cat "$NFS/n.bin$OPT" | cmp - /tmp/n.bin
