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
| **Multi-OS CI** | mount.cifs/mount_smbfs | macOS/Linux clients | ~10 min |

## Prerequisites

- **Docker**, for the suites' containers and the KDC
- **Go 1.26+**, for `dt` and the E2E tests
- **smbclient** (for smbclient E2E tests)
  - Ubuntu: `sudo apt-get install -y smbclient`
  - macOS: `brew install samba`

## WPTS BVT

Runs Microsoft [Windows Protocol Test Suites (WPTS)](https://github.com/microsoft/WindowsProtocolTestSuites) FileServer BVT tests against the DittoFS SMB adapter.

### Running

The suite runs through `dt`, from the test registry (`test/suites.yaml`), and CI runs it in
`.github/workflows/tests.yml`. `dt` starts the server on the `badger-s3` profile with the
shares in `shares.yaml`, fills in the `ptfconfig/` templates, and runs WPTS's container:

```bash
test/harness/bin/dt run wpts/badger-s3    # badger-s3 needs Localstack: dt services up localstack
```

It needs Linux, Docker and port 445; on macOS, run it in the Linux container,
`test/harness/bin/dtc run wpts/badger-s3`. The filled-in configuration and the TRX file are in
the cell's results directory, under `test/harness/results/`.

### Known Failures

`known.yaml` lists the tests that are expected to fail, and the tests WPTS is expected not to
execute, each with its reason. A failure it doesn't list is a new failure, and fails the run; a
listed test that passes is an unexpected pass, and fails it too, until its entry goes.

## smbtorture

Runs Samba's [smbtorture](https://wiki.samba.org/index.php/Smbtorture) test suite against DittoFS for wire-level SMB2/3 protocol testing.

### Running

The suite runs through `dt`, from the test registry (`test/suites.yaml`), and CI runs it in
`.github/workflows/tests.yml`, its tests split over two shards:

```bash
test/harness/bin/dt run smbtorture/badger-s3            # both shards; badger-s3 needs Localstack
test/harness/bin/dt run smbtorture/badger-s3/shard-1    # one of them
test/harness/bin/dt run smbtorture/badger-s3 smb2.lock  # one line of tests.txt
test/harness/bin/dt run smbtorture --kerberos           # with Kerberos, against a KDC dt starts
```

Its tests are the lines of `smbtorture/tests.txt`, each with a limit of 5 minutes unless its
line gives its own; the comments there say why some aren't run. The Kerberos option runs
`smb2.session`, `smb2.read` and `smb2.lock` with `--use-kerberos=required`, after a merge and
nightly. A run's directory, under `test/harness/results/`, holds each cell's smbtorture output,
in `logs/`, and its server's log, `dfs.log`, in the cell's own directory.

### Known Failures

`smbtorture/known.yaml` lists the known failures and expected skips `dt` grades against, and
`smbtorture/known-kerberos.yaml` those of the Kerberos option.

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

smbtorture's Kerberos option runs through `dt`, which starts a KDC from `kdc/` for it:

```bash
test/harness/bin/dt run smbtorture --kerberos
```

### Prerequisites

- Docker for KDC container (test helpers manage lifecycle)
- kinit, klist tools for Kerberos ticket management
- Tests skip gracefully on platforms without KDC support

## Multi-OS Client Compatibility

Validates DittoFS SMB adapter against native OS SMB clients on two platforms.

### Workflow

`.github/workflows/smb-client-compat.yml`

### Platforms

| Platform | Client | Mount Command | Profile |
|----------|--------|---------------|---------|
| Ubuntu (latest) | mount.cifs + smbclient | `sudo mount -t cifs //localhost/share /mnt/test -o port=12445,...` | `badger-s3` (Localstack) |
| macOS (latest) | mount_smbfs | `mount_smbfs //user:pass@localhost:12445/share /tmp/smbtest` | `memory`: the runner has no Docker for Localstack |

### When It Runs

- **Push to develop** (SMB paths changed)
- **Weekly** (Sunday 21:55 UTC)
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
| `tests.yml` | PR, push, nightly | WPTS BVT and smbtorture, over two runners, on `badger-s3`, through `dt`, and smbtorture's Kerberos option after a merge and nightly | ~15 min |
| `smb-client-compat.yml` | push, weekly | macOS/Linux client testing | ~10 min |

### Speed Tiers

- **PR checks**: WPTS BVT and smbtorture, on `badger-s3`
- **Push to develop and nightly**: the same, plus Kerberos smbtorture and the e2e suite (in `tests.yml`)
- **Push to develop (SMB paths) and weekly**: multi-OS client compat

### Results

- WPTS and smbtorture results: `dt`'s report in the job summary, each cell's results uploaded as artifacts
- E2E results: the same, from the e2e cells in `tests.yml`
- Weekly regression failures auto-create GitHub issues

## Architecture

```
test/smb-conformance/
├── known.yaml                # WPTS's known failures and expected skips, graded by dt
├── README.md                 # This file
├── shares.yaml               # The shares WPTS and smbtorture run on, for dt's fixture
├── kdc/                      # The KDC image dt starts for smbtorture's Kerberos option
├── ptfconfig/                # WPTS configuration templates, which dt fills in
│   ├── CommonTestSuite.deployment.ptfconfig.template
│   └── MS-SMB2_ServerTestSuite.deployment.ptfconfig.template
└── smbtorture/
    ├── tests.txt             # smbtorture's test list, for dt
    ├── known.yaml            # its known failures and expected skips, graded by dt
    ├── known-kerberos.yaml   # the Kerberos option's
    └── baseline-results.md   # a snapshot of its results, written by dt known baseline
```

## Iterating on Failures

When working on fixing a specific test failure, run its line of `tests.txt` alone, then read
the cell's report:

```bash
test/harness/bin/dt run smbtorture/badger-s3 smb2.lock
test/harness/bin/dt report        # the latest run: each cell's new and known failures
```

### Debugging Tips

- **DittoFS logs:** `dfs.log`, in the `dt` cell's directory of the run
- **TRX output:** WPTS's TRX file, with each test's error message, is in its `dt` cell's results directory
- **smbtorture output:** the run's `logs/`, one log per cell
- **ptfconfig:** `dt` fills in the templates in `ptfconfig/`. Edit them, then re-run
