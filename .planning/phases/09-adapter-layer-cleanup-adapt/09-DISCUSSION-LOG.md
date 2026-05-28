# Phase 09: Adapter layer cleanup (ADAPT) — Discussion Log

> **Audit trail only.** Do not use as input to planning, research, or execution agents.
> Decisions are captured in `09-CONTEXT.md` — this log preserves the alternatives considered.

**Date:** 2026-04-24
**Phase:** 09-adapter-layer-cleanup-adapt
**Areas discussed:** common/ package shape, Error-mapping table design, SMB READ pool lifecycle, []BlockRef groundwork scope + ADAPT-05, Commit/PR strategy, Auth context flow, Docs updates

---

## common/ package shape

### Q1: Input — runtime vs narrow interface

| Option | Description | Selected |
|--------|-------------|----------|
| Narrow interface | Define `BlockStoreRegistry` interface; helpers take it. Testable without booting runtime, no circular import risk. | ✓ |
| *runtime.Runtime directly | Helpers take `*runtime.Runtime`. Simpler at call site but couples common/ to runtime. | |
| Both — interface + *Runtime shim | Interface in common/ but each protocol keeps its own runtime shim. Partial dedup only. | |

**User's choice:** Narrow interface (Recommended)
**Notes:** Drives D-01 in CONTEXT.md.

### Q2: ResolveForRead/Write scope

| Option | Description | Selected |
|--------|-------------|----------|
| Resolve-only | Fetch file + block store; callers invoke PrepareRead/Write themselves. | ✓ |
| Full resolve+permission bundle | Helpers call PrepareRead/Write too; wider return struct. | |
| Separate helper tiers | ResolveBlockStore (thin) + PrepareReadWithBlockStore (bundled). Two overlapping APIs. | |

**User's choice:** Resolve-only (Recommended)
**Notes:** Drives D-02. Protocols consume PrepareRead/Write return shapes differently; bundling them forces awkward widest-common shape.

### Q3: readFromBlockStore return shape

| Option | Description | Selected |
|--------|-------------|----------|
| Keep Release() pattern | Move NFSv3's blockReadResult+Release() to common/ verbatim. | ✓ |
| Accept caller-provided buffer | Caller calls pool.Get; helper just does ReadAt. More boilerplate per call site. | |
| io.ReadCloser-style | Reader-backed with Close() returning buffer. Extra wrapper layer. | |

**User's choice:** Keep Release() pattern (Recommended)
**Notes:** Drives D-04. Already correct in NFSv3; SMB READ adopts unchanged.

### Q4: NFSv4 parity

| Option | Description | Selected |
|--------|-------------|----------|
| All three (v3 + v4 + SMB) now | Full migration in Phase 09; matches ROADMAP. | ✓ |
| v3 + SMB only; v4 deferred | Lower blast radius but diverges from ROADMAP. | |
| v4 migration but error-table excludes it | Helpers yes, NFS4ERR_* codes stay local. | |

**User's choice:** All three (v3 + v4 + SMB) now (Recommended)
**Notes:** Drives D-05. Prevents Phase 12 from having to do adapter dedup.

---

## Error-mapping table design

### Q1: Table structure

| Option | Description | Selected |
|--------|-------------|----------|
| Struct-per-code table | `map[ErrorCode]struct{NFS3; NFS4; SMB}` — one row per code, all protocols on same line. | ✓ |
| Two function-driven mappers, one enum | Shared enum, separate MapToNFS3/MapToSMB functions with own switches. | |
| Struct table + generated lookup fns | Codegen from struct table. Micro-optimization overkill. | |

**User's choice:** Struct-per-code table (Recommended) — with inline preview
**Notes:** Drives D-06. Literally satisfies "one edit, not two" — type system forces all three columns on add.

### Q2: NFSv4 in table

| Option | Description | Selected |
|--------|-------------|----------|
| Include NFSv4 now | Fold NFS4ERR_* codes into the same table. | ✓ |
| NFS3 + SMB only | NFSv4 keeps own mapping; future phase adds column. | |
| NFSv4 reuses NFS3 column via conversion | ConvertNFS3ToNFS4 wrapper. Double-hop debug traces. | |

**User's choice:** Include NFSv4 now (Recommended)
**Notes:** Drives D-07. Consistent with D-05 (all three protocols migrating).

### Q3: Non-metadata error translators in scope

| Option | Description | Selected |
|--------|-------------|----------|
| Out of scope | metadata errors only per ADAPT-03's literal wording. | |
| In scope — consolidate all error translators | Fold everything adapter-layer. | ✓ |
| metadata + block-store errors only | Middle ground. | |

