# Phase 48: Auto-Deduced Configuration - Research

**Researched:** 2026-03-10
**Domain:** System resource detection, auto-configuration, Go build tags, cross-platform memory detection
**Confidence:** HIGH

## Summary

Phase 48 replaces static hardcoded defaults for block store sizing and concurrency with values derived from actual system resources (memory, CPU). The implementation touches three layers: (1) a new `internal/sysinfo/` package for OS-level resource detection with build-tagged files, (2) deduction functions in `pkg/blockstore/defaults.go` with a testable `SystemDetector` interface, and (3) config/CLI integration that removes the old `cache` and `offloader` config sections and wires auto-deduced values into the existing `LocalStoreDefaults` / `SyncerDefaults` plumbing.

The Go runtime (Go 1.25, which this project uses) already handles cgroup-aware CPU detection via `runtime.GOMAXPROCS(0)`, so CPU detection is trivial. Memory detection requires build-tagged implementations: cgroup v2 `memory.max` with `/proc/meminfo` fallback on Linux, `golang.org/x/sys/unix.SysctlUint64("hw.memsize")` on macOS, and `GlobalMemoryStatusEx` via syscall on Windows. The project already depends on `golang.org/x/sys v0.38.0` and has established patterns for build-tagged files in `internal/logger/`.

**Primary recommendation:** Implement the three layers bottom-up (sysinfo -> blockstore/defaults -> config+CLI integration), remove old CacheConfig/OffloaderConfig from the Config struct, and wire deduced values through the existing `SetLocalStoreDefaults`/`SetSyncerDefaults` runtime methods.

<user_constraints>
## User Constraints (from CONTEXT.md)

### Locked Decisions
- Config field naming follows v4.0 block store terminology: `LocalStoreSize` (25% mem), `L1CacheSize` (12.5% mem), `ParallelSyncs` (max(4, GOMAXPROCS)), `ParallelFetches` (max(8, GOMAXPROCS*2))
- `MaxPendingSize` auto-deduced as 50% of LocalStoreSize
- `PrefetchBlocks` removed as static config -- replaced with dynamic prefetch bounded by capacity
- No global config section -- old `cache` and `offloader` YAML sections removed entirely
- Auto-deduced values computed once at startup, used as per-share defaults
- Per-share overrides via flat optional fields on share create/edit API
- Zero value on a share field means "use auto-deduced default"
- Cgroup-aware memory detection: cgroup v2 `memory.max` first, then OS-level fallback
- CPU detection via `runtime.GOMAXPROCS(0)` directly (already cgroup-aware in Go 1.25)
- New `internal/sysinfo/` package with build-tagged OS files
- Deduction logic in `pkg/blockstore/defaults.go`
- `SystemDetector` interface with `AvailableMemory()` and `AvailableCPUs()` for testability
- Floor values: LocalStoreSize >= 256MiB, L1CacheSize >= 64MiB, ParallelSyncs >= 4, ParallelFetches >= 8
- Per-share budget: 25% of memory is DEFAULT per-share LocalStoreSize (multiple shares can each get 25%)
- INFO log at startup with deduced values; WARN log when floor values hit
- Per-share INFO log showing which values are custom vs auto-deduced
- CLI: `dfs config show --deduced` flag
- Config template update: remove old cache/offloader sections, add auto-deduction comments
- Testing via injected SystemDetector interface (normal, small, large machines, user overrides, cgroup containers)

### Claude's Discretion
- Exact SystemDetector interface method signatures and return types
- Internal implementation of cgroup v2 memory.max parsing
- Build tag syntax and file organization within internal/sysinfo/
- Error message wording for detection failures
- Exact YAML comment format in `--deduced` output
- Test helper structure and assertion patterns

### Deferred Ideas (OUT OF SCOPE)
- Disk space detection for LocalStoreSize
- Dynamic re-detection if cgroup limits change mid-run
- Server-wide memory budget split among shares
- Property-based testing for deduction logic
- Documentation updates (ARCHITECTURE.md, CONFIGURATION.md, CLAUDE.md) -- Phase 49
</user_constraints>

