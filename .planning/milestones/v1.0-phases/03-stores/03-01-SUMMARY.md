---
phase: 03-stores
plan: 01
subsystem: e2e-testing
tags: [metadata-stores, cli, testing, crud]
dependency-graph:
  requires: [02-server-identity]
  provides: [metadata-store-e2e-tests, cli-metadata-store-methods]
  affects: [03-02-payload-stores]
tech-stack:
  added: []
  patterns: [functional-options, parallel-subtests, shared-server-testing]
key-files:
  created:
    - test/e2e/metadata_stores_test.go
  modified:
    - test/e2e/helpers/cli.go
decisions:
  - id: MDS-GET-VIA-LIST
    choice: GetMetadataStore implemented via list+filter (no dedicated CLI get command)
    rationale: Consistent with GetGroup pattern from Phase 2; CLI lacks store get subcommand
  - id: MDS-MINIMAL-SHARE
    choice: Added minimal CreateShare/DeleteShare helpers for store-in-use test
    rationale: Required to test deletion protection without pulling in full share management
metrics:
  duration: 2 min
  completed: 2026-02-02
---

# Phase 03 Plan 01: Metadata Stores E2E Tests Summary

MetadataStore CRUD methods added to CLIRunner with comprehensive E2E tests covering memory, BadgerDB, and PostgreSQL store types, including deletion protection when stores are in use by shares.

## Tasks Completed

| Task | Description | Commit | Files |
|------|-------------|--------|-------|
| 1 | Add MetadataStore type and CLIRunner methods | 5293070 | test/e2e/helpers/cli.go |
| 2 | Create metadata stores E2E test file | e5073f8 | test/e2e/metadata_stores_test.go |

## What Was Built

### CLIRunner MetadataStore Methods

- **MetadataStore type** - Matches API response (Name, Type, Config)
- **MetadataStoreOption** - Functional options pattern (WithMetaDBPath, WithMetaRawConfig)
- **CreateMetadataStore(name, storeType, opts...)** - Creates memory, badger, or postgres stores
- **ListMetadataStores()** - Lists all metadata stores
- **GetMetadataStore(name)** - Get by name via list+filter
- **EditMetadataStore(name, opts...)** - Edit store config
- **DeleteMetadataStore(name)** - Delete with --force

### Minimal Share Helpers (for store-in-use test)

- **CreateShare(name, metadataStore, payloadStore)** - Create share referencing stores
- **DeleteShare(name)** - Delete share with --force

### Test Coverage (TestMetadataStoresCRUD)

| Subtest | Requirement | Status |
|---------|-------------|--------|
| create memory store | MDS-01 | Covered |
| create badger store | MDS-02 | Covered |
| create postgres store | MDS-03 | Covered |
| list stores | MDS-04 | Covered |
| get store | - | Covered |
| edit badger store path | MDS-05 | Covered |
| delete store | MDS-06 | Covered |
| duplicate name rejected | - | Covered |
| cannot delete store in use | MDS-07 | Covered |

## Decisions Made

### GetMetadataStore via List+Filter

The CLI does not have a dedicated `store metadata get` command. Following the pattern established in Phase 2 for groups, GetMetadataStore lists all stores and filters by name.

### Minimal Share Helpers

Added CreateShare and DeleteShare as minimal helpers solely to support the "store in use" test. Full share management will be added in a later plan.

## Deviations from Plan

None - plan executed exactly as written.

## Verification Results

```
go build -tags=e2e ./test/e2e/helpers/... - OK
go build -tags=e2e ./test/e2e/...         - OK
MetadataStore functions:                   7
Test subtests:                             9
```

## Next Phase Readiness

Ready for 03-02 (PayloadStores). The PayloadStore CRUD methods were already added to cli.go in a prior commit (608d0d4), so the next plan may need adjustment or can proceed directly to testing.

**Note:** PayloadStore methods already exist in cli.go from commit 608d0d4. The next plan (03-02) should verify this and adjust scope accordingly.
