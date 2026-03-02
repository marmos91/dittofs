# SMB Conformance Test Suite

Comprehensive SMB3 protocol conformance testing for the DittoFS SMB adapter. This suite validates protocol correctness using multiple complementary approaches: Microsoft WPTS, Samba smbtorture, native Go client tests, CLI-based smbclient tests, and multi-OS client compatibility.

## Overview

| Suite | Tool | Coverage | Speed |
|-------|------|----------|-------|
| **WPTS BVT** | Microsoft Protocol Test Suites | Protocol spec conformance | ~10 min |
| **smbtorture** | Samba test suite | Wire-level protocol testing | ~5 min |
| **go-smb2 E2E** | hirochachacha/go-smb2 | Native client file/dir ops | ~2 min |
| **smbclient E2E** | smbclient CLI | CLI-based protocol validation | ~2 min |
| **Cross-protocol** | NFS + SMB mounts | Lease/delegation coordination | ~5 min |
| **Kerberos** | go-smb2 + smbtorture | Kerberos auth + SMB3 features | ~10 min |
| **Multi-OS CI** | mount.cifs/mount_smbfs/net use | Windows/macOS/Linux clients | ~10 min |

## Prerequisites

- **Docker** with Docker Compose V2 (`docker compose` command)
- **xmlstarlet** for TRX result parsing
  - macOS: `brew install xmlstarlet`
  - Ubuntu: `sudo apt-get install -y xmlstarlet`
  - Alpine: `apk add xmlstarlet`
- **envsubst** for ptfconfig template rendering (usually pre-installed)
  - macOS: `brew install gettext`
  - Ubuntu: `sudo apt-get install -y gettext-base`
  - Alpine: `apk add gettext`
- **Go 1.25+** (for E2E tests and local mode)
- **smbclient** (for smbclient E2E tests)
  - Ubuntu: `sudo apt-get install -y smbclient`
  - macOS: `brew install samba`

## WPTS BVT

