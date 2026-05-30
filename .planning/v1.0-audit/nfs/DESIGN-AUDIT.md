# Area 4 — NFS Adapter Design Audit

**Status**: DESIGN AUDIT COMPLETE
**Date**: 2026-05-30
**Scope**: `pkg/adapter/nfs/` (21 src files) + `internal/adapter/nfs/` (~250 files across all sub-packages). 56K production LOC. READ-ONLY.
**Input**: REVIEW.md (17 HIGH / 30 MED / 30 LOW correctness findings), plus direct code reading.
**Companion partials**: `_partial-f-bloat.md` (bloat), `_partial-g-perf.md` (profiling), `_partial-h-handlers2.md` (2nd-pass handlers), `_partial-i-versions.md` (version coexistence).

---

## 1. Current Architecture

### 1.1 Lifecycle

Connection birth-to-death follows a clean layered sequence:

```
[NFSAdapter.New()]
    ↓  applyDefaults / validate / build BaseAdapter
[NFSAdapter.SetRuntime(rt)]
    ↓  inject registry into v3.Handler + mount.Handler
    ↓  build pseudoFS, v4StateManager, v4Handler
    ↓  build blockingQueue, nlmHandler, nsmHandler
    ↓  initGSSProcessor (if Kerberos)
    ↓  subscribe rt.OnShareChange callbacks (pseudoFS rebuild + break callbacks)
    ↓  applyNFSSettings (lease, delegation, blocklist)
[NFSAdapter.Serve(ctx)]
    ↓  startPortmapper (goroutine)
    ↓  performNSMStartup
    ↓  StateManager.StartSessionReaper
    ↓  BaseAdapter.ServeWithFactory(ctx, NFSAdapter, preAcceptCheck, nil)
        ↓  TCP accept loop
        ↓  preAcceptCheck (live MaxConnections gate + applyNFSSettings)
        ↓  NFSAdapter.NewConnection(conn) → NFSConnection
        ↓  go NFSConnection.Serve(ctx)
[NFSConnection.Serve(ctx)]
    ↓  register with ClientRegistry
    ↓  reset idle deadline
    ↓  loop: readRequest → dispatchRequest (goroutine, bounded by requestSem)
[NFSConnection.readRequest]
    ↓  ReadFragmentHeader (internal/adapter/nfs/connection.go:40)
    ↓  ValidateFragmentSize (1.25 MB cap)
    ↓  ReadRPCMessage (pooled buffer)
    ↓  DemuxBackchannelReply (v4.1 MUX)
    ↓  rpc.ReadCall → *RPCCallMessage
[NFSConnection.processRequest]
    ↓  rpc.ReadData → procedureData
    ↓  handleRPCCall (pkg/adapter/nfs/dispatch.go)
[handleRPCCall]
    ↓  GSS interception (AuthFlavor==6, gssProcessor != nil)
    ↓  program switch: NFS / Mount / NLM / NSM / default
[NFSAdapter.Stop(ctx)]
    ↓  unsub shareUnsubscribers
    ↓  stopPortmapper / gssProcessor.Stop / kerberosProvider.Close
    ↓  BaseAdapter.Stop (close listener, cancel ctx, wg.Wait, force-close)
```

Key goroutine ownership: one `Serve` goroutine per connection. Within that, per-request goroutines bounded by `requestSem` (cap=`MaxRequestsPerConnection`, default 100). Replies serialized by `writeMu`. No goroutine leaks on shutdown — `wg.Wait()` in `handleConnectionClose` drains in-flight requests before TCP close.

The only non-obvious lifecycle coupling: v4 backchannel binding (`maybeRegisterBackchannel`) is checked after every COMPOUND on the connection's read-loop goroutine — correct but subtle. `pendingCBReplies` is set on the NFSConnection directly (no mutex), safe because only the Serve goroutine reads/writes it.

### 1.2 Routing Table — Actual Structure (not the documented abstraction)

There are **five separate dispatch tables plus one inline switch**, across three layers:

**Layer 1 — Program-level switch** (`pkg/adapter/nfs/dispatch.go:handleRPCCall:155`):
```
call.Program → ProgramNFS → handleNFSProcedure / handleNFSv4Procedure
              → ProgramMount → handleMountProcedure
              → ProgramNLM → handleNLMProcedure
              → ProgramNSM → handleNSMProcedure
              → default → PROC_UNAVAIL (wrong: should be PROG_UNAVAIL — LOW, REVIEW)
```
This switch is **duplicated**: `pkg/adapter/nfs/dispatch.go` has the inline switch in `handleRPCCall` (live path), AND `internal/adapter/nfs/dispatch.go` has a separate `Dispatch()` function (test path, NOT called by production). Two parallel dispatch entry points.