<phase_requirements>
## Phase Requirements

| ID | Description | Research Support |
|----|-------------|-----------------|
| AUTO-01 | WriteBufferMemory derived from 25% of available memory | Becomes `LocalStoreSize` via `DeduceLocalStoreSize(mem uint64) uint64` in `pkg/blockstore/defaults.go`; memory detected by `internal/sysinfo/` package |
| AUTO-02 | ReadCacheMemory derived from 12.5% of available memory | Becomes `L1CacheSize` via `DeduceL1CacheSize(mem uint64) int64` in `pkg/blockstore/defaults.go`; feeds into `LocalStoreDefaults.ReadCacheBytes` |
| AUTO-03 | ParallelUploads derived from max(4, cpus) | Becomes `ParallelSyncs` via `DeduceParallelSyncs(cpus int) int` in `pkg/blockstore/defaults.go`; uses `runtime.GOMAXPROCS(0)` (cgroup-aware in Go 1.25) |
| AUTO-04 | ParallelDownloads derived from max(8, cpus*2) | Becomes `ParallelFetches` via `DeduceParallelFetches(cpus int) int` in `pkg/blockstore/defaults.go`; uses `runtime.GOMAXPROCS(0)` |
| AUTO-05 | User config overrides auto-deduced defaults | Per-share overrides via share create/edit API fields; zero value = use auto-deduced default; existing `ApplyDefaults` pattern preserved |
</phase_requirements>

## Standard Stack

### Core
| Library | Version | Purpose | Why Standard |
|---------|---------|---------|--------------|
| `golang.org/x/sys/unix` | v0.38.0 (already dep) | macOS `SysctlUint64("hw.memsize")` | Standard Go extended syscall library; avoids raw unsafe syscall |
| `runtime` (stdlib) | Go 1.25 | `GOMAXPROCS(0)` for cgroup-aware CPU count | Built-in, cgroup-aware since Go 1.25 |
| `os` (stdlib) | Go 1.25 | Read `/proc/meminfo`, cgroup files | Standard file I/O |
| `internal/bytesize` | (existing) | Human-readable byte size formatting/parsing | Already used throughout config layer |

### Supporting
| Library | Version | Purpose | When to Use |
|---------|---------|---------|-------------|
| `github.com/spf13/cobra` | (existing dep) | `--deduced` flag on config show | CLI framework already used by project |

### Alternatives Considered
| Instead of | Could Use | Tradeoff |
|------------|-----------|----------|
| Manual sysinfo | `github.com/shirou/gopsutil` | Heavyweight dependency; we only need memory detection, not full system stats |
| Manual cgroup parsing | `github.com/containerd/cgroups` | Too heavy; we just read one file |
| `golang.org/x/sys/unix.SysctlUint64` | `syscall.Sysctl` | `syscall.Sysctl` returns string, truncates 64-bit values on darwin -- `x/sys` is correct |

**Installation:**
No new dependencies needed. `golang.org/x/sys` is already in `go.mod`.

## Architecture Patterns

### Recommended Project Structure
```
internal/sysinfo/
├── sysinfo.go             # SystemDetector interface, DefaultDetector constructor
├── sysinfo_linux.go       # //go:build linux -- cgroup v2 + /proc/meminfo
├── sysinfo_darwin.go      # //go:build darwin -- sysctl hw.memsize
├── sysinfo_windows.go     # //go:build windows -- GlobalMemoryStatusEx
├── sysinfo_other.go       # //go:build !linux && !darwin && !windows -- safe fallback
├── sysinfo_test.go        # Cross-platform unit tests (interface tests)
├── sysinfo_linux_test.go  # //go:build linux -- Linux-specific parsing tests
└── sysinfo_darwin_test.go # //go:build darwin -- macOS-specific tests

pkg/blockstore/
├── defaults.go            # Deduction functions + SystemDetector interface
└── defaults_test.go       # Table-driven tests with mock detector

cmd/dfs/commands/
├── start.go               # Modified: use deduced values instead of cfg.Cache/Offloader
└── config/
    └── show.go            # Modified: add --deduced flag
```

