---
phase: 47
slug: l1-read-cache-and-prefetch
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-10
---

# Phase 47 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | go test |
| **Config file** | none — existing go test infrastructure |
| **Quick run command** | `go test ./pkg/blockstore/...` |
| **Full suite command** | `go test -race ./pkg/blockstore/...` |
| **Estimated runtime** | ~15 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./pkg/blockstore/...`
- **After every plan wave:** Run `go test -race ./pkg/blockstore/...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 15 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 47-01-01 | 01 | 1 | PERF-01 | unit | `go test ./pkg/blockstore/readcache/...` | ❌ W0 | ⬜ pending |
| 47-01-02 | 01 | 1 | PERF-01 | unit | `go test ./pkg/blockstore/readcache/...` | ❌ W0 | ⬜ pending |
| 47-01-03 | 01 | 1 | PERF-02 | unit | `go test ./pkg/blockstore/readcache/...` | ❌ W0 | ⬜ pending |
| 47-02-01 | 02 | 1 | PERF-03 | unit | `go test ./pkg/blockstore/prefetch/...` | ❌ W0 | ⬜ pending |
| 47-02-02 | 02 | 1 | PERF-03 | unit | `go test ./pkg/blockstore/prefetch/...` | ❌ W0 | ⬜ pending |
| 47-02-03 | 02 | 1 | PERF-04 | unit | `go test ./pkg/blockstore/prefetch/...` | ❌ W0 | ⬜ pending |
| 47-03-01 | 03 | 2 | PERF-01 | integration | `go test ./pkg/blockstore/...` | ❌ W0 | ⬜ pending |
| 47-03-02 | 03 | 2 | PERF-04 | benchmark | `go test -bench=. ./pkg/blockstore/...` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `pkg/blockstore/readcache/readcache_test.go` — stubs for PERF-01, PERF-02
- [ ] `pkg/blockstore/prefetch/prefetch_test.go` — stubs for PERF-03, PERF-04
- [ ] `pkg/blockstore/engine_cache_test.go` — integration tests for cache + prefetch in engine

*Existing go test infrastructure covers framework needs.*

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| Sequential read throughput improvement | PERF-04 | Requires real I/O patterns | Run benchmark, compare with/without L1 cache |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 15s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