**Layer 2 — Per-protocol tables:**
- **2a NFS v3** (`dispatch_nfs.go:NfsDispatchTable`) — 22 entries; `func(ctx *NFSHandlerContext, handler *Handler, reg *runtime.Runtime, data []byte) (*HandlerResult, error)`
- **2b Mount** (`dispatch_mount.go:MountDispatchTable`) — 6 entries; mount context, shares `HandlerResult`
- **2c NLM** (`nlm/dispatch.go:NLMDispatchTable`) — 8 entries; own `HandlerResult` (has `NLMStatus`)
- **2d NSM** (`nsm/dispatch.go:NSMDispatchTable`) — 6 entries; **no `reg` param**, own result type
- **2e Portmap** (`portmap/dispatch.go:DispatchTable`) — 5 entries; **no context, no runtime, returns raw `[]byte`** (standalone server)

**Layer 3 — NFSv4 internal COMPOUND dispatch** (`v4/handlers/handler.go:NewHandler`):
- `opDispatchTable` (v4.0, ~35 entries) `OpHandler = func(ctx, reader) *CompoundResult`
- `v41DispatchTable` (v4.1, ~19 entries) `V41OpHandler = func(ctx, v41ctx, reader) *CompoundResult`
- Stream-decode (`io.Reader`), not pre-decoded bytes.

**Fragmentation summary:** 5 (functionally 6) dispatch tables · 6 handler signatures · 3 distinct `HandlerResult` types (NFS/Mount shared, NLM own, NSM own, portmap raw bytes, v4 `*CompoundResult`) · 2 parallel program-level entry points (live in `pkg/`, test in `internal/`).

### 1.3 Handler Contract — Per-Protocol Shape

| Protocol | Context Type | File | Common fields | Protocol-specific |
|---|---|---|---|---|
| NFSv3 | `NFSHandlerContext` | `v3/handlers/doc.go:16` | Context, ClientAddr, AuthFlavor, UID/GID/GIDs | Share |
| NFSv4 | `CompoundContext` | `v4/types/types.go:89` | ″ | CurrentFH/SavedFH/ConnectionID/SessionID |
| Mount | `MountHandlerContext` | `mount/handlers/mount_context.go:30` | ″ | KerberosEnabled |
| NLM | `NLMHandlerContext` | `nlm/handlers/context.go:16` | ″ | Data (raw bytes) |
| NSM | `NSMHandlerContext` | `nsm/handlers/context.go:13` | ″ | ClientName |

All five carry the identical six common fields. The commonality is the consolidation signal.

### 1.4 Auth-Context Threading Model

**Stage 1 — wire credential extraction** (AUTH_UNIX/GSS):
- v3: `middleware.ExtractHandlerContext` (`middleware/auth.go:42`)
- v4: `ExtractV4HandlerContext` (`v4/handlers/context.go:24`)
- Mount: `middleware.ExtractMountHandlerContext` (`middleware/auth.go:126`)
- NLM/NSM: **inlined 8-line AUTH_UNIX block copy-pasted** in `pkg/adapter/nfs/handlers.go:174` (NLM) + `:244` (NSM) — no middleware call.

**Stage 2 — AuthContext construction** (identity mapping + share permission):

| Function | Used by | Cache | On mapping fail |
|---|---|---|---|
| `GetCachedAuthContext` (`v3 doc.go:44`) | getattr, read, write | sync.Map | propagate error |
| `BuildAuthContextWithMapping` (`v3 auth_helper.go:52`) | access, commit, link, lookup, readdir, readdirplus, readlink | none | propagate error |
| `buildAuthContextWithWCCError` (`v3 doc.go:130`) | create, mkdir, mknod, remove, rmdir, rename, setattr, symlink | none | wrap WCC attrs |
| `buildV4AuthContext` (`v4 helpers.go:24`) | all v4 real-FS ops | none | **SILENT fallback to unmapped identity (`helpers.go:62-67`)** |
| none | fsinfo, fsstat, pathconf | — | — |

All funnel to `Registry.ApplyIdentityMapping` — so squash is **not** bypassable (confirms REVIEW §3 downgrade). But the same concern is implemented 4 ways with divergent caching + error-handling. The v4 silent fallback is a real security divergence: a transient mapping failure gives v4 clients raw unsquashed creds while v3 gets an error.

`NeedsAuth bool` is declared on every dispatch entry (42 occurrences across 4 tables) and **read nowhere** — dead field.

### 1.5 pkg/ vs internal/ Split