Runs Microsoft [Windows Protocol Test Suites (WPTS)](https://github.com/microsoft/WindowsProtocolTestSuites) FileServer BVT tests against the DittoFS SMB adapter.

### Running

```bash
# Quick test with memory profile (fastest)
./run.sh --profile memory

# With verbose output
./run.sh --profile memory --verbose

# With specific filter
./run.sh --filter "TestCategory=BVT"

# With specific test category
./run.sh --category BVT

# Run all profiles sequentially
make test-full

# See configuration without running
make dry-run
```

### Storage Profiles

| Profile | Metadata Store | Payload Store | Extra Services |
|---------|---------------|---------------|----------------|
| `memory` | Memory | Memory | None |
| `memory-fs` | Memory | Filesystem | None |
| `badger-fs` | BadgerDB | Filesystem | None |
| `badger-s3` | BadgerDB | S3 | Localstack |
| `postgres-s3` | PostgreSQL | S3 | Localstack + PostgreSQL |

### Flags Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--profile PROFILE` | `memory` | Storage profile to use |
| `--mode MODE` | `compose` | Execution mode (`compose` or `local`) |
| `--filter FILTER` | `TestCategory=BVT` | WPTS test filter expression |
| `--category CAT` | - | Alias for `--filter "TestCategory=CAT"` |
| `--keep` | off | Leave containers running after tests |
| `--dry-run` | off | Show configuration and exit |
| `--verbose` | off | Enable verbose output |

### Known Failures

`KNOWN_FAILURES.md` tracks tests that are expected to fail. Each entry is an individual test name (no wildcards). The `parse-results.sh` script reads test names from the first column of the markdown table.

**How known failures work:**
- Tests in KNOWN_FAILURES.md that fail are classified as `KNOWN` (yellow) -- not a CI failure
- Tests NOT in KNOWN_FAILURES.md that fail are classified as `FAIL` (red) -- CI fails
- Tests that pass are `PASS` (green)
- Tests not executed are `SKIP` (dim)

**Adding new known failures:**

1. Run tests and identify the failing test name from parse-results output
2. Verify the failure is due to a genuinely unimplemented feature (not a bug)
3. Add the exact test name to the table in `KNOWN_FAILURES.md`:
   ```
   | ExactTestName | Category | Reason | #issue |
   ```
4. Re-run to confirm the failure is now classified as `KNOWN`

### Understanding Results

| Status | Color | Meaning |
|--------|-------|---------|
| `PASS` | Green | Test passed |
| `KNOWN` | Yellow | Test failed, listed in KNOWN_FAILURES.md |
| `FAIL` | Red | Test failed, NOT in KNOWN_FAILURES.md (new failure) |
| `SKIP` | Dim | Test was not executed |

## smbtorture

Runs Samba's [smbtorture](https://wiki.samba.org/index.php/Smbtorture) test suite against DittoFS for wire-level SMB2/3 protocol testing.

### Running

```bash
# Run full suite with memory profile
cd smbtorture
./run.sh --profile memory --verbose

# Run specific sub-test
./run.sh --filter smb2.connect

# Run with extended timeout (default: 1200s / 20 min)
./run.sh --timeout 600

# Show configuration
./run.sh --dry-run

# Leave containers running for debugging
./run.sh --keep
```

### Flags Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--profile PROFILE` | `memory` | Storage profile (`memory`, `memory-fs`, `badger-fs`) |
| `--filter FILTER` | (all) | Run specific sub-test by name |
| `--timeout SECONDS` | `1200` | Kill smbtorture after SECONDS |
| `--keep` | off | Leave containers running |
| `--dry-run` | off | Show configuration and exit |
| `--verbose` | off | Enable verbose output |

### Available Sub-Suites

**Standalone tests** (run individually with 60s timeout each):

`smb2.connect`, `smb2.setinfo`, `smb2.stream-inherit-perms`, `smb2.set-sparse-ioctl`, `smb2.zero-data-ioctl`, `smb2.ioctl-on-stream`, `smb2.dosmode`, `smb2.async_dosmode`, `smb2.maxfid`, `smb2.check-sharemode`, `smb2.openattr`, `smb2.winattr`, `smb2.winattr2`, `smb2.sdread`, `smb2.secleak`, `smb2.session-id`, `smb2.tcon`, `smb2.mkdir`

**Full sub-suites** (run with 120s timeout each):

`smb2.acls`, `smb2.acls_non_canonical`, `smb2.aio_delay`, `smb2.bench`, `smb2.change_notify_disabled`, `smb2.compound`, `smb2.create`, `smb2.credits`, `smb2.delete-on-close`, `smb2.dir`, `smb2.durable-open`, `smb2.durable-v2-open`, `smb2.getinfo`, `smb2.ioctl`, `smb2.kernel-oplocks`, `smb2.lease`, `smb2.lock`, `smb2.notify`, `smb2.oplock`, `smb2.read`, `smb2.rename`, `smb2.replay`, `smb2.scan`, `smb2.session`, `smb2.streams`, `smb2.timestamps`, `smb2.twrp`, `smb2.write`

### Known Failures

`smbtorture/KNOWN_FAILURES.md` tracks expected failures with the same format as WPTS. Each entry is an individual test name. The `parse-results.sh` script reads these automatically.

Categories of expected smbtorture failures:
- **Multi-channel** - Not implemented
- **ACLs/Security Descriptors** - Partial implementation
- **Persistent Handles** - Not yet supported
- **Directory Leases** - Not implemented (file leases only)
- **Change Notify** - Partial implementation

## go-smb2 E2E Tests

Native Go SMB3 client tests using [hirochachacha/go-smb2](https://github.com/hirochachacha/go-smb2) library.

### Test Files

- `test/e2e/smb3_gosmb2_test.go` - 7 test functions
- `test/e2e/helpers/smb3_helpers.go` - Shared test helpers

### Running

```bash
# Run all go-smb2 tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3

# Run specific test
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_GoSMB2_BasicFileOps
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_GoSMB2_Encryption
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_GoSMB2_Signing
```

### Test Coverage

| Test | Validates |
|------|-----------|
| `TestSMB3_GoSMB2_BasicFileOps` | Create, read, write, delete via SMB3 |
| `TestSMB3_GoSMB2_DirectoryOps` | Mkdir, readdir, rmdir via SMB3 |
| `TestSMB3_GoSMB2_LargeFile` | 1MB file transfer integrity |
| `TestSMB3_GoSMB2_SessionSetup` | NTLM auth (positive + negative) |
| `TestSMB3_GoSMB2_Encryption` | Data integrity through encryption |
| `TestSMB3_GoSMB2_Signing` | Data integrity through signing |
| `TestSMB3_GoSMB2_MultipleFiles` | 50-file directory enumeration |

### Prerequisites

- go-smb2 dependency (already in go.mod)
- Running DittoFS server with SMB adapter (test helpers handle setup)

## smbclient E2E Tests

CLI-based SMB3 protocol validation using the Samba `smbclient` tool.

### Test Files

- `test/e2e/smb3_smbclient_test.go` - 4 test functions

### Running

```bash
# Run all smbclient tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_SmbClient

# Run specific test
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_SmbClient_Connect
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_SmbClient_DialectNegotiation
```

### Test Coverage

| Test | Validates |
|------|-----------|
| `TestSMB3_SmbClient_Connect` | smbclient connect + directory listing |
| `TestSMB3_SmbClient_FileOps` | put, get, del file operations |
| `TestSMB3_SmbClient_DialectNegotiation` | SMB3 dialect via debug output |
| `TestSMB3_SmbClient_DirectoryOps` | mkdir, cd, ls, rmdir |

### Prerequisites

- smbclient installed (`sudo apt-get install -y smbclient`)
- Tests skip gracefully if smbclient is not available (`IsSMBClientAvailable()` check)

## Cross-Protocol Lease Tests

Validates bidirectional lease/delegation break coordination between NFS delegations and SMB oplocks/leases.

### Test Files

- `test/e2e/cross_protocol_lease_test.go` - 7 test scenarios + concurrent conflict test

### Running

```bash
# Run all cross-protocol tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestCrossProtocol

# Run specific scenario
sudo go test -tags=e2e -v ./test/e2e/ -run TestCrossProtocol_SMBLeaseBreakOnNFSWrite
sudo go test -tags=e2e -v ./test/e2e/ -run TestCrossProtocol_ConcurrentConflicts
```

### Test Coverage

| Test | Validates |
|------|-----------|
| SMB lease break on NFS write | NFS write triggers SMB lease break |
| NFS delegation recall on SMB open | SMB open triggers NFS delegation recall |
| Directory lease breaks (3 tests) | NFS create/delete/rename break directory leases |
| Data consistency | Cross-protocol data visibility after break |
| Concurrent conflicts | 10 goroutines (5 NFS + 5 SMB) with 3 iterations |

### Prerequisites

- NFS mount capabilities (sudo, nfs-common)
- mount.cifs or go-smb2 for SMB access
- Tests skip on platforms without required tools

## Kerberos SMB3 Tests

Validates Kerberos authentication combined with SMB3 features (encryption, signing, session setup).

### Test Files

- `test/e2e/smb3_kerberos_test.go` - 7 test scenarios

### Running

```bash
# Run all Kerberos SMB3 tests
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_Kerberos

# Run specific scenario
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_Kerberos_SessionSetup
sudo go test -tags=e2e -v ./test/e2e/ -run TestSMB3_Kerberos_WithEncryption
```

### smbtorture Kerberos

smbtorture can also be run with Kerberos authentication via the CI workflow:

```bash
# In CI (via environment variable)
SMBTORTURE_AUTH=kerberos ./smbtorture/run.sh --profile memory --verbose
```

### Prerequisites

- Docker for KDC container (test helpers manage lifecycle)
- kinit, klist tools for Kerberos ticket management
- Tests skip gracefully on platforms without KDC support

## Multi-OS Client Compatibility

Validates DittoFS SMB adapter against native OS SMB clients on three platforms.

### Workflow

`.github/workflows/smb-client-compat.yml`

### Platforms

| Platform | Client | Mount Command |
|----------|--------|---------------|
| Ubuntu (latest) | mount.cifs + smbclient | `sudo mount -t cifs //localhost/share /mnt/test -o port=12445,...` |
| macOS (latest) | mount_smbfs | `mount_smbfs //user:pass@localhost:12445/share /tmp/smbtest` |
| Windows (latest) | net use | `net use Z: \\localhost\share /user:test test123` |

### When It Runs

- **Push to develop** (SMB paths changed)
- **Weekly** (Monday 4 AM UTC)
- **Manual dispatch** (`workflow_dispatch`)

NOT on pull requests (too slow for PR checks).

### What It Tests

Each platform runs the same core operations:
1. Build DittoFS from source
2. Start server and bootstrap SMB adapter via dfsctl
3. Mount SMB share
4. Create file, read file, create directory, list directory, delete
5. Verify dialect/connection info where available
6. Cleanup

## CI Integration

### Workflow Organization

| Workflow | Trigger | Purpose | Duration |
|----------|---------|---------|----------|
| `smb-conformance.yml` | PR (memory), push (all), weekly | WPTS BVT + smbtorture + Kerberos | ~20 min |
| `smb-client-compat.yml` | push, weekly | Windows/macOS/Linux client testing | ~10 min |
| `e2e-tests.yml` | PR, push | E2E tests (NFS, SMB, cross-protocol) | ~15 min |

### Speed Tiers

- **PR checks (fast, <5 min)**: Memory-only WPTS BVT, memory-only smbtorture
- **Push to develop (comprehensive, <30 min)**: All profiles WPTS BVT, all profiles smbtorture, E2E tests, multi-OS client compat
- **Weekly (full matrix, <60 min)**: Everything above + Kerberos smbtorture

### Results

- WPTS results: uploaded as GitHub Actions artifacts (30-day retention)
- smbtorture results: uploaded as artifacts with summary
- E2E results: logged to `e2e.log` artifact
- Weekly regression failures auto-create GitHub issues

## Architecture

```
test/smb-conformance/
├── run.sh                    # Main WPTS orchestrator (compose + local modes)
├── parse-results.sh          # TRX XML parser with failure classification
├── KNOWN_FAILURES.md         # WPTS expected test failures (machine-readable)
├── Makefile                  # Convenience targets
├── README.md                 # This file
├── docker-compose.yml        # Service definitions (DittoFS, WPTS, Localstack, PostgreSQL)
├── Dockerfile.dittofs        # DittoFS image with dfs + dfsctl
├── bootstrap.sh              # DittoFS provisioning (stores, shares, users, SMB adapter)
├── configs/                  # DittoFS config files per profile
│   ├── memory.yaml
│   ├── memory-fs.yaml
│   ├── badger-fs.yaml
│   ├── badger-s3.yaml
│   └── postgres-s3.yaml
├── ptfconfig/                # WPTS configuration templates
│   ├── CommonTestSuite.deployment.ptfconfig.template
│   └── MS-SMB2_ServerTestSuite.deployment.ptfconfig.template
├── ptfconfig-generated/      # (gitignored) Rendered ptfconfig files
├── results/                  # (gitignored) Test results per run
└── smbtorture/               # smbtorture sub-suite
    ├── run.sh                # smbtorture orchestrator
    ├── KNOWN_FAILURES.md     # smbtorture expected failures
    ├── Makefile              # Convenience targets
    ├── parse-results.sh      # smbtorture output parser
    └── baseline-results.md   # Baseline measurement data
```

## Iterating on Failures

When working on fixing a specific test failure:

```bash
# 1. Run tests and keep containers alive
./run.sh --profile memory --keep

# 2. Check DittoFS logs
docker compose logs -f dittofs

# 3. Inspect generated ptfconfig
cat ptfconfig-generated/MS-SMB2_ServerTestSuite.deployment.ptfconfig

# 4. Run a specific test category
./run.sh --profile memory --keep --category BVT

# 5. When done, clean up
docker compose down -v
```

### Debugging Tips

- **DittoFS logs:** Set `DITTOFS_LOGGING_LEVEL=DEBUG` (already set in docker-compose.yml)
- **TRX output:** Check `results/<timestamp>/*.trx` for detailed WPTS error messages
- **smbtorture output:** Check `results/smbtorture-<timestamp>/` for test logs
- **Network:** WPTS shares the DittoFS network namespace (`network_mode: service:dittofs`)
- **ptfconfig:** Generated from templates in `ptfconfig/`. Edit templates, then re-run
