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

- **Docker** with Docker Compose V2 (`docker compose` command)
- **xmlstarlet** for TRX result parsing
  - macOS: `brew install xmlstarlet`
  - Ubuntu: `sudo apt-get install -y xmlstarlet`
  - Alpine: `apk add xmlstarlet`
- **Go 1.26+** (for E2E tests and local mode)
- **perl** for the host-wide admission lease (`compose-env.sh`), which takes an
  `flock` on a fixed path so only one conformance run is in flight per host.
  Present by default on macOS and most Linux images; `flock(1)` is not used
  because it is util-linux and absent on macOS.
  - Ubuntu: `sudo apt-get install -y perl`
  - Alpine: `apk add perl`
- **smbclient** (for smbclient E2E tests)
  - Ubuntu: `sudo apt-get install -y smbclient`
  - macOS: `brew install samba`

## Exclusivity

The harness publishes fixed host ports (445, 8080) and the suites assert on
lease and oplock breaks within a second of the request, so only one conformance
run may be in flight per host. Admission is three mechanisms, each covering a
case the others cannot:

| Mechanism | Covers |
| --- | --- |
| Host-wide lease (`compose-env.sh`, fd 9 + `flock`) | One run at a time across **every** checkout on the machine, and a killed harness whose surviving native server still holds the ports |
| Per-checkout `mkdir` claim | Two runs from the **same** checkout starting in the same instant |
| Leftover-stack scan (`docker ps -a` / volume labels) | A stack that outlived its run — left with `--keep`, or merely stopped — which holds no lease yet still owns the bootstrapped volume |

A run that cannot be admitted refuses before it creates anything, and names what
blocked it. The lease is released by the kernel when its holder exits, so a
killed run leaves nothing to clean up by hand; if a run was killed while it
still had processes alive, the refusal names `lsof` on the lease path.

Test the admission logic without Docker:

