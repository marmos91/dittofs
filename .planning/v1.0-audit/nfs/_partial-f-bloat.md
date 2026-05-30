# NFS adapter — bloat / abstraction / boilerplate hunt (partial F)

Structural/bloat audit (not correctness — that's REVIEW.md). All paths verified by direct read.

## Where the code actually lives (brief's seed paths corrected)

The NFS adapter is split across **two** trees; the bulk is `internal/adapter/nfs/`. The brief's seed paths (`pkg/adapter/nfs/v3/doc.go:44`, `auth_helper.go:52`, `v4/helpers.go:24`) are all under `internal/adapter/nfs/...`, not `pkg/`. `pkg/adapter/nfs/` is only the thin lifecycle/dispatch shell.

| area | files | src LOC |
|---|---|---|
| `pkg/adapter/nfs` (+`identity/`) | 15 | ~3,078 |
| `internal/adapter/nfs` (all) | ~190 | ~53,000 |
| **NFS adapter total (src, non-test)** | **~205** | **~56,000** |

Biggest sub-areas: `v3/handlers` 52 files/12,574 LOC · `v4/state` 17/7,690 · `v4/handlers` 36/6,557 · `v4/types` 32/6,544 · `rpc/gss` 7/2,386.

**Verified seed (the 4 auth builders, real file:line):**
- `BuildAuthContextWithMapping` — `internal/adapter/nfs/v3/handlers/auth_helper.go:53`
- `GetCachedAuthContext` — `internal/adapter/nfs/v3/handlers/doc.go:44`
- `buildAuthContextWithWCCError` — `internal/adapter/nfs/v3/handlers/doc.go:130`
- `buildV4AuthContext` — `internal/adapter/nfs/v4/handlers/helpers.go:24`

All funnel to `Registry.ApplyIdentityMapping`. Archetype confirmed. `v3/doc.go` (top of the v3 package) is genuinely a 1-line `package v3` file; the logic-bearing misnamed file is **`v3/handlers/doc.go`** (189 LOC of auth/cache/WCC helpers in a file called "doc").

---

## Useless interfaces

This package is **lighter on the "interface over one concrete type" disease than the brief implied** — most named interfaces have a real import-cycle or multi-impl justification. Verified impl/caller counts (repo-wide, non-test):

| interface | file:line | methods | impls | non-test refs | verdict |
|---|---|---|---|---|---|
| `V4Dispatcher` | `internal/adapter/nfs/dispatch.go:133` | 1 | 1 | 3 | **KEEP (load-bearing)** — explicitly avoids an import cycle (`internal/adapter/nfs` ← `pkg/adapter/nfs`). The concrete impl is in `pkg`, can't be referenced from `internal`. Documented at :131-132. |
| `NLMDispatcher`/`NSMDispatcher`/`PortmapDispatcher` | `dispatch.go:140/146/152` | 1 each | 1 each | 3 each | **KEEP** — same cross-tree cycle break. These look like the classic "4 one-method dispatcher interfaces" smell but are structurally required by the pkg/internal split. Not removable without merging the two packages. |
| `fileChecker` | `pkg/adapter/nfs/nlm.go:25` | 1 | 1 | 13 | **KEEP** — documented import-cycle avoidance (adapter ↔ pkg/metadata). |
| `Releaser` | `v3/handlers/nfs_response.go:52` | 1 | several | 7 | **KEEP** — buffer-release seam used by the generic responder. |
| `rpcRequest`/`rpcResponse` | `internal/adapter/nfs/helpers.go:9/40` | type-union | n/a | 2/3 | **see Abstractions #2** — generic constraints; the union is huge but the helper is used **once**. |
| `XdrEncoder`/`XdrDecoder` | `xdr/core/union.go:14/20` | 1 each | many | 3 each | **KEEP** — real XDR polymorphism. |
| `IdentityMapper` | `pkg/adapter/nfs/identity/mapper.go:32` | 1 | **4** (Static/Cached/Convention/Table) | 23 | **KEEP** — genuine 4-impl strategy. |
| `MappingStore` | `identity/table.go:30` | 4 | DB-backed | 8 | **KEEP** — DB seam. |
| `GroupResolver` | `identity/mapper.go:75` | 2 | 0 wired | 4 | **borderline DELETE** — only referenced in a "can be extended when GroupResolver implements…" comment + its own def; **no impl, no live caller**. Future-proofing interface. DELETE until ACL group-membership lands. ~10 LOC. |
| `PortmapRegistry` | `portmap/handlers/handler.go:16` | 4 | 1 | 5 | **borderline KEEP** — one impl, clean test seam. |

**Net: there is no clear "delete this useless interface" win here except `GroupResolver`** (unimplemented). The 4 `*Dispatcher` interfaces *look* like the brief's target but are load-bearing cycle-breaks — flag, don't cut.

---

## Unneeded abstractions

1. **Four auth-context builders for one concern** (seed archetype, confirmed). All wrap `Registry.ApplyIdentityMapping`. Real variation is only: cache hit/miss (`GetCachedAuthContext`), v3-WCC-error return shape (`buildAuthContextWithWCCError`), v4 vs v3 context type (`buildV4AuthContext`/`BuildAuthContextWithMapping`). The bodies of `BuildAuthContextWithMapping` (auth_helper.go) and `buildV4AuthContext` (v4 helpers.go) are **near-line-for-line duplicates** (same authMethod mapping, same `originalIdentity` build, same `ApplyIdentityMapping`, same effective-AuthContext assembly) differing only in context type and error-vs-fallback handling. **MERGE** to one core `buildAuthContext(ctx, creds, share)` + thin v3-WCC and v4 wrappers + the cache layer on top. Est. **−80 to −120 LOC** and kills the documented "4 ways" smell. Highest-value dedup.

2. **`handleRequest[Req,Resp]` generic enumerates 28 concrete request + 28 response types in two giant type-unions (`internal/adapter/nfs/helpers.go:9-71`) but is called exactly ONCE** (verified: 1 call site). A 60-line generic constraint list maintained for one caller is negative ROI — every new v3 op must be added to both unions. **Either** route all v3 dispatch through it (then the constraint earns its keep — see Duplicated #3) **or** replace it with a plain `func(...)` interface (`{ Encode() ([]byte,error); GetStatus() uint32 }`) and drop the 56-line union. Current state is the worst of both. **MERGE/SIMPLIFY.**

3. **`v3/handlers/doc.go` holds logic, not docs** — 189 LOC containing `GetCachedAuthContext`, `getFileOrError`, `buildAuthContextWithWCCError`, `getOplockBreaker`, `checkMFsymlinkByHandle`. A file named `doc.go` that's actually the v3 helper grab-bag is a discoverability trap. **RENAME → `helpers.go`** (cosmetic, high readability).

4. **`handleClientCrash` no-op stub** (`pkg/adapter/nfs/nlm.go:479`) — ~50 LOC that returns hard-coded 0 (also REVIEW H14). As structure: dead-on-arrival code. **DELETE or implement.**

5. **`encodeChangeInfo4` / `getMetadataServiceForCtx`** (v4 helpers.go) — trivial 3-line wrappers; fine, but `getMetadataServiceForCtx` duplicates the nil-registry guard already in `buildV4AuthContext`. Minor.

---

## Duplicated-N-ways

1. **Auth-context building — 4 implementations** (see Abstractions #1). Top target.

2. **The v3 handler/codec file split — 22 `*_codec.go` siblings** (`v3/handlers`, 3,699 LOC total in codec files). Every op is two files: `getattr.go` (handler + request/response structs + validation) and `getattr_codec.go` (Decode + Encode). Verified on getattr: the codec file is a struct-free Decode/Encode pair, ~120 LOC, mostly RFC-doc comments + example blocks. The split forces a two-file jump per operation and the encode/decode is mechanical. **MERGE each `_codec.go` into its handler.** File-count cut: **−22 files** in v3/handlers, LOC roughly flat. Biggest single navigability win for the v3 path.

3. **Per-handler skeleton repeated ~22× (v3) + ~30× (v4).** Each handler open-codes: cancel-check → log → validate → `getMetadataService` guard → build-authctx → resolve-handle → op → convert-attrs → encode. The generic `handleRequest` factors the decode/encode/error envelope but is used once (#2). WCC pre/post-op capture is open-coded in every mutating v3 handler. The v4 handlers each repeat the `&types.CompoundResult{Status,OpCode,Data:encodeStatusOnly(...)}` triple 3-5× per file (visible in putfh.go, savefh.go, delegreturn.go). **Add a `compoundErr(opCode, status)` helper** (collapses ~4 lines → 1 at hundreds of sites) and route v3 through the generic envelope + a shared `captureWCC`. Conservative net after helpers: **−300 to −500 LOC**, large maintainability gain.

4. **Error→status mapping is already consolidated** in `internal/adapter/common/errmap.go` (single source of truth). This is the *opposite* of bloat — good prior work. **KEEP.** Remove any inline `mapMetadataErrorToNFS` stragglers in individual handlers that errmap superseded (REVIEW notes create.go had a partial one).

---

## Boilerplate

1. **The `&types.CompoundResult{Status:…, OpCode:…, Data: encodeStatusOnly(…)}` literal** appears ~150+ times across the 36 v4 handler files — it's the entire body of every error branch and every trivial op. **Replace with `compoundErr(op, status)` + `compoundOK(op, data)` helpers.** Pure boilerplate; ~−200 to −400 LOC of literal noise, big readability win.

2. **`v4/types` — 32 files, 13 under 150 LOC, each one DTO + Encode/Decode/String.** Includes **9 `cb_*.go` callback DTO files (1,482 LOC)** for pNFS/callback wire types, several for features that are stub-only (the dispatch table shows GETDEVICEINFO/LAYOUT*/SET_SSV etc. are `v41StubHandler` → NOTSUPP). **MERGE** into ~6 op-family files (session, callback, layout, deviceid, delegation, misc). File-count: **32 → ~6 (−26 files)**, LOC flat. This is the real "over-split" the brief sensed — it's here, not in a `pkg/.../v4/types` (which doesn't exist).

3. **73 `String()` methods in `v4/types`.** Verified: there are only **56 `.String()` call sites** in the whole non-test adapter, and most of those are *nested* (a parent `String()` calling a child's `String()` — e.g. `cb_sequence.go` calling `SessionID.String()`). The leaf debug-only `String()`s on top-level Args/Res structs (DestroyClientidRes, FreeStateidRes, etc.) have **no production caller** — they exist for test/debug symmetry. **DELETE the unreferenced leaf ones** (keep `SessionID`/`Stateid`/bitmap `String()`s that are genuinely composed into logs). Est. **−300 to −500 LOC.**

4. **Trivial v4 filehandle-family op files** — `savefh.go` (33), `restorefh.go` (33), `putrootfh.go` (26), `putpubfh.go` (26), `getfh.go` (36), `null.go` (10) are each a few lines. **MERGE into one `filehandle.go`.** ~−5 files.

5. **`NFSResponseBase` embedding + 28-entry rpcResponse union** — the base struct is good (dedups Status field across 20+ responses), but pairing it with a hand-maintained 28-type union (#2) is the boilerplate tax.

---

## AI-bloat & comments

- Comment density `internal/adapter/nfs` ≈ **36%** — *high*. Much is legitimate (RFC §refs), but there's measurable over-documentation: every codec file carries a full `// Example:` block with fake `data := []byte{...}` snippets (see getattr_codec.go:33-42, 84-96) restating how to call Encode/Decode. These example blocks are AI-style filler on mechanical functions. **Trim the `// Example:` blocks** across the ~22 codec files + DTO files. Est. **−300 to −600 comment LOC**, pure noise removal.
- **Planning/conformance leakage: 0 in `internal/adapter/nfs`** (clean — grep found none). The `ADAPT-03`/`gotcha #` refs are in `internal/adapter/common/errmap.go` (shared, out of pure-NFS scope; flag to common owner).
- Step-banner comments (`// ===== Step 1: Validate =====`) in v3 handlers are heavy but arguably aid the skeleton; low priority.

---

## File layout (god-files + over-split)

**God-files (split by concern):**
- `internal/adapter/nfs/v4/state/manager.go` — **2,871 LOC, 79 funcs**: the single biggest file; holds client + session + open-owner + lock-owner + stateid + replay state. **SPLIT** (clients / sessions / owners / stateid-replay). Top god-file, and the locus of REVIEW H2-H8.
- `rpc/gss/framework.go` (831), `v4/types/constants.go` (805), `v4/types/session_common.go` (757) — large but cohesive constant/framework files. **borderline KEEP.**
- `v4/handlers/open.go` (725), `v4/attrs/encode.go` (725), `v3/handlers/create.go` (666), `dispatch_nfs.go` (639) — legitimately complex op logic. **KEEP.**

**Over-split (merge):**
- `v4/types` 32 → ~6 op-family (−26). **#1 over-split.**
- `v3/handlers` 22 `_codec.go` → fold into handlers (−22). **#2 over-split.**
- v4 filehandle-family op files → one file (−5).

**The user's stated pain (lifecycle + routing):** the path is `pkg/adapter/nfs/dispatch.go` (267) → `DispatchDeps` w/ 4 `*Dispatcher` interfaces → `internal/adapter/nfs/dispatch.go` (451) → `dispatch_nfs.go`/`dispatch_mount.go` → handler. The 4 interfaces are cycle-breaks (can't simply inline). The real simplification lever is **collapsing the pkg/internal split for dispatch** (out of this audit's cut-scope; flag as a design item) and the `compoundErr` + auth-builder merges that reduce per-op noise.

---

## Dead code

Verified by repo-wide non-test grep:
- **`ApplySetAttrs`** — `internal/adapter/nfs/xdr/attributes.go:214` — exported, non-method, **zero non-test callers** repo-wide. **DELETE** (~10 LOC).
- **`GroupResolver` interface** — `identity/mapper.go:75` — no impl, no live caller (only a "can be extended" comment). **DELETE** (~10 LOC).
- **Unreferenced leaf `String()` methods in `v4/types`** — biggest dead bucket (see Boilerplate #3), **−300 to −500 LOC.**
- **`handleClientCrash` no-op** (nlm.go:479) — implement or delete (~50 LOC).
- Single-caller exported helpers with 0 test refs (`NLMStatusToString`, `NSMStatusToString`, `GetIdentityMapper`/`SetIdentityMapper` package globals, `ExtractFileIDFromHandle`) — **unexport or inline**; verify each (some wired via dispatch maps). ~50-80 LOC.
- Package-global mutable `identityMapper` + `Set/GetIdentityMapper` in `v4/attrs/encode.go` — singleton-via-global anti-pattern, one setter site. **Pass explicitly** instead. Not dead, but smell.

No unimplemented/panic dispatch entries (the v41 stubs are intentional NOTSUPP).

---

## Quantified bloat budget

**Removable LOC (conservative net):**
- Delete unreferenced `v4/types` `String()` methods: **−300 to −500**
- `compoundErr`/`compoundOK` helper across v4 handlers: **−200 to −400**
- Trim `// Example:` / over-doc blocks in codec + DTO files: **−300 to −600** (comments)
- Handler-skeleton envelope + WCC helper (v3), after helpers: **−200 to −400**
- Auth-builder 4→1 merge: **−80 to −120**
- v4/types DTO file merges (trim during merge): **−100 to −200**
- Dead funcs (`ApplySetAttrs`, `GroupResolver`, single-caller helpers, no-op `handleClientCrash`): **−80 to −150**
- `rpcResponse`/`rpcRequest` union simplification: **−50**

**Total estimated removable: ~1,300–2,400 LOC** (≈2.5–4% of ~56K, concentrated in the hardest-to-navigate spots; comment-trim is a big chunk).

**File-count delta: ~−53 files** — v4/types 32→~6 (−26), v3 `_codec.go` fold (−22), v4 filehandle-family (−5). ~205 src files → **~152**.

### Top-5 highest-leverage cuts for "easier to follow / maintain"

1. **Fold the 22 `v3/handlers/*_codec.go` files into their handlers** — one operation = one file. Kills the constant two-file jump that makes the v3 path hard to read. (−22 files.)
2. **Collapse `v4/types` 32→~6 op-family files and delete the unreferenced leaf `String()` methods.** Biggest combined LOC + file-count win, zero behavior change. (−26 files, −300..500 LOC.)
3. **Merge the 4 auth-context builders into one core + thin wrappers**, and **rename the logic-bearing `v3/handlers/doc.go` → `helpers.go`.** Directly addresses the seed archetype + a discoverability trap.
4. **Add `compoundErr`/`compoundOK` helpers** to kill the ~150+ repeated `CompoundResult{…encodeStatusOnly…}` literals in v4 handlers, and route v3 through the existing `handleRequest` envelope + a `captureWCC` helper.
5. **Split the 2,871-LOC `v4/state/manager.go`** by concern — biggest, hardest file and the locus of most v4 correctness bugs.

**Caveats / load-bearing (do NOT delete):** the 4 `*Dispatcher` interfaces and `fileChecker` are import-cycle breaks (look like bloat, are structural). `IdentityMapper`/`MappingStore` have real multiple impls / DB seams. `XdrEncoder/Decoder`, the `errmap.go` consolidation, and `NFSResponseBase` embedding are good prior cleanups — keep. The big XDR/attr encoders (`encode.go`, `open.go`, `create.go`) are complex-but-justified — split at most, never delete. The errmap `ADAPT-/gotcha-#` comments live in shared `internal/adapter/common` — coordinate before stripping.
