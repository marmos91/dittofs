# NFS Protocol-Version Coexistence Audit (partial-i: versions)

> Repo: `dittofs-v10-plan` (branch `v1.0/blockstore-perf-b1` ≈ develop). READ-ONLY audit.
> Scope: NFSv3 / v4.0 / v4.1 coexistence behind one program/port/dispatch; version
> negotiation/PROG_MISMATCH; cross-version shared state; handle compatibility; cleanliness.
> Primary evidence (byte-verified): `internal/adapter/nfs/dispatch.go` (`Dispatch`),
> `internal/adapter/nfs/rpc/{constants,parser}.go`, `internal/adapter/nfs/v4/handlers/compound.go`,
> `docs/NFS.md`, and the consolidated `.planning/v1.0-audit/nfs/REVIEW.md`.

**Grounding fact confirmed:** DittoFS implements **NFSv3, NFSv4.0, NFSv4.1 only — no NFSv2**.
No `v2/` directory, no `package v2`, no `NFSVersion2` constant or branch. v2 appears in
`docs/NFS.md` only as a historical row in the protocol-history table (1989/UDP), never wired.
There are two coexisting dispatch implementations in the tree (see Coexistence cleanliness): the
canonical consolidated `dispatch.go::Dispatch` (program→version→proc) and an older
`connection.go::handleRPCCall` path; this audit treats `Dispatch` as the source of truth since
its doc comment is the documented routing contract.

---

## Supported versions (factual: what's advertised where)

Single routing chokepoint: `internal/adapter/nfs/dispatch.go::Dispatch` — a flat
`switch call.Program` over five ONC-RPC programs, each delegating to a per-program sub-dispatcher
that owns its own version gate. Program/version numbers are centralized in `rpc/constants.go`.

| Program | # (constants.go) | Versions accepted | Gate (dispatch.go) |
|---|---|---|---|
| Portmap | 100000 | (handler-internal) | `:198` → `dispatchPortmap` |
| NFS | 100003 | **v3, v4** (v4 minor 0/1 chosen in COMPOUND) | `:213` `dispatchNFS` `switch call.Version`: `NFSVersion3`(3) / `NFSVersion4`(4) / `default → MakeProgMismatchReply(xid, 3, 4)` |
| MOUNT | 100005 | **MNT: v3 only**; NULL/DUMP/UMNT/UMNTALL/EXPORT: v1/v2/v3 | `:267` `dispatchMount`: only `MountProcMnt` gated to `MountVersion3`(3); others version-agnostic (macOS UMNT is mount v1) |
| NLM | 100021 | **v4 only** | `:302` `dispatchNLM`: `!= NLMVersion4(4) → MakeProgMismatchReply(xid, 4, 4)` |
| NSM | 100024 | **v1 only** | `:327` `dispatchNSM`: `!= NSMVersion1(1) → MakeProgMismatchReply(xid, 1, 1)` |
| unknown program | — | — | `:201` default → `MakeErrorReply(xid, RPCProgUnavail)` (1) |

Facts from `docs/NFS.md`: TCP-only, default port **12049**, one listener multiplexes all programs;
embedded portmapper (default 10111) registers NFS/MOUNT/NLM/NSM; MOUNT is v3-only and v4 has no
mount (PUTROOTFH+LOOKUP) — doc and code agree.

**Verdict:** versions coexist behind one port via a clean three-level routing chain
(`program` → `version` → v4 `minorversion`). v2 is absent by design and matches the docs.

---

## Version negotiation & PROG_MISMATCH (correctness, incl. v2-client + bad-minorversion)

**This dimension is correct and spec-faithful** — verified down to the wire encoder.

- **CLEAN — v2 client (vers=2) is handled correctly.** A vers=2 call to program 100003 hits the
  `default` arm of `dispatchNFS` (`dispatch.go:231`) → `MakeProgMismatchReply(call.XID, NFSVersion3,
  NFSVersion4)` i.e. advertises `[low=3, high=4]`. No panic/hang/garbage. The encoder
  `rpc/parser.go:383 MakeProgMismatchReply` builds: `MSG_ACCEPTED` (`ReplyState=RPCMsgAccepted`),
  **AUTH_NULL reply verifier** (`Verf`, `:396`), `accept_stat = RPCProgMismatch` (2, `:401`), then
  appends `mismatch_info{low, high}` as two big-endian u32 (`:417-424`), and validates `low<=high`
  (`:385`). This is exactly RFC 5531 §9 (`MSG_ACCEPTED` → verifier → `PROG_MISMATCH` → low → high).