### Pattern 1: Build-Tagged OS Files
**What:** Separate per-OS implementations behind build constraints
**When to use:** OS-specific syscalls that have no cross-platform abstraction
**Example:**
```go
// internal/sysinfo/sysinfo_darwin.go
//go:build darwin

package sysinfo

import "golang.org/x/sys/unix"

func availableMemory() (uint64, string, error) {
    mem, err := unix.SysctlUint64("hw.memsize")
    if err != nil {
        return 0, "", fmt.Errorf("sysctl hw.memsize: %w", err)
    }
    return mem, "sysctl hw.memsize", nil
}
```
This follows the exact pattern established in `internal/logger/terminal.go` (darwin), `terminal_linux.go`, `terminal_windows.go`.

### Pattern 2: Interface-Based Testability
**What:** Define a `SystemDetector` interface so tests can inject controlled values
**When to use:** When system-dependent behavior needs deterministic testing
**Example:**
```go
// pkg/blockstore/defaults.go
type SystemDetector interface {
    AvailableMemory() uint64
    AvailableCPUs() int
}

type DeducedDefaults struct {
    LocalStoreSize  uint64
    L1CacheSize     int64
    MaxPendingSize  uint64
    ParallelSyncs   int
    ParallelFetches int
    PrefetchWorkers int  // derived from ParallelFetches or fixed
}

func DeduceDefaults(d SystemDetector) *DeducedDefaults {
    mem := d.AvailableMemory()
    cpus := d.AvailableCPUs()

    localStoreSize := max(mem/4, 256*1024*1024) // 25% of mem, floor 256MiB
    l1CacheSize := max(int64(mem/8), 64*1024*1024)  // 12.5% of mem, floor 64MiB
    // ... etc
    return &DeducedDefaults{...}
}
```

### Pattern 3: Zero-Value Means "Use Default"
**What:** Config fields use zero-value to indicate "not explicitly set"
**When to use:** When user-provided values should override auto-deduced values
**Example:**
```go
// In start.go, after deduction:
deduced := blockstore.DeduceDefaults(detector)

rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{
    MaxSize:        deduced.LocalStoreSize,
    MaxPendingSize: deduced.MaxPendingSize,
    ReadCacheBytes: deduced.L1CacheSize,
})
rt.SetSyncerDefaults(&shares.SyncerDefaults{
    ParallelUploads:   deduced.ParallelSyncs,
    ParallelDownloads: deduced.ParallelFetches,
    PrefetchWorkers:   deduced.PrefetchWorkers,
})
```

### Anti-Patterns to Avoid
- **Global mutable state for detection results:** Use explicit parameter passing, not package-level vars. The detector is created once, passed through.
- **Reading cgroup files on macOS/Windows:** Build tags must prevent cgroup code from compiling on non-Linux platforms.
- **Parsing `/proc/meminfo` on non-Linux:** This path only exists on Linux. Use build tags.
- **Using `syscall.Sysctl("hw.memsize")` on macOS:** Returns a string, truncates 64-bit values. Must use `golang.org/x/sys/unix.SysctlUint64`.
- **Calling `runtime.NumCPU()` instead of `runtime.GOMAXPROCS(0)`:** `NumCPU()` returns physical cores, not the cgroup-adjusted value. Go 1.25 makes `GOMAXPROCS(0)` the correct choice.

## Don't Hand-Roll

| Problem | Don't Build | Use Instead | Why |
|---------|-------------|-------------|-----|
| macOS memory detection | Raw `syscall.Syscall6` with sysctl MIB | `golang.org/x/sys/unix.SysctlUint64("hw.memsize")` | Handles 64-bit value correctly; `syscall.Sysctl` truncates |
| CPU count in containers | Custom cgroup CPU quota parsing | `runtime.GOMAXPROCS(0)` | Go 1.25 is already cgroup-aware for CPUs |
| Byte size formatting | Manual string formatting | `internal/bytesize.ByteSize.String()` | Already handles GiB/MiB/KiB formatting |

