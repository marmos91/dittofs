---
phase: 72
slug: wpts-conformance-push
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-04-07
---

# Phase 72 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | Go `testing` (unit) + WPTS BVT (integration, in Docker, runs x86 MSTest binary) |
| **Config file** | `test/smb-conformance/run.sh`, `test/smb-conformance/KNOWN_FAILURES.md` |
| **Quick run command** | `go test ./internal/adapter/smb/...` |
| **Full suite command** | `cd test/smb-conformance && ./run.sh` (Linux CI authoritative) |
| **Estimated runtime** | Unit: ~30s; WPTS BVT: ~25 min |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/adapter/smb/lease/... ./internal/adapter/smb/v2/handlers/...` (≤ 30s)
- **After every plan wave:** Run full unit suite `go test ./...` and a focused WPTS BVT subset for the touched test names
- **Before `/gsd-verify-work`:** Full WPTS BVT must be green in **Linux CI** (not Mac+QEMU). Mac+QEMU diff documented but non-authoritative per D-10a.
- **Max feedback latency:** 30 seconds for unit gate; ~25 min for full WPTS BVT (gated to wave boundary)

---

## Per-Task Verification Map (revised post-#323)

| Task ID | Plan | Wave | Requirement | Threat Ref | Secure Behavior | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|------------|-----------------|-----------|-------------------|-------------|--------|
| 72-01-01 | 01 | 0 | WPTS-03 | — | `BreakParentHandleLeasesOnCreate` calls `WaitForBreakCompletion` with bounded timeout | unit | `go test ./internal/adapter/smb/lease -run TestBreakParentHandleLeasesOnCreate_WaitsForAck` | ❌ W0 | ⬜ pending |
| 72-01-02 | 01 | 0 | WPTS-03 | — | `BreakParentReadLeasesOnModify` calls `WaitForBreakCompletion` with bounded timeout | unit | `go test ./internal/adapter/smb/lease -run TestBreakParentReadLeasesOnModify_WaitsForAck` | ❌ W0 | ⬜ pending |
| 72-01-03 | 01 | 0 | WPTS-03 | — | Excluded client (triggering CREATE) is not waited on (no self-deadlock) | unit | `go test ./internal/adapter/smb/lease -run TestBreakParentHandle_ExcludesTriggeringClient` | ❌ W0 | ⬜ pending |
| 72-01-04 | 01 | 1 | WPTS-03 | — | `BVT_DirectoryLeasing_LeaseBreakOnMultiClients` passes in Linux CI 5 consecutive runs (no flake) | integration | `gh workflow run smb-conformance.yml` (filtered, x5) | ✅ | ⬜ pending |
| 72-02-01 | 02 | 2 | WPTS-04 | — | Linux CI WPTS BVT post-fix: 0 new failures, lease test PASS | integration | `gh workflow run smb-conformance.yml` | ✅ | ⬜ pending |
| 72-02-02 | 02 | 2 | WPTS-04 | T-72-08 | `KNOWN_FAILURES.md` header count == table count == footer count | manual | `grep -c` consistency check | ✅ | ⬜ pending |
| 72-02-03 | 02 | 2 | WPTS-04 | — | 3 deferred timestamp tests labeled with structured "blocked on cross-protocol POSIX timestamp phase" reason | manual | grep + visual review | ✅ | ⬜ pending |
| 72-02-04 | 02 | 2 | WPTS-04 | — | ROADMAP.md Phase 72 marked `[x]` with descope note crediting both PR #323 and this branch | manual | grep + visual review | ✅ | ⬜ pending |
| 72-02-05 | 02 | 2 | WPTS-04 | T-72-09 | Phase 72 SUMMARY.md committed; PR opened | manual | file exists + `gh pr view` | ✅ | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

- [ ] `internal/adapter/smb/lease/manager_test.go` — add `TestBreakParentHandleLeasesOnCreate_WaitsForAck`, `TestBreakParentReadLeasesOnModify_WaitsForAck`, `TestBreakParentHandle_ExcludesTriggeringClient`
- [ ] Branch `fix/phase-72-wpts-trailing-fixes` cut off `origin/develop` (already done — this worktree)
- [ ] No CI baseline capture needed: PR #323 already shipped Plan 02's original ChangeNotify scope, and Plan 02 (formerly 04) gates on a single post-fix CI run in its own task

**Removed from Wave 0 (post-#323 supersession):**
- ~~`response_test.go` TestSendAsyncChangeNotifyResponse_Cleanup~~ — superseded by PR #323's regression tests in `close_notify_test.go` and `smbenc/writer_test.go`
- ~~`close_test.go` TestClose_PendingNotify_Cleanup~~ — superseded by PR #323's regression tests
- ~~Pre-fix Linux CI baseline~~ — current `develop` IS the baseline; #323 already moved the baseline forward
- ~~Mac+QEMU baseline~~ — non-authoritative per D-10a; not worth the developer-time cost for a 1-bug phase

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| WPTS BVT pass count and known-failure list match `KNOWN_FAILURES.md` post-fix | WPTS-04 | WPTS BVT runs in Docker against MSTest binary; result parsing handled by `parse-results.sh`, not Go test framework | Run `cd test/smb-conformance && ./run.sh` in Linux CI; compare output against `KNOWN_FAILURES.md`; verify 0 new entries needed |
| Mac+QEMU diff vs Linux CI documented as noise floor | D-10a | QEMU emulation differences are environmental, not protocol-correctness | After both runs complete, diff results, list any tests that pass in one and fail in the other in `72-SUMMARY.md` |
| `KNOWN_FAILURES.md` header/table reconciliation | D-10 | Cross-section count consistency check | `grep -c "^|" test/smb-conformance/KNOWN_FAILURES.md` rows vs header count vs category breakdown — all three must agree |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 30s for unit gate
- [ ] `nyquist_compliant: true` set in frontmatter
- [ ] Linux CI baseline captured before any code change
- [ ] Both target tests pass in Linux CI for 5 consecutive runs (lease test specifically — flake gate)

**Approval:** pending