- **CLEAN — unknown program returns `PROG_UNAVAIL` (1), not PROC_UNAVAIL.** `dispatch.go:203`
  `MakeErrorReply(call.XID, rpc.RPCProgUnavail)`. (Note: the *older* `connection.go:194` path uses
  `RPCProcUnavail` here — a minor nit isolated to the legacy dispatcher; see cleanliness §.)
- **CLEAN — bad NFSv4 minorversion → `NFS4ERR_MINOR_VERS_MISMATCH`, not RPC PROG_MISMATCH.**
  RPC program-version is `4` for both 4.0 and 4.1; the minor is a COMPOUND-body field (RFC 8881
  §16.2.3). `v4/handlers/compound.go:149` decodes `minorversion`, and `:163`
  `if minorVersion < h.minMinorVersion || > h.maxMinorVersion → encodeCompoundResponse(
  NFS4ERR_MINOR_VERS_MISMATCH, tag, nil)` (empty resarray). The `switch` then handles
  `NFS4_MINOR_VERSION_0`/`_1`. A **v4.2 client (minor=2)** therefore gets the spec-mandated
  `NFS4ERR_MINOR_VERS_MISMATCH (10021)` in the COMPOUND result. Correct layering and correct error.
- **CLEAN — v4-program-but-no-v4-handler degradation.** If `deps.V4Handler == nil`,
  `dispatchNFS:220` returns `MakeProgMismatchReply(xid, 3, 3)` — i.e. honestly advertises "v3 only"
  so the client renegotiates down. Sensible.

- **[LOW] MOUNT non-MNT procedures accept v1/v2/v3 by design** — `dispatch.go:272`. Only `MNT` is
  version-gated (→ `MakeProgMismatchReply(xid, 3, 3)` for non-v3); NULL/DUMP/UMNT/UMNTALL/EXPORT
  accept any of v1/v2/v3 because macOS issues UMNT over mount **v1**. *Why LOW:* intentional,
  documented inline; the loosened gate only covers procedures that return no v3 file handle, so
  there is no v3-handle-format leak to a v1/v2 client. Noted for completeness, not a defect.

**Net:** version negotiation is one of the *cleanest* parts of the NFS surface — a single
chokepoint, correct per-program `[low,high]` ranges, a byte-correct RFC-5531 mismatch encoder, and
correct COMPOUND-level minorversion rejection. v2 and v4.2 clients are both handled cleanly.

---

## Cross-version shared state (lock / share-reservation / store visibility)

Highest-stakes dimension. Evidence from this pass + the consolidated `REVIEW.md`.

**Good news — cross-version locks are NOT siloed.** `REVIEW.md` §6 "Verified-correct":
*"NLM byte-range locking delegates to the unified `pkg/metadata/lock` manager with cross-protocol
conflict detection"*, with `internal/adapter/nfs/nlm/handlers/cross_protocol.go` existing
specifically for this. That same unified manager backs the v4 `LOCK` path and SMB (CLAUDE.md
invariant: one lock manager). So a **v3 NLM byte-range lock and a v4 LOCK on the same file are
mutually visible and conflict correctly** — the per-version dirs are thin protocol shells over one
shared lock/metadata/block store. **Verdict: cross-version lock visibility is correct (NOT a
siloing HIGH).**

The *recovery* and *share-reservation* halves are broken, already filed HIGH in REVIEW.md;
restated here as version-coexistence integrity holes because the hazard is specifically cross-version:

- **[HIGH] v4 grace and NLM grace are not unified at restart → cross-version reclaim race** —
  `internal/adapter/nfs/v4/state/grace.go:104` (v4 grace no-ops on ungraceful restart, REVIEW H7) +
  `pkg/adapter/nfs/nlm.go:74` / `pkg/metadata/lock/manager.go:2282` (NLM managers built with
  `gracePeriod==nil`; `LockFileNLM` never calls `IsOperationAllowed`, REVIEW H15). *What:* on
  restart, v3-NLM and v4 clients must observe **one coherent grace window** before either grabs a
  lock the other is reclaiming. Today neither side enters grace reliably, so a v4 client can take a
  lock during the window a v3 client is reclaiming (and vice-versa). *Fix:* construct the lock
  manager with a grace provider seeded from persisted locks; gate both `LockFileNLM` and v4 reclaim
  on the same `IsOperationAllowed` window.