**Key insight:** CPU detection is already solved by Go 1.25's runtime. Memory detection is the only OS-specific code needed. Keep the OS layer thin (just detect memory) and put all deduction logic in platform-independent code.

## Common Pitfalls

### Pitfall 1: cgroup v2 `memory.max` Contains "max" (Unlimited)
**What goes wrong:** Parsing fails or returns 0 when container has no memory limit
**Why it happens:** When no memory limit is set, cgroup v2 writes the literal string "max" to `memory.max`
**How to avoid:** Check for "max" string before parsing as uint64. If "max", fall back to `/proc/meminfo` (MemTotal)
**Warning signs:** Deduced values are unreasonably small (floor values) in unlimited containers

### Pitfall 2: `/proc/meminfo` MemTotal is in kB
**What goes wrong:** Memory off by 1000x
**Why it happens:** `/proc/meminfo` reports "MemTotal: 16384512 kB" -- value is in kilobytes
**How to avoid:** Multiply parsed value by 1024 (kB -> bytes)
**Warning signs:** LocalStoreSize computed as ~4KiB instead of ~4GiB

### Pitfall 3: macOS `syscall.Sysctl` Truncates 64-bit Values
**What goes wrong:** `syscall.Sysctl("hw.memsize")` returns a truncated string for 64-bit integer values
**Why it happens:** `syscall.Sysctl` strips the last byte. This is a known Go issue (#21614)
**How to avoid:** Use `golang.org/x/sys/unix.SysctlUint64("hw.memsize")` which correctly reads the raw bytes
**Warning signs:** Memory reported as 0 or negative on macOS

### Pitfall 4: Windows Syscall `GlobalMemoryStatusEx` Requires Struct Size Field
**What goes wrong:** Syscall returns error (ERROR_INVALID_PARAMETER)
**Why it happens:** `GlobalMemoryStatusEx` requires `dwLength` field to be pre-set to struct size
**How to avoid:** Set `memStatus.dwLength = uint32(unsafe.Sizeof(memStatus))` before the call
**Warning signs:** Memory detection fails on Windows with "invalid parameter" error

### Pitfall 5: Config Struct Removal Breaking Existing Configs
**What goes wrong:** Users with existing `config.yaml` containing `cache:` and `offloader:` sections get parse errors
**Why it happens:** Removing CacheConfig/OffloaderConfig from Config struct makes Viper reject unknown keys
**How to avoid:** Since the CONTEXT.md says "breaking change, acceptable for experimental project", ensure clear error message. Alternatively, keep the struct fields but mark them as deprecated/ignored, or use Viper's `SetDefault` behavior
**Warning signs:** Existing installations fail on upgrade

### Pitfall 6: Multiple Floor Values Competing for Memory
**What goes wrong:** On a 512MiB machine, LocalStoreSize floor (256MiB) + L1CacheSize floor (64MiB) = 320MiB > 62.5% of memory
**Why it happens:** Floor values are independent, not coordinated
**How to avoid:** Log WARN when total deduced memory exceeds 50% of available memory. This is per-share, so a single share is fine; warn that multi-share setups need tuning
**Warning signs:** OOM on very small machines with multiple shares

## Code Examples

### Linux cgroup v2 Memory Detection
```go
// internal/sysinfo/sysinfo_linux.go
//go:build linux

package sysinfo

import (
    "errors"
    "fmt"
    "os"
    "strconv"
    "strings"
)

func availableMemory() (uint64, string, error) {
    // Try cgroup v2 first
    mem, err := readCgroupV2MemoryMax()
    if err == nil && mem > 0 {
        return mem, "cgroup v2 memory.max", nil
    }

    // Fallback to /proc/meminfo
    mem, err = readProcMeminfo()
    if err != nil {
        return 0, "", fmt.Errorf("failed to detect memory: cgroup=%v, meminfo=%w", err, err)
    }
    return mem, "/proc/meminfo", nil
}

func readCgroupV2MemoryMax() (uint64, error) {
    data, err := os.ReadFile("/sys/fs/cgroup/memory.max")
    if err != nil {
        return 0, err
    }
    s := strings.TrimSpace(string(data))
    if s == "max" {
        return 0, errors.New("cgroup memory.max is unlimited")
    }
    return strconv.ParseUint(s, 10, 64)
}

func readProcMeminfo() (uint64, error) {
    data, err := os.ReadFile("/proc/meminfo")
    if err != nil {
        return 0, err
    }
    for _, line := range strings.Split(string(data), "\n") {
        if strings.HasPrefix(line, "MemTotal:") {
            fields := strings.Fields(line)
            if len(fields) < 2 {
                continue
            }
            kb, err := strconv.ParseUint(fields[1], 10, 64)
            if err != nil {
                return 0, err
            }
            return kb * 1024, nil // kB -> bytes
        }
    }
    return 0, errors.New("MemTotal not found in /proc/meminfo")
}
```

### macOS Memory Detection
```go
// internal/sysinfo/sysinfo_darwin.go
//go:build darwin

package sysinfo

import (
    "fmt"
    "golang.org/x/sys/unix"
)

func availableMemory() (uint64, string, error) {
    mem, err := unix.SysctlUint64("hw.memsize")
    if err != nil {
        return 0, "", fmt.Errorf("sysctl hw.memsize: %w", err)
    }
    return mem, "sysctl hw.memsize", nil
}
```

### Windows Memory Detection
```go
// internal/sysinfo/sysinfo_windows.go
//go:build windows

package sysinfo

import (
    "fmt"
    "syscall"
    "unsafe"
)

type memoryStatusEx struct {
    dwLength                uint32
    dwMemoryLoad            uint32
    ullTotalPhys            uint64
    ullAvailPhys            uint64
    ullTotalPageFile         uint64
    ullAvailPageFile         uint64
    ullTotalVirtual         uint64
    ullAvailVirtual         uint64
    ullAvailExtendedVirtual uint64
}

var (
    kernel32                  = syscall.NewLazyDLL("kernel32.dll")
    procGlobalMemoryStatusEx  = kernel32.NewProc("GlobalMemoryStatusEx")
)

func availableMemory() (uint64, string, error) {
    var ms memoryStatusEx
    ms.dwLength = uint32(unsafe.Sizeof(ms))
    r, _, err := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&ms)))
    if r == 0 {
        return 0, "", fmt.Errorf("GlobalMemoryStatusEx: %w", err)
    }
    return ms.ullTotalPhys, "GlobalMemoryStatusEx", nil
}
```

### Deduction Logic
```go
// pkg/blockstore/defaults.go
package blockstore

import "runtime"

const (
    MinLocalStoreSize  = 256 << 20 // 256 MiB
    MinL1CacheSize     = 64 << 20  // 64 MiB
    MinParallelSyncs   = 4
    MinParallelFetches = 8
)

type SystemDetector interface {
    AvailableMemory() uint64
    AvailableCPUs() int
}

type DeducedDefaults struct {
    LocalStoreSize  uint64
    L1CacheSize     int64
    MaxPendingSize  uint64
    ParallelSyncs   int
    ParallelFetches int
}

func DeduceDefaults(d SystemDetector) *DeducedDefaults {
    mem := d.AvailableMemory()
    cpus := d.AvailableCPUs()

    localStoreSize := max(mem/4, MinLocalStoreSize)
    l1CacheSize := max(int64(mem/8), MinL1CacheSize)
    maxPendingSize := localStoreSize / 2 // 50% of LocalStoreSize
    parallelSyncs := max(MinParallelSyncs, cpus)
    parallelFetches := max(MinParallelFetches, cpus*2)

    return &DeducedDefaults{
        LocalStoreSize:  localStoreSize,
        L1CacheSize:     l1CacheSize,
        MaxPendingSize:  maxPendingSize,
        ParallelSyncs:   parallelSyncs,
        ParallelFetches: parallelFetches,
    }
}
```

### Start.go Integration
```go
// In cmd/dfs/commands/start.go, replacing current cfg.Cache/cfg.Offloader usage:

import (
    "github.com/marmos91/dittofs/internal/sysinfo"
    "github.com/marmos91/dittofs/pkg/blockstore"
)

// Detect system resources and compute defaults
detector := sysinfo.NewDetector()
deduced := blockstore.DeduceDefaults(detector)

logger.Info("Auto-deduced defaults",
    "local_store_size", bytesize.ByteSize(deduced.LocalStoreSize),
    "l1_cache_size", bytesize.ByteSize(deduced.L1CacheSize),
    "max_pending_size", bytesize.ByteSize(deduced.MaxPendingSize),
    "parallel_syncs", deduced.ParallelSyncs,
    "parallel_fetches", deduced.ParallelFetches,
    "memory", bytesize.ByteSize(detector.AvailableMemory()),
    "cpus", detector.AvailableCPUs(),
)

rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{
    MaxSize:        deduced.LocalStoreSize,
    MaxPendingSize: deduced.MaxPendingSize,
    ReadCacheBytes: deduced.L1CacheSize,
})
rt.SetSyncerDefaults(&shares.SyncerDefaults{
    ParallelUploads:   deduced.ParallelSyncs,
    ParallelDownloads: deduced.ParallelFetches,
    PrefetchWorkers:   4, // fixed, not deduced
})
```

## State of the Art

| Old Approach | Current Approach | When Changed | Impact |
|--------------|------------------|--------------|--------|
| `runtime.NumCPU()` for CPU count | `runtime.GOMAXPROCS(0)` | Go 1.25 (cgroup-aware) | Correct CPU count in containers |
| `syscall.Sysctl("hw.memsize")` on macOS | `x/sys/unix.SysctlUint64("hw.memsize")` | Always (bug in syscall) | Correct 64-bit memory value |
| Third-party `automaxprocs` for container CPU | Stdlib Go 1.25 | Go 1.25 release | No external dependency needed |
| Global cache/offloader config sections | Per-share auto-deduced defaults | This phase | Simpler config, better defaults |

**Deprecated/outdated:**
- `runtime.NumCPU()`: Returns physical cores, not cgroup-limited cores. Use `GOMAXPROCS(0)` instead.
- `CacheConfig` / `OffloaderConfig` in `pkg/config/config.go`: Being removed in this phase.
- `applyCacheDefaults()` / `applyOffloaderDefaults()` in `pkg/config/defaults.go`: Being removed.

## Open Questions

1. **Cache path after removing CacheConfig**
   - What we know: `CacheConfig.Path` is currently required and validated. The local block store's path comes from the block store config in the DB (via `dfsctl store block local add --config '{"path":"..."}'`).
   - What's unclear: Can we remove `Cache.Path` from config.yaml entirely since local store paths come from block store configs?
   - Recommendation: Yes, remove it. The path is already in the DB-managed block store config. The config template should explain this. Validation should be updated to not require Cache.Path.

2. **PrefetchWorkers deduction**
   - What we know: CONTEXT.md says PrefetchBlocks is removed as static config. Current code has `PrefetchWorkers` default of 4.
   - What's unclear: Should PrefetchWorkers be auto-deduced from CPUs, or remain a fixed default?
   - Recommendation: Keep fixed at 4. Prefetch workers are bounded by L1 cache budget anyway; scaling with CPUs adds no benefit. The CONTEXT.md does not list PrefetchWorkers among deduced values.

3. **Per-share override API fields**
   - What we know: CONTEXT.md says per-share overrides via `--local-store-size`, `--l1-cache-size`, `--parallel-syncs`, `--parallel-fetches` flags on share create/edit.
   - What's unclear: Whether these override fields need DB model changes (Share table columns) or just runtime-only application.
   - Recommendation: Add optional fields to Share model for persistence across restarts. Zero value = use auto-deduced defaults.

## Validation Architecture

### Test Framework
| Property | Value |
|----------|-------|
| Framework | Go testing (stdlib) |
| Config file | None needed |
| Quick run command | `go test ./internal/sysinfo/ ./pkg/blockstore/ -run TestDeduc -count=1` |
| Full suite command | `go test ./internal/sysinfo/... ./pkg/blockstore/... ./pkg/config/... ./cmd/dfs/... -count=1` |

### Phase Requirements -> Test Map
| Req ID | Behavior | Test Type | Automated Command | File Exists? |
|--------|----------|-----------|-------------------|-------------|
| AUTO-01 | LocalStoreSize = 25% of memory | unit | `go test ./pkg/blockstore/ -run TestDeduceLocalStoreSize -count=1` | No -- Wave 0 |
| AUTO-02 | L1CacheSize = 12.5% of memory | unit | `go test ./pkg/blockstore/ -run TestDeduceL1CacheSize -count=1` | No -- Wave 0 |
| AUTO-03 | ParallelSyncs = max(4, cpus) | unit | `go test ./pkg/blockstore/ -run TestDeduceParallelSyncs -count=1` | No -- Wave 0 |
| AUTO-04 | ParallelFetches = max(8, cpus*2) | unit | `go test ./pkg/blockstore/ -run TestDeduceParallelFetches -count=1` | No -- Wave 0 |
| AUTO-05 | User override preserves explicit values | unit | `go test ./pkg/blockstore/ -run TestDeduceOverride -count=1` | No -- Wave 0 |
| AUTO-01 | Floor: LocalStoreSize >= 256MiB | unit | `go test ./pkg/blockstore/ -run TestDeduceFloor -count=1` | No -- Wave 0 |
| AUTO-02 | Floor: L1CacheSize >= 64MiB | unit | `go test ./pkg/blockstore/ -run TestDeduceFloor -count=1` | No -- Wave 0 |
| -- | Linux cgroup v2 memory.max parsing | unit | `go test ./internal/sysinfo/ -run TestCgroup -count=1` | No -- Wave 0 |
| -- | Linux /proc/meminfo fallback | unit | `go test ./internal/sysinfo/ -run TestProcMeminfo -count=1` | No -- Wave 0 |
| -- | Config removal: old sections rejected gracefully | unit | `go test ./pkg/config/ -run TestRemoved -count=1` | No -- Wave 0 |

### Sampling Rate
- **Per task commit:** `go test ./internal/sysinfo/... ./pkg/blockstore/... -count=1`
- **Per wave merge:** `go test ./... -count=1` (excludes integration and e2e tagged tests)
- **Phase gate:** Full suite green before `/gsd:verify-work`

### Wave 0 Gaps
- [ ] `internal/sysinfo/sysinfo_test.go` -- interface-level tests with mock detector
- [ ] `pkg/blockstore/defaults.go` -- deduction functions (new file)
- [ ] `pkg/blockstore/defaults_test.go` -- table-driven tests for all deduction scenarios
- [ ] `pkg/config/defaults_test.go` -- update existing tests after CacheConfig/OffloaderConfig removal

## Sources

### Primary (HIGH confidence)
- **Go 1.25 release notes / container-aware GOMAXPROCS blog post** -- Verified that `GOMAXPROCS(0)` is cgroup-aware in Go 1.25+
- **golang/go#21614** -- Confirmed `syscall.Sysctl("hw.memsize")` truncation bug on macOS
- **`golang.org/x/sys/unix` package** -- `SysctlUint64` available for 64-bit sysctl reads
- **Linux kernel cgroup v2 docs** -- `memory.max` contains "max" when unlimited

### Secondary (MEDIUM confidence)
- **go-osstat, gopsutil source code** -- Verified `GlobalMemoryStatusEx` struct layout and calling convention for Windows
- **kubernetes/kubernetes PR #57124** -- Confirmed Windows memory detection pattern via `GlobalMemoryStatusEx`

### Tertiary (LOW confidence)
- None -- all findings verified with primary or secondary sources

## Metadata

**Confidence breakdown:**
- Standard stack: HIGH -- all libraries already in project, no new dependencies
- Architecture: HIGH -- follows established project patterns (build tags, interface-based testing, ApplyDefaults)
- Pitfalls: HIGH -- verified with official docs and known Go issues
- OS detection: HIGH for Linux/macOS (well-documented), MEDIUM for Windows (verified from multiple open-source projects)

**Research date:** 2026-03-10
**Valid until:** 2026-04-10 (stable domain, no fast-moving dependencies)
