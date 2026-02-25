---
phase: 20-sequence-and-compound-bifurcation
verified: 2026-02-21T19:45:00Z
status: passed
score: 11/11 must-haves verified
re_verification: false
---

# Phase 20: SEQUENCE and COMPOUND Bifurcation Verification Report

**Phase Goal:** Every v4.1 COMPOUND is gated by SEQUENCE validation, providing exactly-once semantics while v4.0 clients continue working unchanged

**Verified:** 2026-02-21T19:45:00Z
**Status:** passed
**Re-verification:** No — initial verification

## Goal Achievement

### Observable Truths

| # | Truth | Status | Evidence |
|---|-------|--------|----------|
| 1 | v4.1 COMPOUND with SEQUENCE as first op validates session/slot/seqid and dispatches remaining ops | ✓ VERIFIED | `handleSequenceOp()` in sequence_handler.go validates via `ValidateSequence()`, returns V41RequestContext; `dispatchV41()` dispatches remaining ops (line 289-375 compound.go) |
| 2 | v4.1 COMPOUND without SEQUENCE as first op returns NFS4ERR_OP_NOT_IN_SESSION (unless exempt op) | ✓ VERIFIED | Line 239-243 compound.go checks `firstOpCode != OP_SEQUENCE` and returns NFS4ERR_OP_NOT_IN_SESSION; TestCompound_V41_NonExemptNoSequence passes |
| 3 | Exempt ops (EXCHANGE_ID, CREATE_SESSION, DESTROY_SESSION, BIND_CONN_TO_SESSION) work without SEQUENCE | ✓ VERIFIED | `isSessionExemptOp()` line 199-209 sequence_handler.go checks 4 ops; line 230-236 compound.go dispatches exempt ops without session context; TestCompound_V41_ExemptOpNoSequence passes |
| 4 | Duplicate v4.1 request (same slot+seqid) returns cached COMPOUND response without re-execution | ✓ VERIFIED | Line 69-79 sequence_handler.go returns `slot.CachedReply` on SeqRetry; line 253-256 compound.go returns cachedReply directly; TestSequence_ReplayWithCache verifies byte-identical response |
| 5 | Per-owner seqid validation is skipped for v4.1 operations (slot table provides replay protection) | ✓ VERIFIED | Line 269 compound.go sets `compCtx.SkipOwnerSeqid = true`; manager.go lines 353, 424, 541, 642, 803, 989, 1088 check `seqid == 0` bypass; TestCompound_V41_SkipOwnerSeqid verifies flag set |
| 6 | SEQUENCE implicitly renews v4.1 client lease | ✓ VERIFIED | Line 133 sequence_handler.go calls `StateManager.RenewV41Lease(sess.ClientID)`; manager.go lines 1886-1907 implement lease renewal |
| 7 | Prometheus metrics track SEQUENCE operations (total, errors by type, replay hits, slot utilization, cache memory) | ✓ VERIFIED | sequence_metrics.go provides 5 metrics; sequence_handler.go lines 37, 43, 57, 76-77, 85, 101-109, 130 record metrics at all code paths |
| 8 | Minor version range is configurable per NFS adapter via control plane (REST API and dfsctl) | ✓ VERIFIED | adapter_settings.go lines 67-68, 103-116 define fields and getters; compound.go lines 67-74 gates on range; dfsctl settings.go wires CLI flags |
| 9 | v4.0 clients continue working unchanged through the bifurcated dispatcher | ✓ VERIFIED | `dispatchV40()` unchanged (line 93-189 compound.go); TestCompound_V40_Regression 8 subtests pass; TestCompound_V40_V41_Coexistence verifies no interference |
| 10 | Concurrent v4.0 and v4.1 COMPOUNDs on the same TCP connection work without interference | ✓ VERIFIED | TestCompound_ConcurrentMixedTraffic (10 goroutines × 100 ops) passes with -race; TestCompound_V40_V41_Coexistence verifies alternating versions |
| 11 | v4.0 OPEN/CLOSE/LOCK seqid validation still works correctly (no bypass regression) | ✓ VERIFIED | TestCompound_V40_Regression/SkipOwnerSeqid_is_false verifies flag remains false for v4.0; manager.go seqid=0 bypass only triggered from v4.1 path |

