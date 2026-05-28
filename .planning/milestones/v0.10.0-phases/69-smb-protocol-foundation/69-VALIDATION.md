---
phase: 69
slug: smb-protocol-foundation
status: draft
nyquist_compliant: false
wave_0_complete: false
created: 2026-03-20
---

# Phase 69 — Validation Strategy

> Per-phase validation contract for feedback sampling during execution.

---

## Test Infrastructure

| Property | Value |
|----------|-------|
| **Framework** | go test |
| **Config file** | none — standard Go test infrastructure |
| **Quick run command** | `go test ./internal/adapter/smb/...` |
| **Full suite command** | `go build ./... && go test ./...` |
| **Estimated runtime** | ~60 seconds |

---

## Sampling Rate

- **After every task commit:** Run `go test ./internal/adapter/smb/...`
- **After every plan wave:** Run `go build ./... && go test ./...`
- **Before `/gsd:verify-work`:** Full suite must be green
- **Max feedback latency:** 60 seconds

---

## Per-Task Verification Map

| Task ID | Plan | Wave | Requirement | Test Type | Automated Command | File Exists | Status |
|---------|------|------|-------------|-----------|-------------------|-------------|--------|
| 69-01-01 | 01 | 1 | SMB-01 | unit+integration | `go test ./internal/adapter/smb/signing/...` | TBD | ⬜ pending |
| 69-01-02 | 01 | 1 | SMB-01 | manual | macOS mount_smbfs test | N/A | ⬜ pending |
| 69-02-01 | 02 | 1 | SMB-02 | unit | `go test ./internal/adapter/smb/session/...` | TBD | ⬜ pending |
| 69-02-02 | 02 | 1 | SMB-03 | unit | `go test ./internal/adapter/smb/session/...` | TBD | ⬜ pending |
| 69-02-03 | 02 | 1 | SMB-04 | unit | `go test ./internal/adapter/smb/...` | TBD | ⬜ pending |
| 69-02-04 | 02 | 1 | SMB-05 | unit | `go test ./internal/adapter/smb/...` | TBD | ⬜ pending |

*Status: ⬜ pending · ✅ green · ❌ red · ⚠️ flaky*

---

## Wave 0 Requirements

Existing infrastructure covers all phase requirements. Go test framework already in place with SMB-specific test files.

---

## Manual-Only Verifications

| Behavior | Requirement | Why Manual | Test Instructions |
|----------|-------------|------------|-------------------|
| macOS mount_smbfs signing | SMB-01 | Requires macOS host with mount_smbfs | 1. Start DittoFS server 2. Run `mount_smbfs //user@host/share /mnt` on macOS 3. Verify mount succeeds without signature errors 4. Create/read files to confirm signing works end-to-end |
| Windows 11 signing regression | SMB-01 | Requires Windows 11 client | 1. Connect from Windows 11 client 2. Verify NET USE succeeds with signing 3. Confirm no regression from macOS fix |
| WPTS BVT regression check | All | Requires Docker WPTS environment | 1. Run WPTS BVT suite 2. Compare results against baseline 3. Confirm no new failures |

---

## Validation Sign-Off

- [ ] All tasks have `<automated>` verify or Wave 0 dependencies
- [ ] Sampling continuity: no 3 consecutive tasks without automated verify
- [ ] Wave 0 covers all MISSING references
- [ ] No watch-mode flags
- [ ] Feedback latency < 60s
- [ ] `nyquist_compliant: true` set in frontmatter

**Approval:** pending
