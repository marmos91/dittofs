# NFS Interface Audit — NSM / NLM / AUTH / Lock

**Date**: 2026-05-30. READ-ONLY.
**Method**: enumerated every `type X interface` in `internal/adapter/nfs`, `pkg/adapter/nfs`, `pkg/metadata/lock`; counted impls (`var _ X` assertions + method-impl grep) and non-test refs. (Compiled by orchestrator after the spawned agent stalled on a watchdog timeout — facts grep-verified directly.)

**Total interfaces in scope: 24.** Genuinely load-bearing: ~21. Removable/reshape: 3.

---

## AUTH / identity interfaces

| Interface | file:line | impls | refs | Verdict |
|---|---|---|---|---|
| `IdentityMapper` | `identity/mapper.go:32` | ≥2 (`StaticMapper`, `CachedMapper` wrapper) | 20 | **KEEP** — genuine abstraction; CachedMapper decorates any mapper; central auth seam. |
| `GroupResolver` | `identity/mapper.go:75` | **0** | 3 (all doc-comments + one "can be extended when GroupResolver…" comment) | **DELETE** — zero implementations (no `ResolveGroup` method exists anywhere), zero real callers. Pure speculative future-proofing. Confirmed dead by bloat pass too. |
| `MappingStore` | `identity/table.go:30` | 1 | 7 | **KEEP** — backing-store seam for the mapping table; small, cohesive. |
| `Verifier` | `rpc/gss/framework.go:49` | 1 (krb5) | 35 | **KEEP** — GSS verifier seam; real (allows test fakes + future mechs); heavily used. |

**AUTH note**: `metadata.AuthContext` is a struct (not interface) threaded everywhere — correct. The 4-builder fragmentation (DESIGN SF-4) is a *function* problem, not an interface problem. The identity-mapping interface surface is clean; only `GroupResolver` is dead.

## Lock interfaces (`pkg/metadata/lock/`)

| Interface | file:line | methods | impls | refs | Verdict |
|---|---|---|---|---|---|
| `LockManager` | `manager.go:27` | **48** | 1 (`*Manager`, `manager.go:307`) | 107 | **KEEP, consider SPLIT** — fat interface (48 methods, 1 impl). Interface-segregation smell: NLM, v4-state, and SMB each use only a slice. But it IS the shared cross-protocol lock seam (the thing that makes v3-NLM + v4-LOCK + SMB conflict-detection unified — a genuine architectural win, see versions audit). Splitting into role interfaces (`ByteRangeLocker`, `OpenStateLocker`, `DelegationManager`, `GraceController`) consumed where used would honor ISP without losing the shared impl. Low-priority refactor; not v1.0-blocking. |
| `HandleChecker` | `manager.go:302` | small | 1 | 5 | **KEEP** — narrow seam (handle-validity probe), consumed not where implemented. Good Go idiom. |
| `LockStore` | `store.go:211` | — | 1+ | 49 | **KEEP** — persistence seam (memory/durable backends); real. |
| `DurableHandleStore` | `durable_store.go:90` | — | 1+ | 49 | **KEEP** — durable-handle backend seam. |
| `ClientRegistrationStore` | `client_store.go:59` | — | 1+ | 34 | **KEEP** — client-registry backend. |
| `OpLockBreakCallback` | `oplock_break.go:29` | small | — | 6 | **KEEP** — break-notification seam (NFS + SMB register callbacks). |
| `BreakCallbacks` | `oplock_break.go:48` | — | — | 18 | **KEEP** — callback registry; cross-protocol. |
| `DirChangeNotifier` | `directory.go:57` | small | — | 16 | **KEEP** — dir-change notification seam (dir delegations). |

**Lock note — the central positive**: NLM (`internal/adapter/nfs/nlm/`) and v4-state (`internal/adapter/nfs/v4/state/`) both consume the **same** `LockManager` — NOT two parallel siloed lock interfaces. This is the correct design and what makes cross-protocol conflict detection work (confirmed by `_partial-i-versions.md`). The earlier worry "do NLM and v4 define parallel lock interfaces?" → **answered NO, they share one.** The only lock-interface issue is `LockManager` being fat (ISP), not duplication.