Within one Go module, `internal/` does NOT block cross-package imports — the split here is purely organizational, zero enforcement. `pkg/adapter/nfs/handlers.go` contains hand-written dispatch logic that duplicates `internal/adapter/nfs/dispatch.go:Dispatch()`. Asymmetric with SMB (SMB: one `DispatchTable`, one context, one result, adapter struct in `pkg/`, logic in `internal/`). The NFS split causes the dual-entry-point problem (SF-1).

---

## 2. Structural Findings

### SF-1 — DUAL DISPATCH ENTRY POINTS — **HIGH-design** — redesign
`pkg/adapter/nfs/handlers.go` (live) and `internal/adapter/nfs/dispatch.go:Dispatch()` (test-only) both implement program routing. They share the tables but not auth-extraction or error-handling. NLM/NSM auth lives ONLY in the live path. Every fix must be verified in both; divergence is silent. Evidence: `handlers.go:34` vs `dispatch.go:249` both index `NfsDispatchTable` via different code.

### SF-2 — 5 DISPATCH TABLES, 6 SIGNATURES, 3 RESULT TYPES — **MED-design** — refactor-in-place
Generic `handleRequest[Req,Resp]()` (`helpers.go:73`) unifies NFS/Mount only. NLM/NSM each carry ~15-line bespoke decode-call-encode boilerplate. Not forced by protocol — all could share `([]byte, error)` + a status field. Portmap's raw-bytes style is a defensible exception (standalone server).

### SF-3 — 5 CONTEXT TYPES, IDENTICAL CREDENTIAL FIELDS — **MED-design** — refactor-in-place
All carry the same six common fields. Extract `RPCHandlerBase{Context, ClientAddr, AuthFlavor, UID, GID, GIDs}` as an embedded struct; per-protocol types become thin extensions. Mechanical, zero-behavior. Enables one generalized stage-1 extractor (kills SF-5 duplication).

### SF-4 — 4 AUTH BUILDERS, DIVERGENT SEMANTICS — **HIGH-design** (causes correctness bugs) — refactor-in-place
Catalogued §1.4. The `buildV4AuthContext` silent fallback (`helpers.go:62`) is a security divergence. `buildAuthContextWithWCCError` exists for a legit need (WCC attrs in error responses) but awkwardly as a separate fn. Canonical fix: one `BuildAuthContext(ctx, reg, shareName, wccHandle *FileHandle) (*AuthContext, error)`; caching as a composable wrapper; v4 propagates mapping errors as `NFS4ERR_ACCESS` in the COMPOUND loop. Delete `NeedsAuth` (SF-6).

### SF-5 — NLM/NSM AUTH INLINED, NOT MIDDLEWARE — **LOW-design** — refactor-in-place
`handlers.go:174` (NLM) + `:244` (NSM) inline the AUTH_UNIX block that `middleware.ExtractHandlerContext` already implements. Future bounds/GSS changes silently miss NLM/NSM. Fix depends on SF-3.

### SF-6 — `NeedsAuth` IS DEAD CODE — **LOW-design** — refactor-in-place
42 occurrences, zero consumers. Planned-but-never-completed dispatch-level auth gate. Delete.

### SF-7 — pkg/internal SPLIT IS COSMETIC — **LOW-design** — keep or restructure
No enforcement within the module; asymmetric with SMB. Root cause of SF-1.

### SF-8 — v4 TWO INTERNAL OP TABLES — **LOW-design / acceptable** — keep
v4.1 ops need slot/session context (`v41ctx`) v4.0 ops don't. Asymmetry justified by protocol. v4.0 table reused by v4.1 for shared ops — efficient.

### SF-9 — PORTMAP ISOLATED — **acceptable** — keep
Own server/registry/table, no auth/share context, localhost-gated mutations. Correct. The `PortmapDispatcher` interface only exists for the unused `Dispatch()` test path (benign, from SF-1).

### SF-10 — GOD-FILE `pkg/adapter/nfs/nlm.go` 600+ LINES — **MED-design** — refactor-in-place
Six concerns: `nlmService`, `routingNLMService`, `createRoutingNLMService`, `handleClientCrash` (the H14 stub bug), `initNSMHandler`, `initGSSProcessor`. Init logic belongs in `adapter.go`; H14 stub is hidden in infra code.

### SF→REVIEW correctness mapping
| Design | Correctness bug it causes/prevents |
|---|---|
| SF-4 | v4 silent fallback enables squash bypass on mapping error; REVIEW §3 residual |
| SF-1 | fixes miss the other path; NLM/NSM auth only in live path |
| SF-10 | H14 (handleClientCrash stub) hidden in infra |

---

## 3. Redesign Options