**Score:** 11/11 truths verified

### Required Artifacts

| Artifact | Expected | Status | Details |
|----------|----------|--------|---------|
| `internal/protocol/nfs/v4/handlers/sequence_handler.go` | SEQUENCE handler implementation with session lookup, slot validation, lease renewal, status flags | ✓ VERIFIED | 209 lines (exceeds min 80); `handleSequenceOp()` with all required logic; `isSessionExemptOp()` helper |
| `internal/protocol/nfs/v4/handlers/compound.go` | dispatchV41 with SEQUENCE gating, exempt op detection, replay cache at COMPOUND level | ✓ VERIFIED | Contains `isSessionExemptOp` pattern; dispatchV41 lines 208-386 implements full SEQUENCE gating flow |
| `internal/protocol/nfs/v4/types/types.go` | SkipOwnerSeqid flag on CompoundContext for v4.1 seqid bypass | ✓ VERIFIED | Line 127 defines `SkipOwnerSeqid bool` field with doc comment lines 122-126 |
| `internal/protocol/nfs/v4/state/manager.go` | RenewV41Lease and GetStatusFlags methods on StateManager | ✓ VERIFIED | Contains `RenewV41Lease` pattern; lines 1886-1907 RenewV41Lease; lines 1909-1948 GetStatusFlags |
| `internal/protocol/nfs/v4/handlers/sequence_handler_test.go` | Table-driven SEQUENCE validation edge case tests | ✓ VERIFIED | 909 lines (exceeds min 100); 15 SEQUENCE validation tests + 11 COMPOUND dispatch tests + 3 benchmarks |
| `internal/protocol/nfs/v4/state/sequence_metrics.go` | SequenceMetrics with nil-safe receivers following SessionMetrics pattern | ✓ VERIFIED | 121 lines (exceeds min 60); 5 metrics, nil-safe receivers |
| `pkg/controlplane/models/adapter_settings.go` | V4MinMinorVersion and V4MaxMinorVersion fields on NFSAdapterSettings | ✓ VERIFIED | Contains `V4MinMinorVersion` pattern; lines 67-68 define fields, lines 103-116 getters |
| `internal/protocol/nfs/v4/handlers/compound_test.go` | v4.0 regression tests and coexistence tests | ✓ VERIFIED | Contains `TestCompound_V40_Regression` pattern; line 780 starts regression suite |

### Key Link Verification

| From | To | Via | Status | Details |
|------|----|----|--------|---------|
| compound.go | sequence_handler.go | handleSequenceOp called as first op in dispatchV41 | ✓ WIRED | Line 247 compound.go calls `h.handleSequenceOp(compCtx, reader)` |
| sequence_handler.go | manager.go | GetSession, RenewV41Lease, GetStatusFlags | ✓ WIRED | Lines 52, 133, 136 call StateManager methods matching pattern |
| sequence_handler.go | slot_table.go | ValidateSequence and CompleteSlotRequest | ✓ WIRED | Line 66 calls `ValidateSequence`, CompleteSlotRequest called via defer in compound.go line 274-280 |
| compound.go | types.go | SkipOwnerSeqid set on CompoundContext in v4.1 dispatch path | ✓ WIRED | Line 269 compound.go sets `compCtx.SkipOwnerSeqid = true` |
| sequence_handler.go | sequence_metrics.go | SequenceMetrics recording on SEQUENCE handler success/error/replay | ✓ WIRED | Lines 37, 43, 57, 76-77, 85, 101-109, 130 call sequenceMetrics methods |
| compound.go | adapter_settings.go | Minor version range gating in ProcessCompound | ✓ WIRED | Lines 67-73 compound.go check `h.minMinorVersion` and `h.maxMinorVersion` set from NFSAdapterSettings |

### Requirements Coverage

