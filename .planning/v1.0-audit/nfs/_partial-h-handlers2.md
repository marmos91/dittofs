# NFS Handlers — Second-Pass Audit (`_partial-h-handlers2.md`)

**Date**: 2026-05-30.
**Scope**: Second-pass focused on per-op edge cases the first pass missed. All findings are NEW — not in REVIEW.md.
**Method**: Read all 52 v3 handler files, 36 v4 handler files, 12 v4.1 handler files, 11 mount handler files; cross-checked vs RFC 1813/7530/8881; used WRITE (correct) as reference.

---

## v3 handlers

No new HIGH beyond REVIEW.md. Second pass confirms v3 otherwise clean at handler level: RENAME validates dot/dotdot, cross-share detection correct, LOOKUP delegates dot/dotdot to store, SETATTR two-phase split (size then other) is RFC-correct.

### [LOW] SETATTR two-phase: second-phase failure leaves new size + stale mode/uid/gid, no rollback — `v3/handlers/setattr.go:296-308` — NEW
Both Size + other attrs: size-only applied first (correct per RFC), then remaining attrs. If second `SetFileAttributes` fails, file has new size but old mode/uid/gid; no rollback. Hard to trigger (size ok, perms fail e.g. non-root chown) but silent divergence; client errors and may retry against already-truncated file. Fix: atomic dual-attr if store supports, or compensating size-restore on second-phase failure. Confidence 82.

---

## v4 COMPOUND handlers

### [HIGH] SETATTR: stateid decoded but `ValidateStateid` NEVER called — no lease renewal, no stale rejection, no open-mode guard — `v4/handlers/setattr.go:43-57` — NEW
Stateid decoded (line 43), logged (line 54, comment "StateManager validates separately"), then discarded. No `ValidateStateid` anywhere in setattr.go or callees. Consequences: (1) bogus/expired stateid succeeds → should be `NFS4ERR_BAD_STATEID`/`NFS4ERR_STALE_STATEID`; (2) no implicit lease renewal (RFC 7530 §8.1.3); (3) revoked-delegation stateid can SETATTR; (4) read-only-open stateid not rejected on size-reduce. Compare WRITE (`write.go:129` checks `SHARE_ACCESS_WRITE`), LOCK (`state/lockowner.go:160`). Fix: `openState, err := h.StateManager.ValidateStateid(stateid, ctx.CurrentFH)` after decode; `NFS4ERR_BAD_STATEID` on error; check write access on size-reduce. **Confidence 100.** Security: any observed stateid can SETATTR after the real holder closed.

### [HIGH] READ: `ValidateStateid` return discarded — no `SHARE_ACCESS_READ` check — `v4/handlers/read.go:71` — NEW
`if _, stateErr := h.StateManager.ValidateStateid(...)` — `openState` thrown away. Per RFC 7530 §16.25.4, a regular open stateid must be verified opened with `SHARE_ACCESS_READ`. Without it a write-only open can READ via its stateid (symmetric to H3 on the READ side, for ANY open stateid). Compare `write.go:145-156` (checks WRITE, returns `NFS4ERR_OPENMODE`). Fix: capture openState; `if openState != nil && openState.ShareAccess&OPEN4_SHARE_ACCESS_READ == 0 { return NFS4ERR_OPENMODE }`. **Confidence 100.**

### [MED] OPEN EXCLUSIVE4 + EXCLUSIVE4_1 verifiers consumed & discarded — idempotent retry broken — `v4/handlers/open.go:118-141` — NEW
Both decode `createverf4` then set `createMode = GUARDED4`, dropping the verifier. Per RFC 7530 §16.16.3 EXCLUSIVE create must be idempotent: retry with same verifier on an existing file returns the existing stateid, not `NFS4ERR_EXIST`. GUARDED4 path hits exist-check (`open.go:263`) → spurious `NFS4ERR_EXIST` on lost-reply retry. Distinct from v3 CREATE-EXCLUSIVE (REVIEW) — here it's the primary v4 create path + v4.1 variant. Fix: store verifier as IdempotencyToken; on GUARDED4 exist, compare token, return existing stateid on match. Confidence 95.

### [MED] ACCESS on pseudo-fs unconditionally grants MODIFY/EXTEND/DELETE — misleads clients vs NFS4ERR_ROFS — `v4/handlers/access.go:52-63` — NEW
Pseudo-fs path reflects `accessReq` back as fully granted + advertises all bits supported. Pseudo-fs is read-only → subsequent mutating op returns `NFS4ERR_ROFS`. Clients (macOS Finder) pre-checking writable access see MODIFY granted then ROFS → spurious app-level "permission denied" misattributed to ACLs. RFC 7530 §16.1.3: report only genuinely supported bits. Fix: clamp supported + granted to `ACCESS4_READ|ACCESS4_LOOKUP`. Confidence 88.

---

## v4.1 session handlers

### [LOW] `dispatchV41Ops` returns `(nil, error)` on context cancel — no wire response for exempt ops — `v4/handlers/compound.go:512-518` — NEW
Session-exempt path (EXCHANGE_ID, CREATE_SESSION…) returns `nil, ctx.Err()` on cancel → RPC layer drops reply → client sees EOF/RST, full timeout before retry. `dispatchV41` (non-exempt, lines 407-416) correctly encodes `NFS4ERR_DELAY`. Fix: encode DELAY/NOTSUPP on cancel. Confidence 82.

---

## v4 RENAME

### [LOW] RENAME self-rename (SavedFH==CurrentFH && oldName==newName) not detected — `v4/handlers/rename.go:150` — NEW
No self-detection before `metaSvc.Move`. RFC 7530 §16.29.4: same-file rename MUST be no-op success. Store behavior undefined (some backends ENOENT the target). v3 RENAME shares the gap but v3 store handles it at syscall layer; v4 uses DittoFS metadata layer. Fix: detect same-dir+same-name, encode success with before==after. Confidence 80.

---

## mount handlers

Clean on second pass. Auth-flavor policy, netgroup ACL, disabled-share gate, export-path validation, pseudoflavor advertising all correct. No new findings.

---

## Severity tally

| Severity | Count | NEW |
|---|---|---|
| HIGH | 2 | 2 |
| MED | 2 | 2 |
| LOW | 3 | 3 |
| **Total** | **7** | **7** |

All NEW; none duplicates REVIEW.md.

**Top finding**: SETATTR stateid never validated (`setattr.go:43`, confidence 100) — bogus stateid silently succeeds, no lease renewal, revoked-delegation stateid can modify attrs; any client that observes a stateid can SETATTR files after the real holder closed.