### Option A — Incremental (auth unify + kill dead code) — LOW risk
- One `BuildAuthContext` fn; caching as wrapper; **fix v4 silent fallback** → error → `NFS4ERR_ACCESS`.
- Pull NLM/NSM inline auth into middleware.
- Delete `NeedsAuth` (42×).
- Split `nlm.go` into 3 files.
- **Buys**: kills the v4 security divergence; one builder for handler authors; −120..−200 LOC. **Leaves**: dual dispatch, 5 contexts, 5 tables. **Cost**: 1 PR (~200 LOC), zero behavior change except the fallback fix. Mechanically verifiable.

### Option B — Restructure (single registry + shared base + collapse dual path) — MED risk
On top of A: (1) `RPCHandlerBase` embedded in all 5 contexts; (2) **eliminate `Dispatch()`/`DispatchDeps`** — one entry point in `handleRPCCall`; tests repoint; (3) unify NFS/Mount/NLM `HandlerResult` into one (NSM→`([]byte,error)`); (4) NLM/NSM through middleware; (5) dissolve `handlers.go` into `internal/` dispatchers via the existing `V4Dispatcher`/`NLMDispatcher`/`NSMDispatcher` interfaces.
- **Buys**: eliminates dual-entry-point (SF-1); 1 base struct; 1 result type; aligns with SMB. Structurally prevents future divergence. **Leaves**: 5 tables (now compatible), v4 COMPOUND dispatch (correct, untouched), H1-H17. **Cost**: 2-3 PRs, zero behavior change. Each step independently verifiable.

### Option C — Rewrite (clean-sheet Router + middleware chain) — HIGH risk, NOT recommended
A `Router{program→version→procedure}` + middleware chain (credentials → auth-context → blocked-op gate → dispatch), one context, one result. **Buys**: true single path, auth impossible to skip, clean pkg=lifecycle/internal=logic. **Costs**: full rewrite of dispatch + 5 contexts + wrappers; high test churn; hard without flag-day; **does NOT fix the 17 HIGH bugs** (they live in handler impls, unchanged); ~3-4 weeks.
**Verdict: not warranted.** Invariants are clean (REVIEW confirmed), handlers disciplined, dispatch tables work — complexity is *accidental* (grown incrementally), not *fundamental*. A+B resolve it at a fraction of the cost.

---

## 4. Recommendation

**Option A now, Option B next sprint. Option C off the table for v1.0.**

1. **A is PR-B-ready** — it IS the auth-builder unification REVIEW §3 flags, and directly closes the v4 silent-fallback security divergence. Ships as one focused PR alongside correctness PR-Bs.
2. **B unblocks long-term maintainability with no flag-day** — the dual-dispatch entry point is the root cause of "fix one path, miss the other" friction. Two clean PRs after A, no handler logic touched.
3. **C delays v1.0 correctness fixes by weeks for no user-visible gain.** No prod users permits eager deletion, but the structure isn't broken enough to justify the cost.

**Phased PR-B sequence:**
```
PR-B0 (Option A): auth consolidation + v4 fallback fix + NeedsAuth delete + nlm.go split (~200 LOC, ~0 behavior)
PR-B1..B7: REVIEW correctness fixes (§7) — independent files
PR-B8 (Option B.1): RPCHandlerBase + NLM/NSM via middleware (zero behavior)
PR-B9 (Option B.2): collapse dual dispatch entry point; repoint tests (zero behavior)
```
Structural work (B0/B8/B9) interleaves with correctness PRs (different files). Only B0 overlaps auth_helper.go with the H9 WCC fix — coordinate ordering there.

---

## Executive Summary

**Top 3 structural findings:**
1. **Dual dispatch entry point** (`handlers.go` live + `dispatch.go:Dispatch()` test): two parallel routing impls that diverge silently; NLM/NSM auth only in the live path. Highest maintainability friction.
2. **4 auth builders, divergent semantics**: v4 silently falls back to *unmapped* credentials on mapping failure while v3 errors — a live security divergence. `NeedsAuth` dead field (42×) is the visible symptom of an abandoned dispatch-auth gate.
3. **5 incompatible dispatch tables / 6 signatures / 3 result types**: not forced by protocol — all could share `([]byte, error)` + a status field.

**Recommendation**: Option A (auth consolidation) now + Option B (collapse dual dispatch + context base) same sprint as correctness fixes. Option C (rewrite) NOT warranted — handlers are disciplined and invariant-clean; only the dispatch layer carries accidental complexity. The 17 HIGH correctness bugs are orthogonal to structure and must be fixed regardless.

**One-sentence why**: NFS handler logic is sound and protocol-invariant-clean, but the dispatch layer grew into 5 parallel tables with incompatible contracts and 4 auth builders with different semantics — Option A closes the v4 silent-fallback security divergence in one PR; Option B eliminates the dual-entry-point maintainability trap in two more, zero behavior change.
