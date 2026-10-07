#!/usr/bin/env bash
# A snapshot taken while S3 is down restores the version it saw (RFC 12 §2.4, S-snap-remote-down,
# S-snap-hold-overwrite; SN-03). f is v1 and drained; with S3 stopped it is overwritten with v2, which
# only the journal holds, and snapshotted twice: "unverified" with --no-verify, then "verified" as by
# default, which waits for remote durability (recorded: done within 20 s, or still waiting). f is
# overwritten with v3, S3 starts, uploads drain and GC runs. Each snapshot that is ready is restored,
# and f must read cold as v2. --no-verify goes first: a second snapshot is refused while one is creating.
# Fails today (#2928): --no-verify also waits, past 20 s, so "verified" is never made; "unverified" becomes
# ready once S3 is back and restores v3, the content written after it was taken.

mkdir /tmp/v /tmp/back
head -c 8M /dev/urandom >/tmp/v/v1
head -c 8M /dev/urandom >/tmp/v/v2
head -c 8M /dev/urandom >/tmp/v/v3
smbclient //127.0.0.1/test -c 'lcd /tmp/v; put v1 f'
dfsctl system drain-uploads
s3 stop
smbclient //127.0.0.1/test -c 'lcd /tmp/v; put v2 f'
timeout 20 dfsctl share snapshot create /test --name unverified --no-verify && U='done' || U=waiting
timeout 20 dfsctl share snapshot create /test --name verified && V='done' || V=waiting
echo "snapshot with S3 down: --no-verify $U, default $V"
smbclient //127.0.0.1/test -c 'lcd /tmp/v; put v3 f'
s3 start
dfsctl system drain-uploads
dfsctl store block gc /test --grace-period 0 -o json
for _ in $(seq 1 30); do dfsctl share snapshot list /test -o json | jq -e 'all(.[]; .state != "creating")' >/dev/null && break; sleep 1; done
dfsctl share snapshot list /test -o json | tee /tmp/snaps.json
dfsctl share disable /test
dfsctl share snapshot restore /test "$(jq -r '.[] | select(.name == "unverified" and .state == "ready") | .id' /tmp/snaps.json)" --yes --force && R1=ok || R1=failed
dfsctl share enable /test
dfsctl store block evict --share /test
smbclient //127.0.0.1/test -c 'lcd /tmp/back; get f unverified'
VID="$(jq -r '.[] | select(.name == "verified" and .state == "ready") | .id' /tmp/snaps.json)"
if test -n "$VID"; then dfsctl share disable /test; fi
if test -n "$VID"; then dfsctl share snapshot restore /test "$VID" --yes; fi
if test -n "$VID"; then dfsctl share enable /test; fi
if test -n "$VID"; then dfsctl store block evict --share /test; fi
if test -n "$VID"; then smbclient //127.0.0.1/test -c 'lcd /tmp/back; get f verified'; fi
echo "restore: unverified $R1, verified ${VID:-not made}"
for F in /tmp/back/*; do for W in v1 v2 v3; do cmp -s "$F" /tmp/v/$W && echo "$F: $W"; done; done

for F in /tmp/back/*; do cmp "$F" /tmp/v/v2; done
