---
phase: 48-auto-deduced-configuration
plan: 01
subsystem: blockstore
tags: [sysinfo, auto-sizing, memory-detection, cgroup, sysctl]

# Dependency graph
requires:
  - phase: 47-l1-read-cache-and-prefetch
    provides: "ReadCacheBytes and PrefetchWorkers config fields in engine.Config"
provides:
  - "internal/sysinfo package with platform-aware memory/CPU detection"
  - "pkg/blockstore SystemDetector interface and DeduceDefaults function"
  - "Floor-enforced deduction formulas for all block store sizing parameters"
affects: [48-auto-deduced-configuration]

# Tech tracking
tech-stack:
  added: [golang.org/x/sys/unix (already in go.mod)]
  patterns: [build-tagged platform detection, interface-based dependency injection for testability, table-driven TDD]

key-files:
  created:
    - internal/sysinfo/sysinfo.go
    - internal/sysinfo/sysinfo_darwin.go
    - internal/sysinfo/sysinfo_linux.go
    - internal/sysinfo/sysinfo_windows.go
    - internal/sysinfo/sysinfo_other.go
    - internal/sysinfo/sysinfo_test.go
    - pkg/blockstore/defaults.go
    - pkg/blockstore/defaults_test.go
  modified: []

key-decisions:
  - "SystemDetector interface in pkg/blockstore mirrors sysinfo.Detector to avoid internal/ import from pkg/"
  - "PrefetchWorkers fixed at 4, not CPU-scaled, per research recommendation"
  - "formatBytes helper local to each package (sysinfo and blockstore) to avoid import dependency"

patterns-established:
  - "Build-tagged platform files: sysinfo_<os>.go with unexported availableMemory() function"
  - "Interface-based SystemDetector for mock injection in deduction tests"

requirements-completed: [AUTO-01, AUTO-02, AUTO-03, AUTO-04]

# Metrics
duration: 4min
completed: 2026-03-10
---

# Phase 48 Plan 01: System Resource Detection and Auto-Deduction Summary

**Platform-aware sysinfo detector (darwin sysctl, linux cgroup/meminfo, windows GlobalMemoryStatusEx) with DeduceDefaults deriving block store sizing from 25%/12.5% memory ratios and CPU-scaled parallelism**

## Performance

- **Duration:** 4 min
- **Started:** 2026-03-10T14:26:19Z
- **Completed:** 2026-03-10T14:30:18Z
- **Tasks:** 2
- **Files modified:** 8

## Accomplishments
- Platform-aware memory detection via build-tagged OS files (darwin, linux, windows, fallback)
- DeduceDefaults function with floor-enforced formulas: LocalStoreSize=25% mem, L1CacheSize=12.5% mem, ParallelSyncs=max(4,cpus), ParallelFetches=max(8,cpus*2)
- Comprehensive TDD test coverage: 5 sysinfo tests + 8 deduction test scenarios (normal, small, very small, large, medium, many-CPUs)

## Task Commits

Each task was committed atomically:

1. **Task 1: System resource detection package** - `d9772978` (test: RED) + `8e7a3160` (feat: GREEN)
2. **Task 2: Deduction functions** - `d07f534f` (test: RED) + `db00f957` (feat: GREEN)

_TDD tasks have two commits each (test -> feat)_

## Files Created/Modified
- `internal/sysinfo/sysinfo.go` - Detector interface, NewDetector constructor, formatBytes helper
- `internal/sysinfo/sysinfo_darwin.go` - macOS memory via unix.SysctlUint64("hw.memsize")
- `internal/sysinfo/sysinfo_linux.go` - Linux cgroup v2 then /proc/meminfo fallback
- `internal/sysinfo/sysinfo_windows.go` - Windows GlobalMemoryStatusEx syscall
- `internal/sysinfo/sysinfo_other.go` - 4 GiB fallback for unsupported platforms
- `internal/sysinfo/sysinfo_test.go` - Platform-generic tests (5 tests)
- `pkg/blockstore/defaults.go` - SystemDetector interface, DeducedDefaults struct, DeduceDefaults function
- `pkg/blockstore/defaults_test.go` - Table-driven tests with mock detector (8 test scenarios)

## Decisions Made
- SystemDetector interface lives in pkg/blockstore (not internal/) to avoid import cycle: pkg/ cannot import internal/
- PrefetchWorkers fixed at 4, not scaled with CPUs, per research finding that prefetch benefits plateau quickly
- Each package has its own formatBytes helper to avoid cross-package dependencies for a simple utility

## Deviations from Plan

None - plan executed exactly as written.

## Issues Encountered

None.

## User Setup Required

None - no external service configuration required.

## Next Phase Readiness
- sysinfo.Detector satisfies blockstore.SystemDetector via structural typing (duck typing)
- Plan 02 can call sysinfo.NewDetector() and pass it to blockstore.DeduceDefaults() in start.go
- All floor values and formulas match CONTEXT.md requirements

## Self-Check: PASSED

All 8 created files verified present. All 4 commits (d9772978, 8e7a3160, d07f534f, db00f957) verified in git log.

---
*Phase: 48-auto-deduced-configuration*
*Completed: 2026-03-10*