| Requirement | Source Plan | Description | Status | Evidence |
|-------------|-------------|-------------|--------|----------|
| SESS-04 | 20-01 | Server handles SEQUENCE as first operation in every v4.1 COMPOUND with slot validation and lease renewal | ✓ SATISFIED | handleSequenceOp validates session/slot/seqid; RenewV41Lease called on every success; dispatchV41 enforces SEQUENCE-first |
| COEX-01 | 20-01 | COMPOUND dispatcher routes minorversion=0 to existing v4.0 path and minorversion=1 to v4.1 path | ✓ SATISFIED | Lines 76-88 compound.go switch on minorVersion; dispatchV40 and dispatchV41 are separate functions |
| COEX-02 | 20-02 | v4.0 clients continue working unchanged when v4.1 is enabled | ✓ SATISFIED | TestCompound_V40_Regression passes 8 subtests; SkipOwnerSeqid remains false for v4.0; dispatchV40 unchanged |
| COEX-03 | 20-01 | Per-owner seqid validation bypassed for v4.1 operations (slot table provides replay protection) | ✓ SATISFIED | SkipOwnerSeqid flag set to true in v4.1 path; manager.go treats seqid=0 as bypass signal; 7 call sites updated |

**Coverage:** 4/4 requirements satisfied (100%)

### Anti-Patterns Found

| File | Line | Pattern | Severity | Impact |
|------|------|---------|----------|--------|
| N/A | N/A | N/A | N/A | No anti-patterns detected |

**Summary:** No TODO, FIXME, placeholder comments, or stub implementations found in modified files. All handlers are fully implemented with proper error handling, logging, and metrics.

### Human Verification Required

None. All observable behaviors are programmatically verifiable via unit tests and code inspection.

---

## Detailed Verification

### Plan 01: SEQUENCE Handler and Bifurcation

**Commits:**
- `3439e506` - SEQUENCE handler, lease/status methods, seqid bypass (370 insertions)
- `61f79bea` - dispatchV41 SEQUENCE gating with replay cache and tests (998 insertions)

**Verification:**

1. **handleSequenceOp implementation** (sequence_handler.go):
   - ✓ Decodes SequenceArgs (line 40-49)
   - ✓ Looks up session via StateManager.GetSession (line 52)
   - ✓ Validates slot/seqid via ValidateSequence (line 66)
   - ✓ Handles SeqRetry by returning cached reply (line 69-79)
   - ✓ Handles SeqMisordered with error classification (line 92-122)
   - ✓ Handles SeqNew: builds V41RequestContext, renews lease, computes status flags (line 124-179)
   - ✓ Records metrics at every path (lines 37, 43, 57, 76-77, 85, 101-109, 130)

2. **dispatchV41 SEQUENCE gating** (compound.go):
   - ✓ Reads first opcode (line 224)
   - ✓ Checks isSessionExemptOp (line 230)
   - ✓ Enforces SEQUENCE-first for non-exempt ops (line 239-243)
   - ✓ Calls handleSequenceOp (line 247)
   - ✓ Returns cached reply on replay (line 253-256)
   - ✓ Sets SkipOwnerSeqid=true (line 269)
   - ✓ Uses defer to release slot (line 272-282)
   - ✓ Caches response bytes (line 382)

3. **StateManager lease/status methods** (manager.go):
   - ✓ RenewV41Lease updates LastRenewal (lines 1886-1907)
   - ✓ GetStatusFlags computes SEQ4_STATUS bitmask (lines 1909-1948)
   - ✓ seqid=0 bypass at 7 ValidateSeqID call sites (lines 353, 424, 541, 642, 803, 989, 1088)

4. **Test coverage** (sequence_handler_test.go):
   - ✓ 15 SEQUENCE validation tests (new request, bad session, bad slot, misordered, replay with/without cache, bad XDR)
   - ✓ 11 COMPOUND dispatch tests (SEQUENCE+ops, exempt ops, non-exempt without SEQUENCE, SEQUENCE at position >0, slot release on error, sequential/multiple slots)
   - ✓ 3 benchmarks (SEQUENCE validation, v4.1 dispatch, v4.0 dispatch baseline)

### Plan 02: Metrics, Version Range, and Testing

**Commits:**
- `3c49fe05` - SequenceMetrics and minor version range configuration (121 insertions)
- `9f741896` - v4.0 regression, coexistence, concurrent, version range tests (852 insertions)

**Verification:**

1. **SequenceMetrics** (sequence_metrics.go):
   - ✓ 5 metrics: SequenceTotal, ErrorsTotal (with labels), ReplayHitsTotal, SlotsInUse (with session_id label), ReplayCacheBytes
   - ✓ Nil-safe receivers on all methods (lines 84-121)
   - ✓ Follows exact SessionMetrics pattern (NewSequenceMetrics constructor, MustRegister in NewSequenceMetrics)
   - ✓ Wired into sequence_handler.go at all code paths

