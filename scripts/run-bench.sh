#!/usr/bin/env bash
#
# Run the full DittoFS benchmark suite on Scaleway infrastructure.
#
# Usage:
#   ./scripts/run-bench.sh [round-name]
#
# Prerequisites:
#   - Server (51.15.211.189) and client (51.15.199.235) accessible via SSH
#   - SSH key at ~/.ssh/id_rsa (1Password agent disabled via IdentityAgent=none)
#   - DFS binary already deployed to server at /usr/local/bin/dfs
#
# What it does:
#   1. Stops DFS and cleans all data directories
#   2. Starts DFS with fresh controlplane, metadata, and block stores
#   3. Creates both filesystem and S3-backed shares
#   4. Mounts both shares on the client
#   5. Runs full benchmark suite (seq-write, seq-read, rand-write, rand-read, metadata)
#   6. Copies results locally to results/<round-name>/

set -euo pipefail

SERVER=51.15.211.189
CLIENT=51.15.199.235
SSH="ssh -o IdentityAgent=none -i ~/.ssh/id_rsa"
SCP="scp -o IdentityAgent=none -i ~/.ssh/id_rsa"

ROUND=${1:-"round-$(date +%Y%m%d-%H%M)"}
RESULTS_DIR="results/$ROUND"

DURATION=60s
THREADS=4
FILE_SIZE=1GiB
BLOCK_SIZE=4KiB

S3_REGION="${S3_REGION:-fr-par}"
S3_BUCKET="${S3_BUCKET:-dittofs-bench-blocks}"
S3_ENDPOINT="${S3_ENDPOINT:-https://s3.fr-par.scw.cloud}"
S3_ACCESS_KEY="${S3_ACCESS_KEY:?S3_ACCESS_KEY must be set}"
S3_SECRET_KEY="${S3_SECRET_KEY:?S3_SECRET_KEY must be set}"
S3_CONFIG="{\"region\":\"${S3_REGION}\",\"bucket\":\"${S3_BUCKET}\",\"endpoint\":\"${S3_ENDPOINT}\",\"access_key\":\"${S3_ACCESS_KEY}\",\"secret_key\":\"${S3_SECRET_KEY}\",\"force_path_style\":true}"

echo "=== DittoFS Benchmark Suite ==="
echo "Round: $ROUND"
echo "Duration: $DURATION | Threads: $THREADS | File Size: $FILE_SIZE"
echo ""

# 1. Stop and clean
echo "[1/7] Stopping DFS and cleaning data..."
$SSH root@$CLIENT 'umount /mnt/bench 2>/dev/null; umount /mnt/bench-s3 2>/dev/null; true'
$SSH root@$SERVER 'killall -9 dfs 2>/dev/null; sleep 1; rm -rf /root/.config/dittofs/controlplane.db /data/cache/* /data/metadata/* /data/blocks/* /export/*'

# 2. Start DFS
echo "[2/7] Starting DFS..."
$SSH root@$SERVER 'DITTOFS_CONTROLPLANE_SECRET="dittofs-bench-secret-key-for-jwt-1234567890" DITTOFS_ADMIN_INITIAL_PASSWORD="benchadmin123" nohup /usr/local/bin/dfs start --foreground > /tmp/dfs.log 2>&1 &'
sleep 4

# 3. Configure stores and shares
echo "[3/7] Configuring stores and shares..."
$SSH root@$SERVER "dfsctl login --server http://localhost:8080 --username admin --password benchadmin123 && \
  dfsctl store metadata add --name badger-meta --type badger --db-path /data/metadata/badger && \
  dfsctl store block local add --name fs-local --type fs --path /data/blocks && \
  dfsctl store block remote add --name s3-remote --type s3 --config '$S3_CONFIG' && \
  dfsctl share create --name /export --metadata badger-meta --local fs-local && \
  dfsctl share create --name /export-s3 --metadata badger-meta --local fs-local --remote s3-remote"

# 4. Mount on client
echo "[4/7] Mounting shares on client..."
$SSH root@$CLIENT "mount -t nfs -o tcp,port=12049,mountport=12049,hard,vers=3,rsize=1048576,wsize=1048576 $SERVER:/export /mnt/bench && \
  mkdir -p /mnt/bench-s3 && \
  mount -t nfs -o tcp,port=12049,mountport=12049,hard,vers=3,rsize=1048576,wsize=1048576 $SERVER:/export-s3 /mnt/bench-s3"

# 5. Run filesystem benchmark
echo "[5/7] Running filesystem benchmark..."
$SSH root@$CLIENT "dfsctl bench run /mnt/bench \
  --system dittofs-fs \
  --duration $DURATION --threads $THREADS --file-size $FILE_SIZE --block-size $BLOCK_SIZE \
  --save /tmp/dittofs-fs.json" 2>&1 | grep -E '(WORKLOAD|seq-|rand-|metadata|small-|Benchmarking)' | grep -v '%'

# 6. Run S3 benchmark
echo "[6/7] Running S3 benchmark..."
$SSH root@$CLIENT "dfsctl bench run /mnt/bench-s3 \
  --system dittofs-s3 \
  --duration $DURATION --threads $THREADS --file-size $FILE_SIZE --block-size $BLOCK_SIZE \
  --save /tmp/dittofs-s3.json" 2>&1 | grep -E '(WORKLOAD|seq-|rand-|metadata|small-|Benchmarking)' | grep -v '%'

# 7. Collect results
echo "[7/7] Collecting results..."
mkdir -p "$RESULTS_DIR"
$SCP root@$CLIENT:/tmp/dittofs-fs.json "$RESULTS_DIR/dittofs-fs.json"
$SCP root@$CLIENT:/tmp/dittofs-s3.json "$RESULTS_DIR/dittofs-s3.json"

echo ""
echo "=== Done ==="
echo "Results saved to: $RESULTS_DIR/"
echo "To compare: dfsctl bench compare $RESULTS_DIR/dittofs-fs.json $RESULTS_DIR/dittofs-s3.json"
