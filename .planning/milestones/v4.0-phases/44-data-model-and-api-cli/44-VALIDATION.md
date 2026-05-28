---
phase: 44
slug: data-model-and-api-cli
status: draft
nyquist_compliant: true
wave_0_complete: false
created: 2026-03-09
---

# Phase 44 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go testing + `go test` |
| **Config file** | None (Go convention) |
| **Quick run command** | `go test ./pkg/controlplane/... -run TestBlockStore -count=1` |
| **Full suite command** | `go test ./pkg/controlplane/... -count=1 && go test ./internal/controlplane/... -count=1 && go test ./pkg/apiclient/... -count=1` |
| **Estimated runtime** | ~15 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go build ./... && go vet ./...`
- **After every plan wave:** Run `go test ./pkg/controlplane/... -count=1 && go test ./internal/controlplane/... -count=1`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 15 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 44-01-01 | 01 | 1 | MODEL-01 | unit | MISSING (W0: `pkg/controlplane/store/block_test.go`) -- Task 1 defines types only | ❌ W0 | ⬜ pending |
| 44-01-02 | 01 | 1 | MODEL-02..05 | unit | `go test ./pkg/controlplane/store/ -run "TestBlockStoreOperations\|TestBlockStoreKindFilter\|TestShareBlockStore\|TestMigrationBlockStore\|TestMigrationShareBlockStore" -count=1` | ❌ W0 | ⬜ pending |
| 44-02-01 | 02 | 2 | API-01, API-02 | unit | `go test ./internal/controlplane/api/handlers/ -run "TestBlockStoreHandler" -count=1` | ❌ W0 | ⬜ pending |
| 44-02-02 | 02 | 2 | API-03, CLI-04 | unit | `go test ./internal/controlplane/api/handlers/ -run "TestShareBlockStore" -count=1 && go test ./pkg/apiclient/ -run "TestBlockStore" -count=1` | ❌ W0 | ⬜ pending |
| 44-03-01 | 03 | 3 | CLI-01, CLI-02 | manual | MISSING (CLI commands manual-only). Compile check: `go build ./cmd/dfsctl/...` | N/A | ⬜ pending |
| 44-03-02 | 03 | 3 | CLI-03 | unit+manual | `go test ./pkg/apiclient/ -run "TestShareCreate" -count=1 && go build ./cmd/dfsctl/...` | ❌ W0 | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `pkg/controlplane/store/block_test.go` — stubs for MODEL-01 through MODEL-05 (CRUD, kind filter, migration tests)
- [ ] `internal/controlplane/api/handlers/block_stores_test.go` — stubs for API-01, API-02, API-03
- [ ] `pkg/apiclient/block_stores_test.go` — stubs for CLI-04 and share create

*Existing `go test` infrastructure covers all framework needs.*

---

## Nyquist Sampling Continuity

| Wave | Task 1 | Task 2 | Continuity |
|------|--------|--------|------------|
| 1 (Plan 01) | MISSING (types only) | `go test` store tests | OK -- Task 2 covers Wave 0 stubs + store tests |
| 2 (Plan 02) | `go test` handler tests | `go test` handler + client tests | OK -- both tasks have automated tests |
| 3 (Plan 03) | MISSING (CLI manual) | `go test` client tests + build | OK -- no 3 consecutive MISSING |

**Max consecutive without automated test:** 1 (44-01 T1 MISSING, then 44-01 T2 has tests; 44-03 T1 MISSING, then 44-03 T2 has tests)

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| `dfsctl store block local` commands | CLI-01 | CLI commands require binary + interactive prompts | Build `dfsctl`, run `store block local add/list/remove` |
| `dfsctl store block remote` commands | CLI-02 | CLI commands require binary + interactive prompts | Build `dfsctl`, run `store block remote add/list/remove` |
| `dfsctl share create --local --remote` | CLI-03 | Share create with new flags requires running server | Build `dfsctl`, run `share create --local X --remote Y` |

*All manual verifications are CLI smoke tests. Core logic is verified via automated unit tests.*

---

## Validation Sign-Off

- [x] All tasks have `<automated>` verify or Wave 0 dependencies
- [x] Sampling continuity: no 3 consecutive tasks without automated verify
- [x] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 15s
- [x] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
