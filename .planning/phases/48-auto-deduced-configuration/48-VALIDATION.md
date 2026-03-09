---
phase: 48
slug: auto-deduced-configuration
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-10
---

# Phase 48 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) |
| **Config file** | None needed |
| **Quick run command** | `go test ./internal/sysinfo/ ./pkg/blockstore/ -run TestDeduc -count=1` |
| **Full suite command** | `go test ./internal/sysinfo/... ./pkg/blockstore/... ./pkg/config/... ./cmd/dfs/... -count=1` |
| **Estimated runtime** | ~10 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/sysinfo/... ./pkg/blockstore/... -count=1`
- **After every plan wave:** Run `go test ./... -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 15 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 48-01-01 | 01 | 1 | AUTO-01 | unit | `go test ./pkg/blockstore/ -run TestDeduceLocalStoreSize -count=1` | ❌ W0 | ⬜ pending |
| 48-01-02 | 01 | 1 | AUTO-02 | unit | `go test ./pkg/blockstore/ -run TestDeduceL1CacheSize -count=1` | ❌ W0 | ⬜ pending |
| 48-01-03 | 01 | 1 | AUTO-03 | unit | `go test ./pkg/blockstore/ -run TestDeduceParallelSyncs -count=1` | ❌ W0 | ⬜ pending |
| 48-01-04 | 01 | 1 | AUTO-04 | unit | `go test ./pkg/blockstore/ -run TestDeduceParallelFetches -count=1` | ❌ W0 | ⬜ pending |
| 48-01-05 | 01 | 1 | AUTO-05 | unit | `go test ./pkg/blockstore/ -run TestDeduceOverride -count=1` | ❌ W0 | ⬜ pending |
| 48-01-06 | 01 | 1 | AUTO-01 | unit | `go test ./pkg/blockstore/ -run TestDeduceFloor -count=1` | ❌ W0 | ⬜ pending |
| 48-02-01 | 02 | 1 | -- | unit | `go test ./internal/sysinfo/ -run TestCgroup -count=1` | ❌ W0 | ⬜ pending |
| 48-02-02 | 02 | 1 | -- | unit | `go test ./internal/sysinfo/ -run TestProcMeminfo -count=1` | ❌ W0 | ⬜ pending |
| 48-03-01 | 03 | 2 | -- | unit | `go test ./pkg/config/ -run TestRemoved -count=1` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `pkg/blockstore/defaults_test.go` — stubs for AUTO-01 through AUTO-05 deduction tests
- [ ] `internal/sysinfo/sysinfo_test.go` — interface-level tests with mock detector
- [ ] `pkg/config/defaults_test.go` — update existing tests after CacheConfig/OffloaderConfig removal

*Existing Go test infrastructure covers framework needs.*

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| Startup INFO log shows deduced values | -- | Log output verification | Start server, check logs for "Auto-deduced defaults:" line |
| `dfs config show --deduced` output | -- | CLI output format | Run command, verify YAML with inline comments |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 15s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
