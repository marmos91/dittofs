#!/usr/bin/env bash
# Block-store options are kept or refused, never silently dropped, clamped or replaced by a default
# (RFC 13 §6: "Unknown fields are refused"). A setting bound to stored content cannot change under
# it (RFC 13 §5.1).
#   1. With --config, each of --compression, --parallel-uploads and --encryption-* must show up in
#      the stored config, or the add must fail and name the flag (#2923).
#   2. Bad values in the config JSON must be refused: an unknown compression, an empty one, a
#      negative parallel_uploads, and a misspelt key ("compresion", which would leave it off).
#   3. Once a share has data on a store, changing the store's bucket or prefix must be refused, and
#      the data must still read back after a restart, when a stored config change takes effect.
# Fails today, at all three:
#   1. each flag is dropped, and the store is created without it;
#   2. the bad values are refused, naming the field, but "compresion" is accepted and ignored;
#   3. both edits are accepted. After the restart the share looks for its blocks under the new
#      prefix: the read fails with NT_STATUS_UNEXPECTED_IO_ERROR, and the server logs "CAS object
#      missing for live FileChunk ... possible GC race or live-data-loss".
S3="$(jq -c '.bucket = "opts"' /etc/dittofs-s3.json)"
rclone mkdir s3:opts
rclone mkdir s3:other

# 1. Flags next to --config. Every store gets its own prefix, so none is refused as a duplicate
dfsctl store block add --name s3-o1 --type s3 --config "$(jq -c '.prefix = "o1/"' <<<"$S3")" --compression zstd >/tmp/o1.out 2>&1 || echo "exit $?" >/tmp/o1.rc
dfsctl store block add --name s3-o2 --type s3 --config "$(jq -c '.prefix = "o2/"' <<<"$S3")" --parallel-uploads 4 >/tmp/o2.out 2>&1 || echo "exit $?" >/tmp/o2.rc
dfsctl store block add --name s3-o3 --type s3 --config "$(jq -c '.prefix = "o3/"' <<<"$S3")" --encryption-aead aes-256-gcm --encryption-key-kind local --encryption-key-file /tmp/none.key >/tmp/o3.out 2>&1 || echo "exit $?" >/tmp/o3.rc
dfsctl store block list -o json >/tmp/list.json

# 2. Bad values in the JSON
dfsctl store block add --name s3-b1 --type s3 --config "$(jq -c '.prefix = "b1/" | .compression = {algo: "bogus"}' <<<"$S3")" >/tmp/b1.out 2>&1 || echo "exit $?" >/tmp/b1.rc
dfsctl store block add --name s3-b2 --type s3 --config "$(jq -c '.prefix = "b2/" | .compression = {algo: ""}' <<<"$S3")" >/tmp/b2.out 2>&1 || echo "exit $?" >/tmp/b2.rc
dfsctl store block add --name s3-b3 --type s3 --config "$(jq -c '.prefix = "b3/" | .parallel_uploads = -1' <<<"$S3")" >/tmp/b3.out 2>&1 || echo "exit $?" >/tmp/b3.rc
dfsctl store block add --name s3-b4 --type s3 --config "$(jq -c '.prefix = "b4/" | .compresion = {algo: "zstd"}' <<<"$S3")" >/tmp/b4.out 2>&1 || echo "exit $?" >/tmp/b4.rc

# 3. A store with content: its bucket and its prefix must not change
dfsctl store block add --name s3-used --type s3 --config "$(jq -c '.prefix = "used/"' <<<"$S3")"
dfsctl share create --name /used --metadata md --block-store s3-used --default-permission none
dfsctl share permission grant /used --user tester --level read-write
head -c 8M /dev/urandom >/tmp/u.bin
smbclient //127.0.0.1/used -c 'lcd /tmp; put u.bin'
dfsctl system drain-uploads
dfsctl store block edit s3-used --bucket other >/tmp/e1.out 2>&1 || echo "exit $?" >/tmp/e1.rc
dfsctl store block edit s3-used --config "$(jq -c '.prefix = "moved/"' <<<"$S3")" >/tmp/e2.out 2>&1 || echo "exit $?" >/tmp/e2.rc
dfsctl store block list -o json | jq -c '.[] | select(.name == "s3-used") | .config | {bucket, prefix}' | tee /tmp/used.json
dfs-server stop
dfs-server start
dfsctl store block evict --share /used
timeout 120 smbclient //127.0.0.1/used -c 'get u.bin /tmp/back.bin' || true

# Outcomes first, then the checks: each flag kept, or the refusal naming it
for f in o1 o2 o3 b1 b2 b3 b4 e1 e2; do echo "== $f $(cat "/tmp/$f.rc" 2>/dev/null || echo accepted)"; cat "/tmp/$f.out"; done
jq -c '.[] | select(.name | startswith("s3-o")) | {name, config: (.config | {compression, parallel_uploads, encryption})}' /tmp/list.json
jq -e '.[] | select(.name == "s3-o1") | .config.compression.algo == "zstd"' /tmp/list.json || grep -q -- --compression /tmp/o1.out
jq -e '.[] | select(.name == "s3-o2") | .config.parallel_uploads == 4' /tmp/list.json || grep -q -- --parallel-uploads /tmp/o2.out
jq -e '.[] | select(.name == "s3-o3") | .config.encryption.aead == "aes-256-gcm"' /tmp/list.json || grep -q -- --encryption /tmp/o3.out
test -e /tmp/b1.rc
test -e /tmp/b2.rc
test -e /tmp/b3.rc
test -e /tmp/b4.rc
test -e /tmp/e1.rc
test -e /tmp/e2.rc
test "$(jq -c . /tmp/used.json)" = '{"bucket":"opts","prefix":"used/"}'
cmp /tmp/back.bin /tmp/u.bin