**User's choice:** In scope — consolidate all error translators
**Notes:** User expanded scope beyond the Recommended default. Follow-up question pinned the boundary (see Q4).

### Q4: Which translators (follow-up after scope expansion)

| Option | Description | Selected |
|--------|-------------|----------|
| Meta + content + lock | Three translator families; excludes auth/signing (those are wire failures). | ✓ |
| Everything adapter-layer calls | Full audit — risk of pulling in wire-level translations. | |
| Meta + content only; lock stays local | Avoids awkward lock-code asymmetries. | |

**User's choice:** Meta + content + lock (Recommended)
**Notes:** Drives D-08. Fixed boundary: auth/signing errors stay out.

---

## SMB READ pool lifecycle

### Q1: Release point

| Option | Description | Selected |
|--------|-------------|----------|
| After response encoding | Handler returns response with pooled Data; encoder fires release closure post-SendResponse. | ✓ |
| Defer Put() in Read() handler | Simple but risky if response path is async. | |
| Reference-counted wrapper | Robust but heaviest; overkill absent confirmed async paths. | |

**User's choice:** After response encoding (Recommended)
**Notes:** Drives D-09. Matches NFSv3's existing model; correct by construction.

### Q2: Read variants in scope

| Option | Description | Selected |
|--------|-------------|----------|
| Regular file reads only | Hot path only; pipe/symlink stay as-is. | |
| All read variants | Uniform: file + pipe + symlink all pool. | ✓ |
| File + symlink (skip pipe) | Middle ground. | |

**User's choice:** All read variants
**Notes:** Drives D-10. User preferred uniformity over micro-optimization; prevents future contributors reintroducing inline alloc on non-hot paths.

### Q3: Over-cap behavior

| Option | Description | Selected |
|--------|-------------|----------|
| Direct allocation fallback | pool.Get already does this for size > LargeSize; no handler code change. | ✓ |
| Clamp to 1MB and chunk | Violates SMB2 wire semantics. | |
| Bump pool LargeSize to 8MB | Pins ~128MB idle on 16-core machines. | |

**User's choice:** "Help me choose here. What is the best option here?"
**Claude's recommendation (accepted):** Direct allocation fallback — reasoning:
- DittoFS advertises `MaxReadSize = 1048576` (1 MB) at `internal/adapter/smb/v2/handlers/handler.go:361`, matching the pool's LargeSize tier exactly. Over-cap path is dormant today.
- `pool.Get` (bufpool.go:157-162) already falls through to `make([]byte, size)` for size > LargeSize; `pool.Put` ignores oversized buffers — all handled transparently.
- Option 3 pins ~128MB of idle pool memory on a 16-core machine (sync.Pool is per-P) for an optimization that doesn't fire at current negotiated cap. Premature.
- Option 2 violates SMB2 wire semantics (client asked for N, server returns N or EOF — not chunked).

**Notes:** Drives D-11. Documentation note in common/ Godoc is the deliverable; no handler code change.

---

## []BlockRef groundwork scope + ADAPT-05

### Q1: ADAPT-04 shape

| Option | Description | Selected |
|--------|-------------|----------|
| Call-site refactor only | Extract NFS/SMB READ/WRITE into common/; Phase 12 wires Blocks fetch. | ✓ |
| Intermediate engine.ReadAtV2 shim | Add V2 method now; call sites pass nil until Phase 12. Two edits per call site. | |
| Defer ADAPT-04 entirely to Phase 12 | Honest about the field/signature dependency. | |

**User's choice:** "I would align to real SMB 3.1.1 and NFS3/4 behaviour. What is the best option?"
**Claude's recommendation (accepted):** Call-site refactor only — reasoning:
- SMB2/NFS3/NFS4 READ at the wire all carry `(offset, length)` — never block identity. `[]BlockRef` is internal DittoFS plumbing between adapter and engine, not a protocol concept.
- Option 1 keeps protocol call sites speaking `(offset, length)`; common/ becomes the seam where Phase 12 transparently adds `FileAttr.Blocks` fetch+slice without touching protocol handlers.
- Option 2 leaks internal abstraction up into handlers prematurely — every call site changes now (to pass nil) then again in Phase 12 (real blocks). Two edits > one.
- Option 3 loses groundwork value — Phase 12 has to do the call-site refactor anyway, just in a bigger phase.

**Notes:** Drives D-12. Protocol fidelity preserved; Phase 12 blast radius minimized.