2. **Minor version range configuration**:
   - ✓ Model fields: V4MinMinorVersion/V4MaxMinorVersion in adapter_settings.go (lines 67-68)
   - ✓ Getters: GetV4MinMinorVersion/GetV4MaxMinorVersion (lines 103-116)
   - ✓ Default values: 0 and 1 (line 184-185)
   - ✓ ProcessCompound gating: lines 67-74 compound.go check before switch
   - ✓ Handler fields: minMinorVersion/maxMinorVersion on Handler struct
   - ✓ Full stack: REST API (adapter_settings.go), apiclient (adapter_settings.go), dfsctl CLI (settings.go)

3. **v4.0 regression tests** (compound_test.go line 780+):
   - ✓ TestCompound_V40_Regression with 8 subtests:
     - PUTROOTFH_GETATTR (v4.0 ops still work)
     - SkipOwnerSeqid_is_false (flag not set for v4.0)
     - BlockedOperation (v4.0 blocked ops still return NOTSUPP)
     - UnknownOpcode (v4.0 unknown ops still return OP_ILLEGAL)
     - EmptyCompound (v4.0 empty compound still succeeds)
     - IllegalOp (v4.0 illegal op handling unchanged)
     - MultiOpStopOnError (v4.0 error handling unchanged)
     - OpCountLimit (v4.0 count limit unchanged)

4. **Coexistence tests** (compound_test.go):
   - ✓ TestCompound_V40_V41_Coexistence with 4 subtests:
     - Sequential v4.0 then v4.1 on same handler
     - v4.1 SEQUENCE + v4.0 ops via fallback
     - SkipOwnerSeqid=true in v4.1 COMPOUND
     - Alternating v4.0/v4.1 versions

5. **Concurrent mixed traffic test** (compound_test.go):
   - ✓ TestCompound_ConcurrentMixedTraffic: 10 goroutines × 100 ops with random version selection
   - ✓ No data races (test passes with -race flag)

6. **Version range gating tests** (compound_test.go):
   - ✓ TestCompound_VersionRangeGating with 4 subtests:
     - v4.1-only mode (min=1, max=1)
     - v4.0-only mode (min=0, max=0)
     - Default both enabled (min=0, max=1)
     - Out-of-range rejection (NFS4ERR_MINOR_VERS_MISMATCH)

7. **Protocol CLAUDE.md documentation** (internal/protocol/CLAUDE.md):
   - ✓ SEQUENCE-gated dispatch pattern documented
   - ✓ isSessionExemptOp convention explained
   - ✓ Seqid bypass via SkipOwnerSeqid flag documented
   - ✓ Replay cache at COMPOUND level explained
   - ✓ SequenceMetrics pattern documented
   - ✓ Minor version range configuration documented

---

## Verification Evidence Summary

**All must-haves verified:** 11/11 truths, 8/8 artifacts, 6/6 key links, 4/4 requirements

**Test execution:**
```bash
$ go test -run TestSequence_NewRequest_Success ./internal/protocol/nfs/v4/handlers/...
ok  	github.com/marmos91/dittofs/internal/protocol/nfs/v4/handlers	0.398s

$ go test -run TestCompound_V40_Regression ./internal/protocol/nfs/v4/handlers/...
ok  	github.com/marmos91/dittofs/internal/protocol/nfs/v4/handlers	0.541s

$ go test -run TestCompound_V40_V41_Coexistence ./internal/protocol/nfs/v4/handlers/...
ok  	github.com/marmos91/dittofs/internal/protocol/nfs/v4/handlers	0.392s
```

**Build verification:**
```bash
$ go build ./...
# Success - entire project compiles

$ go vet ./...
# Success - no static analysis issues
```

**Commit verification:**
- ✓ All 4 commits present in git history (3439e506, 61f79bea, 3c49fe05, 9f741896)
- ✓ Commits match SUMMARY.md claims
- ✓ File modification counts match plan expectations

---

_Verified: 2026-02-21T19:45:00Z_
_Verifier: Claude (gsd-verifier)_
