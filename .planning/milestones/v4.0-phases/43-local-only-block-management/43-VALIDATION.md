---
phase: 43
slug: local-only-block-management
status: draft
nyquist_compliant: true
wave_0_complete: true
created: 2026-03-09
---

# Phase 43 -- Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) |
| **Config file** | none (Go convention) |
| **Quick run command** | `go test ./pkg/cache/ ./pkg/payload/offloader/ -count=1` |
| **Full suite command** | `go test ./... -count=1` |
| **Estimated runtime** | ~30 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./pkg/cache/ ./pkg/payload/offloader/ -count=1`
- **After every plan wave:** Run `go test ./... -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 30 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | Test Source | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 43-01-01 | 01 | 1 | LOCAL-01 | unit | `go test ./pkg/cache/ -run "TestManage\|TestEviction" -count=1 -v` | Created in task (tdd=true) | pending |
| 43-01-02 | 01 | 1 | LOCAL-01 | unit | `go build ./... && go test ./pkg/cache/ ./pkg/payload/ ./pkg/payload/store/ -count=1` | Existing callers updated | pending |
| 43-02-01 | 02 | 2 | LOCAL-02 | unit | `go test ./pkg/payload/offloader/ -run "TestNilBlockStore\|TestSetRemoteStore" -count=1 -v` | Created in task (tdd=true) | pending |
| 43-02-02 | 02 | 2 | LOCAL-04 | unit | `go build ./... && go test ./pkg/controlplane/runtime/ -run TestEnsurePayloadService -count=1 -v` | Created in task action | pending |

*Status: pending / green / red / flaky*

---

## Wave 0 Resolution

All Wave 0 test gaps are resolved within plan tasks themselves:

- **pkg/cache/manage_test.go** -- created by Plan 43-01 Task 1 (tdd=true, RED phase creates tests first)
- **EvictMemory rename tests** -- existing TestRemove in cache_test.go updated by Plan 43-01 Task 2
- **Nil blockStore offloader tests** -- created by Plan 43-02 Task 1 (tdd=true, explicit test functions listed in action)
- **SetRemoteStore tests** -- created by Plan 43-02 Task 1 (tdd=true, explicit test functions listed in action)
- **init.go local-only test** -- created by Plan 43-02 Task 2 (init_test.go with TestEnsurePayloadServiceLocalOnly)

**LOCAL-03 note:** "Local-only flush marks blocks BlockStateLocal" is existing behavior already implemented by `cache.Flush()` and tested in existing `cache_test.go` flush tests. No new task or test needed.

---

## Manual-Only Verifications

*All phase behaviors have automated verification.*

---

## Validation Sign-Off

- [x] All tasks have `<automated>` verify commands
- [x] Sampling continuity: no 3 consecutive tasks without automated verify
- [x] Wave 0 gaps resolved -- tests created within plan tasks (tdd=true or explicit action steps)
- [x] No watch-mode flags
- [x] Feedback latency < 30s
- [x] `nyquist_compliant: true` set in frontmatter

**Approval:** ready
