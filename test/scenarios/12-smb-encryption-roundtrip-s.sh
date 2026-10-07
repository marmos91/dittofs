#!/usr/bin/env bash
# A block store with AES-256-GCM and a local key file. No plaintext may reach the bucket, a cold
# read must round-trip, also after a restart, and with a wrong passphrase the store must refuse to
# serve instead of writing unencrypted (RFC 5 §2.7, §5.1). There is no command to make a key file
# (docs/guide/encryption.md calls keyprovider.GenerateKeyFile from Go), so a few lines of Go make
# one. The encryption block goes in the --config JSON, because the --encryption-* flags are dropped
# when --config is given (#2923).
# Passes today. No marker reaches the bucket, and the cold reads match before and after a restart.
# With a wrong passphrase the server starts, but the share is not served: a tree connect gets
# NT_STATUS_BAD_NETWORK_NAME, the log says the key provider failed, and the bucket is unchanged.
P=scenario-passphrase

mkdir /tmp/keygen
cat >/tmp/keygen/main.go <<'EOF'
package main

import (
	"os"

	"github.com/marmos91/dittofs/pkg/block/middleware/encryption/keyprovider"
)

func main() {
	key, err := keyprovider.GenerateKeyFile(os.Args[1])
	if err != nil {
		panic(err)
	}
	os.Stdout.Write(key)
}
EOF
# The program joins the read-only checkout through an overlay, so it builds against its go.mod
echo '{"Replace": {"/src/cmd/scenario-keygen/main.go": "/tmp/keygen/main.go"}}' >/tmp/keygen/overlay.json
(cd /src && GOFLAGS=-buildvcs=false GOMODCACHE=/cache/go/mod GOCACHE=/cache/go/build /opt/go/usr/local/go/bin/go run -overlay /tmp/keygen/overlay.json ./cmd/scenario-keygen "$P" >/tmp/enc.key)
chmod 600 /tmp/enc.key

# The server needs the passphrase in its environment; then the store, the share and a file with a
# plaintext marker in it
dfs-server stop
dfs-server start DITTOFS_ENCRYPTION_PASSPHRASE="$P"
rclone mkdir s3:enc
dfsctl store block add --name s3-enc --type s3 --config "$(jq -c '.bucket = "enc" | .encryption = {aead: "aes-256-gcm", key: {kind: "local", file: "/tmp/enc.key"}}' /etc/dittofs-s3.json)"
dfsctl share create --name /enc --metadata md --block-store s3-enc --default-permission none
dfsctl share permission grant /enc --user tester --level read-write
awk 'BEGIN { for (i = 0; i < 150000; i++) print "PLAINTEXT-MARKER-0123456789" }' >/tmp/m.bin
head -c 16M /dev/urandom >>/tmp/m.bin
smbclient //127.0.0.1/enc -c 'lcd /tmp; put m.bin'
dfsctl system drain-uploads
dfsctl store block list -o json | jq -c '.[] | select(.name == "s3-enc")' | tee /tmp/store.json
rclone size --json s3:enc | tee /tmp/enc0.json
rclone cat s3:enc | grep -a -c PLAINTEXT-MARKER | tee /tmp/marker.count || true

# Cold reads: now, and after a restart with the same passphrase
dfsctl store block evict --share /enc
smbclient //127.0.0.1/enc -c 'get m.bin /tmp/back1.bin'
dfs-server stop
dfs-server start DITTOFS_ENCRYPTION_PASSPHRASE="$P"
dfsctl store block evict --share /enc
smbclient //127.0.0.1/enc -c 'get m.bin /tmp/back2.bin'

# A wrong passphrase: no read, no write, nothing new in the bucket
dfs-server stop
dfs-server start DITTOFS_ENCRYPTION_PASSPHRASE=wrong-passphrase >/tmp/wrong.out 2>&1 || echo "exit $?" >/tmp/wrong.rc
timeout 120 smbclient //127.0.0.1/enc -c 'get m.bin /tmp/back3.bin' >/tmp/read3.out 2>&1 || echo "exit $?" >/tmp/read3.rc
head -c 1M /dev/urandom >/tmp/w.bin
timeout 120 smbclient //127.0.0.1/enc -c 'lcd /tmp; put w.bin' >/tmp/write3.out 2>&1 || echo "exit $?" >/tmp/write3.rc
sleep 10
rclone size --json s3:enc | tee /tmp/enc1.json

# Outcomes first, then the checks
cat /tmp/wrong.out /tmp/read3.out /tmp/write3.out
cat /tmp/*.rc 2>/dev/null || true
grep -q aes-256-gcm /tmp/store.json
test "$(cat /tmp/marker.count)" -eq 0
cmp /tmp/back1.bin /tmp/m.bin
cmp /tmp/back2.bin /tmp/m.bin
test -e /tmp/read3.rc
test -e /tmp/write3.rc
test "$(jq .count /tmp/enc1.json)" -eq "$(jq .count /tmp/enc0.json)"
