# Edge Test Suite

Validates DittoFS offline/edge resilience features on real Scaleway infrastructure.
Tests cache persistence, offline reads/writes during S3 disconnection, and automatic
sync recovery on reconnect.

## Prerequisites

- **Infrastructure deployed**: Scaleway VMs provisioned via `pulumi up` (see `infra/README.md`)
- **SSH access**: SSH key configured for root access to the server VM
- **Client VM**: Tests run from the persistent client VM (51.15.199.235)
- **NFS client**: `nfs-common` installed on client VM (`apt install nfs-common`)
- **Tools on client**: `jq`, `openssl`, `sha256sum` (pre-installed on Ubuntu Noble)
- **Tools on server**: `dig`, `iptables`, `curl`, `jq` (pre-installed by base-server.sh)

## Architecture

```
Client VM (51.15.199.235)          Server VM (pulumi-deployed)
+-----------------------+          +---------------------------+
| edge-test.sh          |  SSH     | DittoFS (NFS :12049)      |
|   - generate files    | -------> |   - BadgerDB metadata     |
|   - checksum verify   |          |   - FS local block store  |
|   - assert pass/fail  |  NFS     |   - S3 remote block store |
|   - poll health API   | <------> |   - Health API :8080      |
+-----------------------+          +---------------------------+
                                          |
                                          | S3 (blocked by iptables
                                          |      during offline test)
                                          v
                                   +------------------+
                                   | s3.fr-par.scw.cloud |
                                   | Bucket: dittofs-bench |
                                   | Prefix: dittofs-edge/ |
                                   +------------------+
```

## Step-by-Step Workflow

### 1. Deploy Infrastructure

```bash
# On your local machine
cd infra
export PULUMI_CONFIG_PASSPHRASE=""

# Deploy base (only needed once)
pulumi up --stack base

# Configure bench stack for edge testing
pulumi config set --stack bench dittofs-bench:system dittofs-edge
pulumi config set --stack bench dittofs-bench:privateNetworkID <output from base>
pulumi config set --stack bench --secret s3AccessKey <your-scw-access-key>
pulumi config set --stack bench --secret s3SecretKey <your-scw-secret-key>

# Deploy edge server
pulumi up --stack bench -y
# Note the serverIP output
```

### 2. Provision Server

SSH to the server and run the edge install script:

```bash
SERVER_IP=<from pulumi output>
scp infra/scripts/dittofs-edge.sh root@${SERVER_IP}:/tmp/
ssh root@${SERVER_IP} "S3_BUCKET=dittofs-bench S3_REGION=fr-par \
    AWS_ACCESS_KEY_ID=<key> AWS_SECRET_ACCESS_KEY=<secret> \
    DITTOFS_BRANCH=$(git branch --show-current) \
    bash /tmp/dittofs-edge.sh"
```

### 3. Run Tests from Client VM

```bash
# SSH to client VM
ssh root@51.15.199.235

# Copy test script to client
# (or scp from local: scp test/edge/edge-test.sh root@51.15.199.235:/root/)

# Run individual scenarios
SERVER_IP=<server-ip> ./edge-test.sh persist         # ~6 min
SERVER_IP=<server-ip> ./edge-test.sh offline          # ~4 min
SERVER_IP=<server-ip> ./edge-test.sh sync             # ~7 min

# Run all scenarios
SERVER_IP=<server-ip> ./edge-test.sh all              # ~17 min

# Quick test with shorter delays
SERVER_IP=<server-ip> ./edge-test.sh --delay 30 --offline-duration 30 all
```

### 4. Interpret Results

The script outputs structured logs with `[PASS]` and `[FAIL]` prefixes:

```
2026-03-20 10:30:15 === SCENARIO: Cache Persistence ===
2026-03-20 10:30:16 [PASS] Initial sync completed
2026-03-20 10:35:20 [PASS] All files readable with correct checksums after 300s delay
...
2026-03-20 10:45:30 =========================================
2026-03-20 10:45:30 RESULTS: 12 passed, 0 failed
2026-03-20 10:45:30 =========================================
```

