---
phase: 45
slug: package-restructure
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-09
---

# Phase 45 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing (stdlib) |
| **Config file** | None (go test uses convention) |
| **Quick run command** | `go test ./pkg/blockstore/...` |
| **Full suite command** | `go build ./... && go test ./...` |
| **Estimated runtime** | ~30 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go build ./... && go test ./...`
- **After every plan wave:** Run `go build ./... && go test ./... && go vet ./...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 30 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 45-01-01 | 01 | 1 | PKG-01 | build | `go build ./pkg/blockstore/local/` | ❌ W0 | ⬜ pending |
| 45-01-02 | 01 | 1 | PKG-02 | build | `go build ./pkg/blockstore/remote/` | ❌ W0 | ⬜ pending |
| 45-02-01 | 02 | 2 | PKG-03 | unit | `go test ./pkg/blockstore/local/fs/` | ❌ W0 | ⬜ pending |
| 45-02-02 | 02 | 2 | PKG-04 | unit | `go test ./pkg/blockstore/local/memory/` | ❌ W0 | ⬜ pending |
| 45-02-03 | 02 | 2 | PKG-05 | build | `go build ./pkg/blockstore/remote/s3/` | ❌ W0 | ⬜ pending |
| 45-02-04 | 02 | 2 | PKG-06 | unit | `go test ./pkg/blockstore/remote/memory/` | ❌ W0 | ⬜ pending |
| 45-02-05 | 02 | 2 | PKG-07 | unit | `go test ./pkg/blockstore/sync/` | ❌ W0 | ⬜ pending |
| 45-02-06 | 02 | 2 | PKG-08 | unit | `go test ./pkg/blockstore/gc/` | ❌ W0 | ⬜ pending |
| 45-03-01 | 03 | 3 | PKG-09 | unit | `go test ./pkg/blockstore/` | ❌ W0 | ⬜ pending |
| 45-04-01 | 04 | 4 | PKG-10 | build | `go build ./...` | N/A | ⬜ pending |
| 45-04-02 | 04 | 4 | PKG-11 | build | `go build ./... && ! test -d pkg/cache && ! test -d pkg/payload` | N/A | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

*Existing infrastructure covers all phase requirements. Go testing stdlib is already in place. New test files are created as part of each plan (conformance suites, moved tests).*

---

## Manual-Only Verifications

*All phase behaviors have automated verification. `go build ./...` and `go test ./...` verify all moves.*

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 30s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
