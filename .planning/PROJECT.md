# DittoFS E2E Test Suite Redesign

## What This Is

A complete CLI-driven E2E test suite that validates DittoFS across all store combinations (memory, badger, postgres × memory, filesystem, s3) using both NFS and SMB protocols. Tests use `dittofsctl` CLI commands ensuring the system works correctly from a user's perspective.

## Core Value

Ensure DittoFS works correctly with the new control plane and APIs, across all metadata/payload store combinations, with both NFS and SMB protocols — while keeping tests fast, maintainable, and behavior-focused.

## Current State (v1.0 Shipped)

**Shipped:** 2026-02-03

**Test Infrastructure:**
- 30 test files, 8,884 lines of Go code
- `test/e2e/helpers/` package for CLI-driven test utilities
- `test/e2e/framework/` for mount/container helpers
- Testcontainers integration for Postgres and S3

**Test Coverage:**
- Mount/unmount CLI commands (NFS and SMB, macOS/Linux)
- Server lifecycle tests
- User/group CRUD and membership tests
- Metadata/payload store CRUD tests
- Share and permission management tests
- Adapter lifecycle with hot reload
- Backup/restore tests
- Multi-context credential isolation
- Cross-protocol interoperability (NFS ↔ SMB)
- All 9 store matrix combinations validated

**Running Tests:**
```bash
sudo go test -tags=e2e -v ./test/e2e/... -timeout 30m
```

## Requirements

### Validated

- ✓ `dittofsctl share mount` command (NFS and SMB) — v1.0
- ✓ `dittofsctl share unmount` command — v1.0
- ✓ Platform-specific mount handling (macOS/Linux) — v1.0
- ✓ Test framework with Testcontainers — v1.0
- ✓ Server lifecycle E2E tests — v1.0
- ✓ User/group CRUD tests — v1.0
- ✓ Metadata store CRUD tests (memory, badger, postgres) — v1.0
- ✓ Payload store CRUD tests (memory, filesystem, s3) — v1.0
- ✓ Share CRUD tests — v1.0
- ✓ Permission grant/revoke tests — v1.0
- ✓ Adapter lifecycle tests (NFS/SMB enable/disable) — v1.0
- ✓ Backup/restore tests — v1.0
- ✓ Multi-context management tests — v1.0
- ✓ NFS file operations (read, write, delete, mkdir) — v1.0
- ✓ SMB file operations (read, write, delete, mkdir) — v1.0
- ✓ Cross-protocol interoperability tests — v1.0
- ✓ Permission enforcement tests — v1.0
- ✓ All 9 store combinations validated — v1.0

### Active

(None — milestone complete)

### Out of Scope

- Performance benchmarks — separate suite exists
- POSIX compliance testing — separate suite exists
- Stress testing / load testing — not part of E2E validation
- Soft delete for shares — server implements hard delete (noted as known limitation)

## Key Decisions

| Decision | Rationale | Outcome |
|----------|-----------|---------|
| CLI-driven tests (dittofsctl) | Tests user-facing behavior, not internal APIs | ✓ Good — all tests use CLI |
| Go testing + testify | Standard Go practice, suite support | ✓ Good |
| Testcontainers for external deps | Self-contained tests, no pre-setup required | ✓ Good |
| Shared server model | Faster than per-test server | ✓ Good |
| All 9 store combinations | Comprehensive coverage | ✓ Good — full matrix tested |
| Test tags for categories | Enable selective test runs | ✓ Good — `-tags=e2e` works |
| SMB for permission tests | NFS AUTH_UNIX is UID-based, not useful for permission testing | ✓ Good |
| NFS only for store matrix | Cross-protocol already validated separately | ✓ Good — reduced redundancy |
| Old tests deleted | Fresh start with CLI-driven approach | ✓ Good — clean codebase |

## Constraints

- **Platform**: macOS (development) and Linux (CI/CD)
- **Privileges**: Mounting requires sudo/root access
- **External dependencies**: Postgres and S3 via Testcontainers (Docker required)
- **Framework**: Go testing + testify

---
*Last updated: 2026-02-03 after v1.0 milestone*
