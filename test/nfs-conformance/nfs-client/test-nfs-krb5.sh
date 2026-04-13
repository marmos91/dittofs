#!/bin/bash
# NFS Kerberos integration test client.
#
# Mounts an NFS export with sec=krb5, performs basic file operations,
# and verifies the round-trip. Exits 0 on success, non-zero on failure.
#
# Expects:
#   - /keytabs/krb5.conf   (KDC config)
#   - /keytabs/client.keytab (client keytab)
#   - DittoFS server reachable at $NFS_SERVER:$NFS_PORT

set -euo pipefail

NFS_SERVER="${NFS_SERVER:-dittofs}"
NFS_PORT="${NFS_PORT:-12049}"
NFS_EXPORT="${NFS_EXPORT:-/export}"
MOUNT_POINT="/mnt/test"
PRINCIPAL="${KRB5_PRINCIPAL:-nfs-test}"
REALM="${KRB5_REALM:-DITTOFS.TEST}"
MAX_RETRIES="${MAX_RETRIES:-30}"

log() { echo "[NFS-TEST] $*"; }
fail() { log "FAIL: $*"; exit 1; }

# Configure Kerberos
cp /keytabs/krb5.conf /etc/krb5.conf

# Wait for KDC and DittoFS to be ready
log "Waiting for DittoFS server at $NFS_SERVER:$NFS_PORT..."
for i in $(seq 1 "$MAX_RETRIES"); do
    if timeout 2 bash -c "echo > /dev/tcp/$NFS_SERVER/$NFS_PORT" 2>/dev/null; then
        log "DittoFS server is ready (attempt $i)"
        break
    fi
    if [ "$i" -eq "$MAX_RETRIES" ]; then
        fail "DittoFS server not reachable after $MAX_RETRIES attempts"
    fi
    sleep 1
done

# Obtain Kerberos ticket using password (not keytab — ktadd randomizes keys).
USER_PASSWORD="${KRB5_PASSWORD:-TestPassword01!}"
log "Authenticating as $PRINCIPAL@$REALM..."
echo "$USER_PASSWORD" | kinit "$PRINCIPAL@$REALM" \
    || fail "kinit failed"
klist || true

# Start rpc.gssd for kernel NFS Kerberos support
log "Starting rpc.gssd..."
rpcbind || true
rpc.gssd -f &
GSSD_PID=$!
sleep 1

# Mount with sec=krb5
mkdir -p "$MOUNT_POINT"
log "Mounting $NFS_SERVER:$NFS_EXPORT on $MOUNT_POINT with sec=krb5..."
mount -t nfs -o "sec=krb5,tcp,port=$NFS_PORT,mountport=$NFS_PORT,vers=3" \
    "$NFS_SERVER:$NFS_EXPORT" "$MOUNT_POINT" \
    || fail "mount failed"
log "Mount succeeded"

# Test 1: Write a file
TEST_DATA="hello-kerberos-$(date +%s)"
log "Test 1: Writing file..."
echo "$TEST_DATA" > "$MOUNT_POINT/krb5-test.txt" \
    || fail "write failed"
log "Test 1: PASS (write)"

# Test 2: Read it back
log "Test 2: Reading file..."
READ_DATA=$(cat "$MOUNT_POINT/krb5-test.txt") \
    || fail "read failed"
if [ "$READ_DATA" != "$TEST_DATA" ]; then
    fail "read mismatch: expected '$TEST_DATA', got '$READ_DATA'"
fi
log "Test 2: PASS (read round-trip)"

# Test 3: List directory
log "Test 3: Listing directory..."
ls -la "$MOUNT_POINT/" > /dev/null \
    || fail "readdir failed"
log "Test 3: PASS (readdir)"

# Test 4: Remove file
log "Test 4: Removing file..."
rm "$MOUNT_POINT/krb5-test.txt" \
    || fail "remove failed"
log "Test 4: PASS (remove)"

# Cleanup
log "Unmounting..."
umount "$MOUNT_POINT" || true
kill "$GSSD_PID" 2>/dev/null || true

log ""
log "=== All NFS Kerberos tests passed ==="
exit 0