- **[HIGH] No durable lock/share-reservation state spans v4 + NLM** —
  `internal/adapter/nfs/v4/state/manager.go:31` (v4 state in-memory only, REVIEW H8); the NLM side
  has no journal either. *What:* locks and v4 `OPEN` share reservations do not survive restart on
  any backend, so cross-version reclaim has nothing to reconcile against. *Why coexistence-relevant:*
  it is the *same* durable-state gap on both protocol paths — fixing one without the other re-opens
  the cross-version race.

- **[HIGH] v4 share reservations (`share_deny`) are a silent no-op → unenforced against v3 I/O
  too** — `internal/adapter/nfs/v4/handlers/open.go:331`,
  `internal/adapter/nfs/v4/state/manager.go:944` (REVIEW H2). *What:* v3 has no share-reservation
  concept, so the server is the only enforcement point for a v4 `DENY_WRITE` against a concurrent v3
  WRITE. Because `NFS4ERR_SHARE_DENIED` has zero call sites, deny modes are unenforced even within
  v4 — and therefore certainly across the v3↔v4 boundary. A file a v4 client opened `DENY_WRITE`
  can be silently overwritten by a v3 client (or another v4 owner). *Fix (per REVIEW.md):*
  `openStatesByFile` index + conflict-check; ensure the deny check is consulted on the v3 WRITE path,
  not only at v4 OPEN.

**Store visibility (metadata + block store):** correct. Per CLAUDE.md invariants #4/#5 and the
handler structure, every version resolves the same per-share metadata store and
`GetBlockStoreForHandle` block store; v3 and v4 READ/WRITE go through identical store calls — no
per-version data siloing. Cross-protocol error mapping is also unified
(`internal/adapter/common/errmap.go`, `MapToNFS3`/`MapToNFS4` from one table — `docs/NFS.md` §322).

---

## Handle compatibility

- **[LOW] Handles are version-agnostic and decoded once at dispatch — confirm v4 PUTFH reuses the
  shared codec** — `connection.go:248 extractShareName` → `xdr.DecodeFileHandleFromRequest` →
  `Registry.GetShareNameForHandle`, plus `internal/adapter/nfs/xdr/decode_handle.go` /
  `xdr/filehandle.go` (shared codec). *What:* the dispatcher decodes the file handle from the *raw
  request bytes* generically to resolve the share, with **no version branch** — strong evidence
  handles carry no version tag and are opaque per CLAUDE.md invariant #3. A v3 handle presented on a
  v4 `PUTFH` should therefore **resolve normally** (same opaque bytes, same registry lookup), which
  is the correct behavior. *Why LOW:* there is a `v4/handlers/` dir but the handle codec lives in
  the shared `internal/adapter/nfs/xdr/` package, not duplicated under `v4/`. *Fix/verify:* confirm
  `v4/handlers/putfh.go` calls the shared `xdr` decoder and keeps no parallel v4 handle format; if
  it does, that is drift to consolidate. Handle stability across restarts is a metadata-store
  property (memory=ephemeral; badger/postgres=persistent per `docs/NFS.md` §273), independent of
  protocol version.

---

## Coexistence cleanliness (duplication between version dirs → feeds redesign)

The version split is genuinely clean at the routing seam: one `Dispatch`, one program→version→
(v4-minor) chain, one shared `xdr/`, one shared `rpc/`, one unified lock manager, one shared
error-map table. Handlers are organized `v3/handlers/`, `v4/handlers/`, `v4/v41/handlers/` with thin
per-op files. Cleanup items (most already in REVIEW.md):

- **[MED] Two parallel dispatch implementations coexist** — canonical
  `internal/adapter/nfs/dispatch.go::Dispatch` (program→version→proc, PROG_UNAVAIL for unknown
  program, the documented contract) **vs** the older `internal/adapter/nfs/connection.go::
  handleRPCCall` (inline `switch call.Program`, uses `handleUnsupportedVersion`, returns
  **PROC_UNAVAIL** for unknown program at `:194`). *What:* two routers with subtly different
  unknown-program codes and different mismatch helpers (`MakeProgMismatchReply` vs
  `handleUnsupportedVersion`). *Why:* a redesign hazard — future version/program changes must be
  made in two places and can drift (the PROC/PROG_UNAVAIL nit already has). *Fix:* collapse to the
  single consolidated `Dispatch`; delete or thin `handleRPCCall` to call it.

- **[MED] `v4/types/` is over-fragmented (~32 files, many single-DTO sub-100-LoC)** —
  `internal/adapter/nfs/v4/types/` (REVIEW.md §4 E). Collapse to ~6 op-family files; zero behavior
  change; this is the bulk of the "hard to follow" surface in the v4 tree.

