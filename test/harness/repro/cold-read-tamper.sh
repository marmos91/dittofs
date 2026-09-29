#!/usr/bin/env bash
# Reproduction: does a "cold read after evict" of a tampered remote block reach
# DittoFS's BLAKE3 check, and if not, which cache serves it?
#
# Mirrors step 4 of test/e2e/blocks_flip_test.go against the dt stack (compose,
# Localstack S3, NFSv3 mount at ~/mnt/dittofs-nfs), without root:
#   1. write a 16 MiB random file over NFS, wait until it is uploaded
#   2. flip 64 bytes in the middle of its new blocks/ object in S3 (length kept)
#   3. `dfsctl store block evict`, then read it back through the SAME mount
#   4. evict again, unmount + remount (drops the macOS NFS client cache), read again
# Expected with fail-closed verification: step 4 errors (EIO); step 3 errors too
# unless the client's page cache answered it without asking the server.
#
# Prereqs: dt stack up && dt stack mount
set -euo pipefail
HARNESS="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
REPO="${DITTOFS_REPO:-$(git -C "$HARNESS" rev-parse --show-toplevel)}"
export XDG_CONFIG_HOME="$HARNESS/state/dfsctl"
CTL="$REPO/dfsctl"
MNT="$HOME/mnt/dittofs-nfs"
NAME="coldtamper-$$.bin"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

s3() { (cd "$REPO" && docker compose exec -T localstack awslocal "$@"); }
keys() { s3 s3api list-objects-v2 --bucket dittofs --prefix blocks/ --query 'Contents[].Key' --output text | tr '\t' '\n' | grep . | sort || true; }
stats() { "$CTL" store block stats --share /export -o json | jq -r ".totals.$1"; }
readfile() { # prints "sha256:<16 hex> bytes=<n>" or "ERROR:<message> after <n> bytes"
    # Never hash the mount directly: `shasum` hashes whatever it managed to read
    # and exits 0 on an I/O error, so a fail-closed read looks like wrong bytes.
    # Copy first and trust cp's status; dd reports how far the read got.
    local out n
    if out="$(cp "$MNT/$NAME" "$TMP/read" 2>&1)"; then
        echo "sha256:$(shasum -a 256 "$TMP/read" | cut -c1-16) bytes=$(wc -c <"$TMP/read" | tr -d ' ')"
    else
        n="$(dd if="$MNT/$NAME" of=/dev/null bs=1m 2>&1 | awk '/bytes/{print $1; exit}')"
        echo "ERROR:${out##*: } after ${n:-?} bytes"
    fi
    rm -f "$TMP/read"
}

mount | grep -q " on $MNT " || { echo "NFS not mounted at $MNT (dt stack up && dt stack mount)" >&2; exit 2; }

keys >"$TMP/before"
head -c 16777216 /dev/urandom >"$TMP/payload"
want="sha256:$(shasum -a 256 "$TMP/payload" | cut -c1-16) bytes=$(wc -c <"$TMP/payload" | tr -d ' ')"
cp "$TMP/payload" "$MNT/$NAME"
echo "1. wrote $NAME over NFS ($want)"

for _ in $(seq 1 60); do
    [[ "$(stats unsynced_bytes)" == 0 && "$(stats pending_uploads)" == 0 ]] && break
    sleep 1
done
keys >"$TMP/after"
key="$(comm -13 "$TMP/before" "$TMP/after" | head -1)"
[[ -n "$key" ]] || { echo "no new blocks/ object appeared; upload not finished?" >&2; exit 1; }
echo "   uploaded; new remote objects: $(comm -13 "$TMP/before" "$TMP/after" | wc -l | tr -d ' '), tampering $key"

s3 s3 cp "s3://dittofs/$key" - >"$TMP/blk"
python3 - "$TMP/blk" <<'EOF'
import sys
p = sys.argv[1]; b = bytearray(open(p, 'rb').read())
s = len(b) // 2
for i in range(s, min(s + 64, len(b))): b[i] ^= 0xFF
open(p, 'wb').write(b)
EOF
s3 s3 cp - "s3://dittofs/$key" <"$TMP/blk" >/dev/null
echo "2. flipped 64 bytes mid-object (length $(wc -c <"$TMP/blk" | tr -d ' ') preserved)"

"$CTL" store block evict --share /export >/dev/null
echo "3. evicted; local_disk_used=$(stats local_disk_used) read_buffer_used=$(stats read_buffer_used)"
r3="$(readfile)"
echo "   read via SAME mount:        $r3"

"$CTL" store block evict --share /export >/dev/null
umount "$MNT"
"$HARNESS/bin/dt" stack mount >/dev/null
r4="$(readfile)"
echo "4. evicted + REMOUNTED; read: $r4"

verdict() {
    case "$1" in
        ERROR:*) echo "failed closed (verification caught the tamper)";;
        "$want") echo "ORIGINAL bytes (a cache served it; the tampered block was not verified)";;
        *) echo "WRONG bytes returned (fail-open)";;
    esac
}
echo
echo "same mount : $(verdict "$r3")"
echo "remounted  : $(verdict "$r4")"
rm -f "$MNT/$NAME" 2>/dev/null || true