## NLM interfaces

| Interface | file:line | impls | refs | Verdict |
|---|---|---|---|---|
| `NLMLockService` | `nlm/handlers/handler.go:20` | 1 (`routingNLMService` via `nlmService`) | 9 | **KEEP** — seam between NLM handlers and the lock-routing service; enables per-share routing + test fakes. |
| `NLMDispatcher` | `dispatch.go:140` | 1 | 2 | **KEEP** — import-cycle break (`pkg`↔`internal`), confirmed load-bearing by bloat pass. |

NLM has no interface duplication. `fileChecker` (`pkg/adapter/nfs/nlm.go:25`) is a tiny local adapter, not an exported interface — fine.

## NSM interfaces

No dedicated exported NSM interface beyond `NSMDispatcher` (`dispatch.go:146`, 2 refs, import-cycle break — KEEP). The NSM monitor/tracker is a concrete struct. Given NSM's small size and single impl, **not introducing** an interface here is the correct less-is-more call — no finding.

## Cross-cutting (segregation + duplication)

| Interface | file:line | refs | Verdict |
|---|---|---|---|
| `V4Dispatcher` / `NLMDispatcher` / `NSMDispatcher` / `PortmapDispatcher` | `dispatch.go:133-152` | 2 each | **KEEP** — all four are `pkg`↔`internal` import-cycle breaks (the live dispatch in `pkg/` calls into `internal/` handlers through these). They *look* like the classic "1 impl + 2 callers → inline" target but inlining would create an import cycle. NOT bloat. (They only exist because of the dual-dispatch-entry-point — DESIGN SF-1; collapsing that (Option B) would let 2 of the 4 disappear naturally.) |
| `Releaser` | `v3/handlers/nfs_response.go:52` | 6 | KEEP — buffer-release seam for pooled responses. |
| `PseudoFSAttrSource` | `v4/attrs/encode.go:142` | 10 | KEEP — decouples attr encoder from pseudofs concrete type. |
| `Serializable`/`XdrEncoder`/`XdrDecoder` | `rpc/parser.go:16`, `xdr/core/union.go` | — | KEEP — XDR primitives. |
| `NFS4StatusError` | `v4/attrs/decode.go:391` | — | KEEP — typed-error seam for attr decode → NFS4 status. |
| `PortmapRegistry` | `portmap/handlers/handler.go:16` | — | KEEP — registry seam for the standalone portmap server. |

---

## Top interface-design problems (ranked)

1. **`LockManager` is a 48-method fat interface with 1 impl** (`manager.go:27`) — interface-segregation violation. Each consumer (NLM, v4-state, SMB) uses a different ~6-10 method slice. SPLIT into role interfaces consumed at use-sites. Low priority (works correctly, big refactor), but the clearest ISP smell in the subsystem. NOT v1.0-blocking.
2. **`GroupResolver` is dead** (`identity/mapper.go:75`) — 0 impls, 0 real callers. DELETE (+ the stale "can be extended when GroupResolver…" comment at `encode.go:720`). Trivial, do in PR-B0/bloat.
3. **4 `*Dispatcher` interfaces exist only to break the `pkg`↔`internal` import cycle** — a symptom of DESIGN SF-1 (dual dispatch entry point). They're correct as-is, but collapsing the dual entry point (DESIGN Option B) would let `V4Dispatcher`/`NLMDispatcher`/`NSMDispatcher` shrink or vanish. Track with SF-1, not separately.

## Verdict

**Of 24 interfaces: 21 are genuinely load-bearing** (real seams, multiple impls, cross-protocol sharing, or import-cycle breaks), **1 is dead** (`GroupResolver` → DELETE), **1 is over-fat** (`LockManager` → SPLIT, low priority), and **4 are cycle-break artifacts** of SF-1 (resolve via Option B, not by deletion). **No NLM/v4 lock-interface duplication** — they correctly share one `LockManager`, which is an architectural strength. The interface surface is, overall, healthy — far less bloated than the *function*-level fragmentation (4 auth builders) and the dispatch-table fragmentation. The user's "useless interfaces" concern lands on exactly one symbol (`GroupResolver`); the rest earn their keep.