```bash
make -C test/smb-conformance test-admission
# or
./test/smb-conformance/compose-env_test.sh
```

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
```

`smbtorture/run.sh` stays for the Kerberos option, which CI still runs through it, in
`.github/workflows/conformance.yml`, until that option moves to `dt` too:

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
| `--profile PROFILE` | `memory` | Storage profile (`memory`, `badger`, `badger-s3`, `badger-s3-kerberos`) |
| `--filter FILTER` | (all) | Run specific sub-test by name |
| `--timeout SECONDS` | `1200` | Kill smbtorture after SECONDS |
| `--keep` | off | Leave containers running |
| `--dry-run` | off | Show configuration and exit |
| `--verbose` | off | Enable verbose output |

### Available Sub-Suites

**Standalone tests** (run individually, 300s budget each):

`smb2.connect`, `smb2.setinfo`, `smb2.stream-inherit-perms`, `smb2.set-sparse-ioctl`, `smb2.zero-data-ioctl`, `smb2.ioctl-on-stream`, `smb2.dosmode`, `smb2.async_dosmode`, `smb2.maxfid`, `smb2.check-sharemode`, `smb2.openattr`, `smb2.winattr`, `smb2.winattr2`, `smb2.sdread`, `smb2.secleak`, `smb2.session-id`, `smb2.tcon`, `smb2.mkdir`

**Full sub-suites** (300s budget each, except `smb2.notify` at 120s; `smb2.aio_delay` and `smb2.bench` are deliberately not run — see the comments beside them in `run.sh`):

`smb2.acls`, `smb2.acls_non_canonical`, `smb2.change_notify_disabled`, `smb2.compound`, `smb2.create`, `smb2.credits`, `smb2.delete-on-close`, `smb2.dir`, `smb2.durable-open`, `smb2.durable-v2-open`, `smb2.getinfo`, `smb2.ioctl`, `smb2.kernel-oplocks`, `smb2.lease`, `smb2.lock`, `smb2.notify`, `smb2.oplock`, `smb2.read`, `smb2.rename`, `smb2.replay`, `smb2.scan`, `smb2.session`, `smb2.streams`, `smb2.timestamps`, `smb2.twrp`, `smb2.write`

### Known Failures

`smbtorture/known.yaml` lists the known failures and expected skips `dt` grades against. `smbtorture/KNOWN_FAILURES.md`, which `parse-results.sh` reads, holds the same failures, and a test of `dt` fails while the two differ, until `run.sh` goes.

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
SMBTORTURE_AUTH=kerberos ./smbtorture/run.sh --profile badger-s3 --verbose
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
| `conformance.yml` | push, nightly | Kerberos smbtorture | ~5 min |
| `tests.yml` | PR, push, nightly | WPTS BVT and smbtorture, over two runners, on `badger-s3`, through `dt` | ~15 min |
| `smb-client-compat.yml` | push, weekly | macOS/Linux client testing | ~10 min |
| `e2e-tests.yml` | PR, push | E2E tests (NFS, SMB, cross-protocol) | ~15 min |

### Speed Tiers

- **PR checks**: WPTS BVT and smbtorture, on `badger-s3`
- **Push to develop and nightly**: the same, plus Kerberos smbtorture and the E2E tests
- **Push to develop (SMB paths) and weekly**: multi-OS client compat

### Results

- WPTS and smbtorture results: `dt`'s report in the job summary, each cell's results uploaded as artifacts
- E2E results: logged to `e2e.log` artifact
- Weekly regression failures auto-create GitHub issues

## Architecture

```
test/smb-conformance/
├── known.yaml                # WPTS's known failures and expected skips, graded by dt
├── Makefile                  # Convenience targets
├── README.md                 # This file
├── docker-compose.yml        # Service definitions for smbtorture's scripts (DittoFS, Localstack, KDC)
├── Dockerfile.dittofs        # DittoFS image with dfs + dfsctl
├── bootstrap.sh              # DittoFS provisioning (stores, shares, users, SMB adapter)
├── shares.yaml               # The same shares as data, for dt's fixture
├── configs/                  # DittoFS config files per profile
│   ├── memory.yaml
│   ├── badger.yaml
│   ├── badger-s3.yaml
│   └── badger-s3-kerberos.yaml
├── ptfconfig/                # WPTS configuration templates, which dt fills in
│   ├── CommonTestSuite.deployment.ptfconfig.template
│   └── MS-SMB2_ServerTestSuite.deployment.ptfconfig.template
├── results/                  # (gitignored) smbtorture's scripts' results per run
└── smbtorture/               # smbtorture sub-suite
    ├── run.sh                # smbtorture orchestrator
    ├── KNOWN_FAILURES.md     # smbtorture expected failures
    ├── Makefile              # Convenience targets
    ├── parse-results.sh      # smbtorture output parser
    └── baseline-results.md   # Baseline measurement data
```

## Iterating on Failures

The harness names its Compose project after the checkout it runs from, so a
bare `docker compose` command in this directory will not find the containers.
Source `compose-env.sh` first (it exports `COMPOSE_PROJECT_NAME`); the runner
prints the name as `Stack:` in its banner.

When working on fixing a specific test failure:

```bash
# 0. Address this checkout's stack
source ./compose-env.sh

# 1. Run smbtorture's scripts and keep containers alive
./smbtorture/run.sh --profile memory --keep --filter smb2.lock

# 2. Check DittoFS logs
docker compose logs -f dittofs

# 3. When done, clean up: a kept stack cannot be reused, because bootstrap
#    would run a second time against an already-bootstrapped server
docker compose down -v
```

### Debugging Tips

- **DittoFS logs:** Set `DITTOFS_LOGGING_LEVEL=DEBUG` (already set in docker-compose.yml)
- **TRX output:** WPTS's TRX file, with each test's error message, is in its `dt` cell's results directory
- **smbtorture output:** Check `results/smbtorture-<timestamp>/` for test logs
- **One stack at a time:** the published host ports are a single set and the
  suites assert on sub-second lease and oplock breaks, so the runners refuse to
  start while any stack is live — another checkout's, or one this checkout left
  behind with `--keep` — and name the directory holding it
- **ptfconfig:** `dt` fills in the templates in `ptfconfig/`. Edit them, then re-run
