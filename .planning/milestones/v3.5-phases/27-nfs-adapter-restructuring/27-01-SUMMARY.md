---
phase: 27-nfs-adapter-restructuring
plan: 01
subsystem: infra
tags: [go, refactoring, directory-structure, imports]

# Dependency graph
requires:
  - phase: 26-generic-lock-interface
    provides: Clean lock interface without protocol leaks
provides:
  - internal/adapter/ directory hierarchy replacing internal/protocol/
  - NLM/NSM/portmapper consolidated under internal/adapter/nfs/
  - Generic XDR at internal/adapter/nfs/xdr/core/ with package xdr preserved
  - Shared buffer pool at internal/adapter/pool/
affects: [27-02, 27-03, 27-04, all-future-adapter-work]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "internal/adapter/ as top-level namespace for all protocol internals"
    - "NFS ecosystem protocols (NLM/NSM/portmap) nested under nfs/"
    - "Generic XDR in xdr/core/ subdirectory with preserved package name"
    - "Shared pool/ utility importable by both NFS and SMB adapters"

key-files:
  created: []
  modified:
    - "internal/adapter/ (renamed from internal/protocol/)"
    - "internal/adapter/nfs/nlm/ (moved from internal/adapter/nlm/)"
    - "internal/adapter/nfs/nsm/ (moved from internal/adapter/nsm/)"
    - "internal/adapter/nfs/portmap/ (moved from internal/adapter/portmap/)"
    - "internal/adapter/nfs/xdr/core/ (moved from internal/adapter/xdr/)"
    - "internal/adapter/pool/ (moved from internal/bufpool/)"

key-decisions:
  - "Package pool renamed from bufpool to match directory name (all call sites updated)"
  - "Generic XDR uses package xdr declaration despite living in core/ directory (Go allows this)"
  - "Comments referencing old paths updated alongside import paths"

patterns-established:
  - "internal/adapter/{protocol}/ hierarchy for protocol-specific internals"
  - "internal/adapter/pool/ for shared utilities used by multiple protocol adapters"

requirements-completed: [REF-03]

# Metrics
duration: 6min
completed: 2026-02-25
---

# Phase 27 Plan 01: Directory Rename and Consolidation Summary

**Renamed internal/protocol/ to internal/adapter/, consolidated NFS ecosystem protocols under nfs/, moved generic XDR to xdr/core/, and relocated buffer pool to shared pool/**

## Performance

- **Duration:** 6 min
- **Started:** 2026-02-25T13:08:49Z
- **Completed:** 2026-02-25T13:14:52Z
- **Tasks:** 2
- **Files modified:** 454 (Task 1) + 160 (Task 2) = 614 total file changes

## Accomplishments
- Renamed entire `internal/protocol/` tree to `internal/adapter/` with all 312 Go file imports rewritten
- Consolidated NLM, NSM, and portmapper under `internal/adapter/nfs/` as NFS-specific protocols
- Moved generic XDR primitives to `internal/adapter/nfs/xdr/core/` preserving `package xdr` declaration
- Moved buffer pool to `internal/adapter/pool/` with package rename from `bufpool` to `pool`
- Full build, vet, and test suite pass with zero failures

## Task Commits

Each task was committed atomically:

1. **Task 1: Rename internal/protocol/ to internal/adapter/ and rewrite all imports** - `2c9f571e` (refactor)
2. **Task 2: Consolidate NLM/NSM/portmapper under nfs/ and move generic XDR and bufpool** - `a41ed25e` (refactor)

## Files Created/Modified
- `internal/adapter/` - All protocol implementation code (renamed from internal/protocol/)
- `internal/adapter/nfs/nlm/` - NLM protocol (moved from internal/adapter/nlm/)
- `internal/adapter/nfs/nsm/` - NSM protocol (moved from internal/adapter/nsm/)
- `internal/adapter/nfs/portmap/` - Portmapper (moved from internal/adapter/portmap/)
- `internal/adapter/nfs/xdr/core/` - Generic XDR primitives (moved from internal/adapter/xdr/)
- `internal/adapter/pool/bufpool.go` - Shared buffer pool (moved from internal/bufpool/)
- 312+ Go files with import path updates across the entire codebase

## Decisions Made
- Package `pool` renamed from `bufpool` to match directory convention (4 consumer call sites updated from `bufpool.X` to `pool.X`)
- Generic XDR keeps `package xdr` declaration despite living in `core/` directory (Go allows package name to differ from directory name, preserving all `xdr.DecodeUint32()` call sites)
- Comments referencing `internal/protocol` updated alongside import rewrites for consistency

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered
- GNU sed on this system required `-i` (not `-i ''` as macOS BSD sed). Resolved by detecting sed version and using correct syntax.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- Directory hierarchy established for all subsequent plans (27-02, 27-03, 27-04)
- All import paths now use `internal/adapter/` namespace
- Build and test infrastructure verified working with new layout

---
*Phase: 27-nfs-adapter-restructuring*
*Completed: 2026-02-25*
