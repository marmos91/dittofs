---
phase: 42
slug: legacy-cleanup
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-09
---

# Phase 42 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (built-in), go 1.25.5 |
| **Config file** | go.mod (project root) |
| **Quick run command** | `go build ./...` |
| **Full suite command** | `go build ./... && go test ./...` |
| **Estimated runtime** | ~60 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go build ./... && go test ./...`
- **After every plan wave:** Run `go build ./... && go test ./...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 60 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 42-01-01 | 01 | 1 | CLEAN-01 | compilation | `go build ./pkg/payload/store/...` | N/A (deletion) | ⬜ pending |
| 42-01-02 | 01 | 1 | CLEAN-02 | compilation | `go build ./...` | N/A (deletion) | ⬜ pending |
| 42-01-03 | 01 | 1 | CLEAN-03 | compilation + unit | `go test ./pkg/cache/...` | Existing cache_test.go | ⬜ pending |
| 42-01-04 | 01 | 1 | CLEAN-04 | compilation | `go build ./pkg/payload/offloader/...` | Existing offloader_test.go | ⬜ pending |
| 42-01-05 | 01 | 1 | CLEAN-05 | compilation | `go build ./pkg/controlplane/runtime/...` | N/A (compilation) | ⬜ pending |
| 42-01-06 | 01 | 1 | CLEAN-06 | compilation + unit | `go test ./pkg/cache/...` | Existing cache_test.go | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

Existing infrastructure covers all phase requirements. This is a deletion phase that reduces test code, not adds it.

---

## Manual-Only Verifications

All phase behaviors have automated verification (compilation success = requirement met for deletions).

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 60s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
