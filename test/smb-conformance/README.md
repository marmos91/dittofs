# SMB Conformance Test Suite

Runs Microsoft [Windows Protocol Test Suites (WPTS)](https://github.com/microsoft/WindowsProtocolTestSuites) FileServer BVT tests against the DittoFS SMB adapter.

## Overview

This test infrastructure validates DittoFS SMB protocol conformance using Microsoft's official protocol test suite. It supports:

- **5 storage profiles** (memory, filesystem, BadgerDB, S3, PostgreSQL)
- **Docker Compose mode** (fully containerized) and **local mode** (native DittoFS + Docker WPTS)
- **Intelligent failure classification** against a known failures list
- **CI integration** via GitHub Actions with tiered matrix

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
- **Go 1.25+** (for local mode only)

## Quick Start

```bash
# Run BVT tests with memory profile (fastest)
make test

# Or directly
./run.sh --profile memory

# See configuration without running
make dry-run
```

## Storage Profiles

| Profile | Metadata Store | Payload Store | Extra Services |
|---------|---------------|---------------|----------------|
| `memory` | Memory | Memory | None |
| `memory-fs` | Memory | Filesystem | None |
| `badger-fs` | BadgerDB | Filesystem | None |
| `badger-s3` | BadgerDB | S3 | Localstack |
| `postgres-s3` | PostgreSQL | S3 | Localstack + PostgreSQL |

## Running Tests

### Compose Mode (Default)

Everything runs in Docker containers:

```bash
# Quick test with memory profile
./run.sh --profile memory

# Test with S3 backend (starts Localstack automatically)
./run.sh --profile badger-s3

# Run all profiles sequentially
make test-full
```

### Local Mode

DittoFS runs natively on the host, WPTS runs in Docker:

```bash
./run.sh --mode local --profile memory

# Or via Makefile
make local PROFILE=memory
```

Local mode builds DittoFS from source and starts it in the foreground. On macOS, WPTS connects via `host.docker.internal`; on Linux, it uses `--network host`.

## Flags Reference

| Flag | Default | Description |
|------|---------|-------------|
| `--profile PROFILE` | `memory` | Storage profile to use |
| `--mode MODE` | `compose` | Execution mode (`compose` or `local`) |
| `--filter FILTER` | `TestCategory=BVT` | WPTS test filter expression |
| `--category CAT` | - | Alias for `--filter "TestCategory=CAT"` |
| `--keep` | off | Leave containers running after tests |
| `--dry-run` | off | Show configuration and exit |
| `--verbose` | off | Enable verbose output |

Environment variables `PROFILE` and `WPTS_FILTER` can also be used.

## Understanding Results

After a test run, `parse-results.sh` produces a colored summary table:

| Status | Color | Meaning |
|--------|-------|---------|
| `PASS` | Green | Test passed |
| `KNOWN` | Yellow | Test failed, but is listed in KNOWN_FAILURES.md |
| `FAIL` | Red | Test failed, NOT in KNOWN_FAILURES.md (new failure) |
| `SKIP` | Dim | Test was not executed |

**CI exit code:** 0 if all failures are known, non-zero (count of new failures) otherwise.

## Known Failures

`KNOWN_FAILURES.md` tracks tests that are expected to fail. This is common for conformance testing where the implementation does not yet cover all protocol features.

Current categories of expected failures:
- **SMB 3.x negotiation** - DittoFS currently supports SMB 2.1 only
- **Encryption** - SMB3 encryption not yet implemented
- **Multi-channel** - Not supported
- **Durable handles v2** - Not yet implemented
- **QUIC transport** - Not supported

### Adding New Known Failures

After a test run reveals expected failures:

1. Copy the exact test name from the parse-results output
2. Add a row to the table in `KNOWN_FAILURES.md`:
   ```
   | ExactTestName | Category | Reason | #issue |
   ```
3. Re-run to confirm the failure is now classified as `KNOWN`

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
- **TRX output:** Check `results/<timestamp>/*.trx` for detailed error messages
- **Network:** WPTS shares the DittoFS network namespace (`network_mode: service:dittofs`), so `localhost` in ptfconfig resolves to DittoFS
- **ptfconfig:** Generated from templates in `ptfconfig/`. Edit templates, then re-run to regenerate

## CI Integration

The GitHub Actions workflow (`.github/workflows/smb-conformance.yml`) runs automatically:

| Trigger | Profiles | Purpose |
|---------|----------|---------|
| Pull request (SMB paths) | `memory` only | Fast feedback on PRs |
| Push to `develop` | All 5 profiles | Full conformance validation |
| Weekly cron (Monday 3 AM UTC) | All 5 profiles | Regression detection |
| Manual dispatch | Selectable | Ad-hoc testing |

**Path triggers:** The workflow only runs when files in `pkg/adapter/smb/`, `internal/adapter/smb/`, `pkg/adapter/base.go`, `test/smb-conformance/`, or the workflow file itself are modified.

Results are uploaded as GitHub Actions artifacts with 30-day retention.

## Architecture

```
test/smb-conformance/
├── run.sh                    # Main orchestrator (compose + local modes)
├── parse-results.sh          # TRX XML parser with failure classification
├── KNOWN_FAILURES.md         # Expected test failures (machine-readable)
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
└── results/                  # (gitignored) Test results per run
```

## Extending for SMB3

Phase 44 will extend this infrastructure with SMB3-specific test categories and filter configurations. The existing framework is designed to support additional WPTS test categories beyond BVT, such as:

- Model-based tests (`TestCategory=Model`)
- Scenario tests (`TestCategory=Scenario`)
- SMB3-specific encryption and signing tests
