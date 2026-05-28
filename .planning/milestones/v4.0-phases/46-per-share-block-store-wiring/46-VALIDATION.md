---
phase: 46
slug: per-share-block-store-wiring
status: draft
nyquist_compliant: true
wave_0_complete: false
created: 2026-03-10
---

# Phase 46 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) |
| **Config file** | none (Go convention) |
| **Quick run command** | `go test ./pkg/controlplane/runtime/...` |
| **Full suite command** | `go build ./... && go test ./pkg/controlplane/... ./internal/adapter/... ./cmd/...` |
| **Estimated runtime** | ~30 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go build ./... && go test ./pkg/controlplane/runtime/...`
- **After every plan wave:** Run `go build ./... && go test ./...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 30 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 46-01-00 | 01 | 1 | W0 | scaffold | `go test ./pkg/controlplane/runtime/... -run "TestPerShare\|TestRemoveShare\|TestGetBlockStoreForHandle" -v` | ✅ (creates stubs) | ⬜ pending |
| 46-01-01 | 01 | 1 | SHARE-01 | build | `go build ./...` | ✅ | ⬜ pending |
| 46-01-02 | 01 | 1 | SHARE-02 | build | `go build ./...` | ✅ | ⬜ pending |
| 46-01-03 | 01 | 1 | SHARE-01,02,04 | unit | `go test ./pkg/controlplane/runtime/... -run "TestPerShare\|TestRemoveShare\|TestGetBlockStoreForHandle" -v` | ✅ (from W0) | ⬜ pending |
| 46-02-01 | 02 | 2 | SHARE-03 | build | `go build ./internal/adapter/...` | ✅ | ⬜ pending |
| 46-02-02 | 02 | 2 | SHARE-03 | unit | `go test ./internal/adapter/nfs/... ./internal/adapter/smb/...` | ✅ | ⬜ pending |
| 46-03-01 | 03 | 3 | SHARE-01 | build | `go build ./...` | ✅ | ⬜ pending |
| 46-03-02 | 03 | 3 | SHARE-01 | unit | `go test ./pkg/controlplane/runtime/...` | ✅ | ⬜ pending |

*Status: pending / green / red / flaky*

---

## Wave 0 Requirements

- [ ] **Task 0 in Plan 01** creates `t.Skip()` stubs in `init_test.go` and `runtime_test.go`
  - Replaces `TestEnsureBlockStoreLocalOnly` with `TestPerShareBlockStoreLocalOnly` (stub)
  - Adds `TestPerShareBlockStoreIsolation` (stub)
  - Adds `TestPerShareBlockStoreRemoteSharing` (stub)
  - Adds `TestRemoveShareClosesBlockStore` (stub)
  - Adds `TestGetBlockStoreForHandle` (stub) in `runtime_test.go`
- [ ] Task 0 runs FIRST, before Tasks 1-2 (production code)
- [ ] Task 3 replaces stubs with real implementations

*Wave 0 is satisfied by Task 0 in Plan 01. Test files exist (with skips) before production tasks run.*

---

## Manual-Only Verifications

*All phase behaviors have automated verification.*

---

## Validation Sign-Off

- [x] All tasks have `<automated>` verify or Wave 0 dependencies
- [x] Sampling continuity: no 3 consecutive tasks without automated verify
- [x] Wave 0 covers all MISSING references (Task 0 creates stubs)
- [x] No watch-mode flags
- [x] Feedback latency < 30s
- [x] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