- **Exit 0**: All assertions passed
- **Exit 1**: One or more assertions failed

### 5. Teardown

```bash
# Destroy server VM (keeps base infrastructure)
cd infra
PULUMI_CONFIG_PASSPHRASE="" pulumi destroy --stack bench -y
```

## Test Scenarios

### persist (INFRA-02)
Validates that files remain readable after a configurable delay.
1. Mounts NFS, sets retention to `pin`
2. Generates test files (10x 4KB, 5x 1MB, 2x 64MB by default)
3. Records SHA256 checksums
4. Waits for S3 sync, then waits `--delay` seconds (default 5 min)
5. Drops NFS client cache, verifies all checksums match
6. Switches retention to `ttl` (1h) and `lru`, verifies reads under each

### offline (INFRA-03)
Validates reads and writes while S3 is unreachable.
1. Pre-populates files, syncs to S3
2. Blocks S3 via iptables (named chain `DITTOFS_EDGE_TEST`)
3. Verifies health endpoint shows `degraded`
4. Reads cached files -- verifies checksums match
5. Writes new files while offline
6. Verifies directory listing works
7. Waits `--offline-duration` (default 2 min)
8. Restores S3, verifies offline-written files survived

### sync (INFRA-04)
Validates that offline writes are automatically synced on reconnect.
1. Blocks S3 via iptables
2. Writes files while offline
3. Verifies pending uploads > 0
4. Restores S3 connectivity
5. Polls health endpoint until `healthy` with `total_pending=0`
6. Verifies all files readable with correct checksums

## Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--server IP` | `$SERVER_IP` | Server VM IP address |
| `--ssh-key PATH` | `~/.ssh/id_rsa` | SSH private key |
| `--delay SECONDS` | `300` | Persistence test delay |
| `--offline-duration SECONDS` | `120` | S3 block duration |
| `--sync-timeout SECONDS` | `300` | Max sync wait |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `SERVER_IP` | (required) | Server VM IP |
| `SSH_KEY` | `~/.ssh/id_rsa` | SSH key path |
| `SSH_USER` | `root` | SSH user |
| `ADMIN_PASSWORD` | `dittofs-edge-admin-1234567890` | DittoFS admin password |
| `SMALL_COUNT` | `10` | Number of 4KB files |
| `MEDIUM_COUNT` | `5` | Number of 1MB files |
| `LARGE_COUNT` | `2` | Number of 64MB files |

## Troubleshooting

### S3 still reachable after iptables block
S3 DNS may resolve to multiple IPs. The script resolves ALL IPs via `dig +short` at
test start and blocks them all. If S3 remains reachable, check `iptables -L DITTOFS_EDGE_TEST -v`
on the server for rule hit counts.

### Health never shows 'degraded'
Health check interval is ~30s. The script waits up to 90s. Verify the health endpoint
JSON body (`.status` field), NOT the HTTP status code (which is 200 for both healthy
and degraded).

### Sync timeout after S3 restore
The syncer uses exponential backoff during outages. After restore, the health monitor
detects recovery within ~30s. Increase `--sync-timeout` if uploading many/large files.

### Stale iptables rules from previous run
The script auto-cleans stale rules at startup (pre-flight check). If still stuck,
manually run on the server:
```bash
iptables -D OUTPUT -j DITTOFS_EDGE_TEST 2>/dev/null
iptables -F DITTOFS_EDGE_TEST 2>/dev/null
iptables -X DITTOFS_EDGE_TEST 2>/dev/null
```

### NFS mount fails
Verify DittoFS is running: `ssh root@<server> "dfs status"`. Check NFS port:
`ssh root@<server> "ss -tlnp | grep 12049"`.

## Cost

Estimated infrastructure cost while VMs are running:
- PLAY2-MICRO VM (server): ~0.01 EUR/hour
- 150 GB Block Storage: ~0.01 EUR/hour
- Flexible IP: ~0.004 EUR/hour
- S3 storage: ~0.01 EUR/GB/month

**Always destroy the bench stack when not in use:**
```bash
cd infra && PULUMI_CONFIG_PASSPHRASE="" pulumi destroy --stack bench -y
```