### Q2: Cross-protocol conformance coverage

| Option | Description | Selected |
|--------|-------------|----------|
| Documented subset (~10 codes) | Common codes only; tractable. | |
| All 27 metadata.Err* codes | Full coverage; exotic codes need fixture plumbing. | ✓ |
| Table-driven per-row | Grow test with table; skip rows that lack fixtures. | |

**User's choice:** All 27 metadata.Err* codes
**Notes:** User chose exhaustive over Recommended subset.

### Q3: Test level

| Option | Description | Selected |
|--------|-------------|----------|
| E2E with real server + kernel clients | Extend cross_protocol_test.go; kernel-level fidelity. | ✓ |
| Adapter-handler unit test with mocked registry | Faster but misses wire framing. | |
| Both — unit for table, e2e for smoke cases | Belt-and-suspenders. | |

**User's choice:** E2E with real server + kernel clients (Recommended)
**Notes:** Combined with "All 27 codes" — triggered follow-up on exotic codes.

### Q4: Hard-to-trigger codes (follow-up)

| Option | Description | Selected |
|--------|-------------|----------|
| Triggerable at e2e + unit for rest | ~18 e2e + ~9 unit. Same table, two tiers. | ✓ |
| All 27 in e2e with fixture plumbing | Max fidelity, significantly larger fixture surface. | |
| Skip exotic codes with rationale | Document why some codes aren't asserted. | |

**User's choice:** Triggerable at e2e + unit for rest (Recommended)
**Notes:** Drives D-13 and D-14. Full coverage preserved; test surface stays tractable.

---

## Commit/PR strategy

### Q1: Slice strategy

| Option | Description | Selected |
|--------|-------------|----------|
| Two PRs: helpers+pool+errors, then []BlockRef+test | PR-A mechanical consolidation; PR-B call-site seam + conformance. | ✓ |
| Single PR with atomic per-ADAPT commits | One large PR, 5-7 commits. | |
| PR-A helpers, PR-B pool, PR-C errors, PR-D []BlockRef+test | Finest split; highest overhead. | |

**User's choice:** Two PRs (Recommended)
**Notes:** Drives D-15 and D-16.

---

## Auth context flow

### Q1: AuthContext plumbing

| Option | Description | Selected |
|--------|-------------|----------|
| AuthContext is a helper param | Explicit; helpers take `authCtx *metadata.AuthContext`. | ✓ |
| common/ accepts handler context wrapper | Couples common/ to protocol types; defeats D-01. | |
| Helper builds AuthContext itself | Doesn't exist today; violates explicit-deps. | |

**User's choice:** AuthContext is a helper param (Recommended)
**Notes:** Drives D-03. Honors CLAUDE.md invariant 2 at the helper boundary.

---

## Docs updates

### Q1: Which docs get updates (multi-select)

| Option | Description | Selected |
|--------|-------------|----------|
| docs/ARCHITECTURE.md | Directory map + Adapter-layer subsection mentioning common/. | ✓ |
| docs/NFS.md + docs/SMB.md | Protocol docs get "Error mapping" sections pointing to common/. | ✓ |
| docs/CONTRIBUTING.md | "Adding a new metadata.ErrorCode" recipe. | |
| README.md / CHANGELOG | No change for internal refactor; deferred. | |

**User's choice:** docs/ARCHITECTURE.md + docs/NFS.md + docs/SMB.md
**Notes:** Drives D-17. CONTRIBUTING.md recipe is Claude's discretion (planner may add if the touched-docs diff stays small).

---

## Claude's Discretion

Items left to the planner's judgment (see CONTEXT.md `<decisions>` §"Claude's Discretion"):

- Exact narrow-interface method set in common/
- Flat common/ package vs subfolders (errmap/, resolve/)
- Where `SMBResponseBase.ReleaseData func()` lives (or alternative encoder-level registry)
- Whether common/readFromBlockStore wraps or inlines NFSv3's current body
- Commit subdivision inside PR-A and PR-B beyond the per-ADAPT-NN floor
- Whether docs/CONTRIBUTING.md gets the ErrorCode recipe

## Deferred Ideas

Captured in CONTEXT.md `<deferred>`:
- Metrics/observability for adapter error translation + pool hit/miss
- Auth-flow refactor (signing, session setup, credential routing)
- `pool.LargeSize` bump to 8 MB
- Bumping `MaxReadSize` / `MaxWriteSize` / `MaxTransactSize`
- Consolidating auth/signing error paths into common/ table
- Non-`metadata.ErrorCode` error translations