- **[LOW] Duplicate AUTH_UNIX parsers** — `internal/adapter/nfs/rpc/auth.go:117` vs
  `internal/adapter/nfs/auth/unix.go:123` (REVIEW.md §5 C). Two credential parsers invite drift in
  how each version builds `*metadata.AuthContext`. Squash is re-applied per-call via
  `BuildAuthContextWithMapping` in both v3 (`v3/handlers/auth_helper.go:52`) and v4
  (`v4/handlers/helpers.go:59`) — REVIEW.md §3 confirms this is **not** bypassable — but the
  two-parser shape is still consolidation debt. *Fix:* single AUTH_UNIX parser.

- **[LOW] Two bitmap impls / two opaque caps in the v4 wire layer** —
  `internal/adapter/nfs/v4/attrs/bitmap.go` vs `v4/types/session_common.go` (caps 8 vs 256); single
  global 1 MB opaque cap should be per-field (REVIEW.md §4/§5 E). Minor; the v4 XDR layer is
  otherwise the healthiest sub-area (DoS-clean, zero HIGH). *Fix:* one bitmap impl, per-field caps.

- **No v3↔v4 attribute-conversion duplication of concern:** v3 `fattr3`
  (`internal/adapter/nfs/xdr/attributes.go`) and v4 `fattr4` (`v4/attrs/encode.go`) are necessarily
  distinct wire formats; both derive from the same store attrs and error mapping is already unified.
  Not a duplication finding.

---

## macOS / Linux client reality

Handled sanely. The NFS program advertises `[low=3, high=4]` so a client offering a range picks its
best: **Linux** defaults to NFSv4.1 (v4 program, minor=1 in COMPOUND), **macOS** typically
negotiates NFSv3. The MOUNT gate's explicit v1 tolerance for non-MNT procs (`dispatch.go:272`)
exists specifically to accommodate macOS umount issuing mount **v1**; COMPOUND decode also tolerates
macOS trailing bytes (`compound.go:154`). `docs/NFS.md` documents both mount recipes (Linux
needs `sudo`/portmapper; macOS uses `port=12049,mountport=12049`, sometimes `resvport`). One doc
gap: `docs/NFS.md` does not spell out the advertised `[3,4]` range or the v4-minor negotiation
explicitly — worth a sentence for operators.

---

## Severity tally

| Severity | Count | Items |
|----------|-------|-------|
| HIGH | 3 | v4/NLM grace not unified at restart (cross-version reclaim race); no durable lock/share state spanning v4+NLM; v4 `share_deny` no-op (unenforced incl. against v3 I/O). *(All three already filed HIGH in REVIEW.md — restated as coexistence findings.)* |
| MED  | 2 | Two parallel dispatch implementations (`Dispatch` vs `handleRPCCall`) → drift risk + PROG/PROC_UNAVAIL inconsistency; `v4/types/` over-fragmentation (cleanliness/redesign) |
| LOW  | 4 | MOUNT non-MNT v1/v2/v3 (by-design note); confirm v4 PUTFH reuses shared handle codec; duplicate AUTH_UNIX parsers; two bitmap impls / global opaque cap |

(No version-negotiation correctness findings: v2-client PROG_MISMATCH `[3,4]`, unknown-program
PROG_UNAVAIL, and bad-minorversion `NFS4ERR_MINOR_VERS_MISMATCH` are all byte-verified correct in
the canonical `Dispatch`/`MakeProgMismatchReply`/`compound.go` paths.)

**Overall verdict:** Version coexistence is **architecturally clean and the negotiation logic is
spec-correct**. A single `Dispatch` does program→version→(v4 minor) routing through one chokepoint;
a vers=2 client is cleanly rejected with RFC-5531 `PROG_MISMATCH {low=3,high=4}` (AUTH_NULL
verifier + correct accept_stat + mismatch_info, byte-verified), an unknown program gets
`PROG_UNAVAIL`, and a v4.2 client gets COMPOUND-level `NFS4ERR_MINOR_VERS_MISMATCH`. v3/v4 share one
metadata store, one block store, and **one unified lock manager with cross-protocol conflict
detection** — so v3-NLM and v4 locks ARE mutually visible (not siloed). The real risk is not
siloing but **broken cross-version *recovery*** (grace not unified, no durable lock state) plus the
**unenforced v4 share-deny** a v3 writer can silently violate. Top finding: unify the grace/reclaim
window across the v4 state machine and the NLM lock manager (HIGH; interlocks with the
durable-lock-state gap). Secondary cleanup: collapse the two parallel dispatchers into one.
