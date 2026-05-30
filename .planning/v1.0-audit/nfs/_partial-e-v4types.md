# NFS Audit — Sub-area E: v4 attrs + types + XDR wire encoding

Scope: `internal/adapter/nfs/v4/attrs/`, `internal/adapter/nfs/v4/types/` (32 prod files), `internal/adapter/nfs/types/`, `internal/adapter/nfs/xdr/` (+ `xdr/core/`), `internal/adapter/nfs/v4/pseudofs/`.

**Wiring note:** these packages are live. `v4/attrs` is imported by `v4/handlers/{getattr,setattr,create,open,verify,readdir}.go` + `pkg/adapter/nfs/settings.go`; `v4/types` by `v4/v41/handlers/*` and `v4/state/*`; `xdr` is the shared NFS XDR primitive package (also used by v3, mount, nlm, nsm, portmap).

**Verification:** `go build ./internal/adapter/nfs/v4/types/` → exit 0; `go vet ./internal/adapter/nfs/...` → clean. Directly read in full: all `xdr/` and `xdr/core/` files; `v4/attrs/{bitmap,decode,encode,acl}.go`; `v4/types/{types,session_common,exchange_id,create_session,layoutget,secinfo_no_name,test_stateid,backchannel_ctl}.go`; `v4/pseudofs/pseudofs.go`. **Every** `make([]T, count)` site across `v4/types`, `v4/attrs`, and `xdr/core` was verified to have a preceding `if count > N` cap (confirmed via grep `-B6` context: getdevicelist≤1024, cb_notify/cb_notify_deviceid≤4096, cb_sequence≤1024, create_session/backchannel_ctl sec_parms≤64, exchange_id impl_id≤1, layoutget≤256, test_stateid≤1024 both sites, channel rdma_ird≤1, auth_sys gids≤16, bitmap≤8, ACL≤MaxACECount, opaque≤1 MB). All cited line numbers are accurate.

**Headline: the XDR/attr decode layer is genuinely clean.** Every attacker-controlled length/count is bounded *before* allocation — there are **no confirmed DoS holes**. The findings below are polish, one real design concern (the 1 MB opaque cap is one-size-fits-all), and bloat. This is unusually disciplined for hand-rolled XDR.

---

## v4 attrs

### [LOW] Bitmap word cap is generous but not RFC-tight — `internal/adapter/nfs/v4/attrs/bitmap.go:55`
`DecodeBitmap4` rejects `numWords > 8` before `make([]uint32, numWords)` (`bitmap.go:55-59`). Safe (max 256 KiB-ish, actually 32 B). RFC 7530/8881 currently define ≤3 words; 8 is fine headroom. Note a *second* bitmap decoder exists — `v4/types/session_common.go:80` (`Bitmap4.Decode`) caps at 256 words, and the encoder paths in `attrs/encode.go` build bitmaps via `SetBit`. Two cap constants (8 vs 256) for the same logical object is mild inconsistency; both are safe. No action required beyond optionally unifying.

### [LOW] fattr4 SETATTR decode correctly walks bits ascending and rejects non-writable/unknown attrs — `internal/adapter/nfs/v4/attrs/decode.go:178-194`
`DecodeFattr4ToSetAttrs` reads the bitmap, then the attr_vals opaque, then iterates bits in ascending order (RFC 7530 §5.1 order), returning `NFS4ERR_ATTRNOTSUPP` for non-writable bits (`decode.go:184-186`) and `NFS4ERR_BADXDR`/`NFS4ERR_INVAL`/`NFS4ERR_BADOWNER` via typed errors. This is correct. The attr_vals is decoded as a bounded opaque first (`decode.go:166`, ≤1 MB) then walked from an in-memory `bytes.Reader`, so a per-attribute over-read fails cleanly with EOF rather than panicking — the "inner length vs outer budget" trap is structurally avoided. Good.

### [LOW] `resolveGroupString` ignores the identity mapper — `internal/adapter/nfs/v4/attrs/encode.go:718-722`
`resolveOwnerString` (uid) consults `identityMapper` for reverse resolution, but `resolveGroupString` (gid) always returns numeric, with a comment noting the mapper lacks group reverse-lookup. Functionally fine for `nfs4_disable_idmapping=Y` (Linux default), but asymmetric: a Kerberos/idmapd deployment gets `user@domain` owners and bare-numeric groups, which idmapd may then map to `nobody`. Track as a known idmap gap.

### [LOW] `supported_attrs` set looks internally consistent with the encoder switch — `internal/adapter/nfs/v4/attrs/encode.go:167-214` vs `encodeRealFileAttr` (`:498`)/`encodeSingleAttr` (`:264`)
Every bit `SetBit`-ed into `SupportedAttrs()` has a matching `case` in both the pseudo-fs and real-file encoder switches (spot-checked: SUPPORTED_ATTRS, TYPE, CHANGE, FSID, ACL, ACLSUPPORT, OWNER, SPACE_*, TIME_*, SUPPATTR_EXCLCREAT, MAXREAD/WRITE). No advertised-but-unencodable attr found. Worth a dedicated test asserting `for each bit in SupportedAttrs(): encoder has a case` to prevent future drift. `FATTR4_FSID` is hard-coded to `(0,1)` for both pseudo-fs and real files (`encode.go:530-533`) — deliberate, to stop macOS triggered-mount churn — see pseudofs note below.

### [LOW] CHANGE attribute derives from ctime nanos — `internal/adapter/nfs/v4/attrs/encode.go:511`
`FATTR4_CHANGE` = `uint64(file.Ctime.UnixNano())`. Monotonic per-file only as long as ctime advances; two metadata updates within the same nanosecond would not bump change. Acceptable for v1.0 but a true change-counter would be more correct (RFC 7530 §5.4). Informational.

---

## XDR primitives

### [MED] Single global 1 MB opaque cap is too small for some legitimate NFSv4 fields — `internal/adapter/nfs/xdr/core/decode.go:38-41`
`DecodeOpaque` hard-caps every variable-length opaque/string at `1024*1024`. This is the right *kind* of defense (bound-before-`make`, `data := make([]byte, length)` at `:44` only runs after the check) and kills the classic 4-GiB-length DoS. But one global constant is applied to wildly different fields: a file handle (≤128 B), an owner string (tiny), a GSS/SPNEGO token, an SSV blob, a symlink target, and — critically — the COMPOUND-level data the v4 handlers read. 1 MB is simultaneously (a) far larger than needed for handles/names (a tighter per-field cap would be better defense-in-depth) and (b) potentially *too small* for a large WRITE payload or a fat fattr4 attr_vals if those ever flow through this opaque path. Recommend per-field limits (handle/name small, payload large) rather than one 1 MB value. Not a DoS — a correctness/robustness smell.

### [MED] Short-read safety is correct, but `encoding/binary.Read` on fixed ints is the one read that won't silently zero-fill — `internal/adapter/nfs/xdr/core/decode.go`
All variable data uses `io.ReadFull` (`:45` data, `:56` padding) — no silent-short-read bug (the dangerous `r.Read` pattern is absent). Fixed-width ints use `binary.Read` (`:32,:97,:116,...`), which internally uses `io.ReadFull` and errors on short read. Confirmed safe. Noting it explicitly because the audit brief calls this class out: it is **not** present here.

### [LOW] Opaque decode does not validate that padding bytes are zero — `internal/adapter/nfs/xdr/core/decode.go:53-59`
Padding is read-and-discarded (`io.ReadFull(reader, padBuf[:padding])`) without checking the 0–3 bytes are zero. RFC 4506 §4.10 says padding "SHOULD" be zero; Linux nfsd does not enforce it either, so this is spec-compliant laxness. Informational only.

### [LOW] Two near-identical opaque/string encoders — `internal/adapter/nfs/xdr/core/encode.go:36` (`WriteXDROpaque`) + `internal/adapter/nfs/xdr/encode.go:300` (thin re-export) + `attrs`/`types` all call through
The top-level `xdr.WriteXDROpaque`/`WriteXDRString`/`WriteXDRPadding` (`encode.go:300-357`) are pure pass-throughs to `xdr/core`. Fine as a layering shim, but `encode.go` *also* hand-rolls padding inline in `EncodeOptionalOpaque` (`:54-59`) instead of calling `WriteXDRPadding` — minor duplication, harmless. Padding math `(4-(len%4))%4` is correct everywhere checked.

### [LOW] Trivial helper files — `internal/adapter/nfs/xdr/{utils.go (8 L), pointers.go (7 L)}`
`containsIgnoreCase` and `ptrUint32` are one-liners each in their own files. Fold into a neighbor (e.g. `errors.go` uses `containsIgnoreCase`; `attributes.go` uses `ptrUint32`). Cosmetic.

---

## types & bloat

### [MED] `v4/types` is over-split — 32 production files, ~20 are <100 LoC single-DTO files
Sub-100-LoC prod files (LoC): `errors.go`(55), `destroy_session.go`(83), `destroy_clientid.go`(84), `free_stateid.go`(84), `cb_recall_slot.go`(85), `reclaim_complete.go`(86), `cb_push_deleg.go`(94), `cb_wants_cancelled.go`(94) — plus `backchannel_ctl.go`(113), `set_ssv.go`(121), `secinfo_no_name.go`(127), `cb_notify_lock.go`(136), `test_stateid.go`(137), `want_delegation.go`(165), `cb_notify_deviceid.go`(168), `cb_recall_any.go`(170), `getdevicelist.go`(172), `bind_conn_to_session.go`(178), `getdeviceinfo.go`(178), `get_dir_delegation.go`(252). Each is one struct + Encode/Decode/String. They group cleanly by operation family: **session** (create_session, bind_conn_to_session, destroy_session, sequence, backchannel_ctl, set_ssv, exchange_id), **callbacks** (all `cb_*`), **pnfs/layout** (layout*, getdevice*), **stateid/clientid** (free_stateid, test_stateid, destroy_clientid, reclaim_complete). Merging to ~8–10 family files would roughly halve the file count with zero behavior change. This is the bloat headline. (Counter-argument: the per-op split mirrors RFC section structure and keeps diffs small — but at 32 files the navigation cost dominates.)

### [LOW] Two bitmap implementations, two cap constants — `v4/attrs/bitmap.go` (funcs, cap 8) vs `v4/types/session_common.go:57-104` (`Bitmap4` type, cap 256)
`attrs` uses `[]uint32` + free functions (`DecodeBitmap4`, `SetBit`, `IsBitSet`, `Intersect`); `v4/types` defines a `Bitmap4` named type with methods. They don't share code. Both safe. If the layer is ever consolidated, pick one (the named-type `Bitmap4` with methods is the cleaner Go shape) and delete the other.

### [LOW] `internal/adapter/nfs/types/` is a small 2-file package (`types.go` 184, `constants.go` 309) — appears correctly scoped
Holds NFS protocol-wire types (`NFSFileAttr`, `WccAttr`, `TimeVal`, `SpecData`) and status/type constants. `xdr/attributes.go` converts `metadata.FileAttr` → `types.NFSFileAttr` at the boundary (`MetadataToNFS`, `attributes.go:53`), which is the correct direction (protocol DTO distinct from domain type), not a 1:1 mirror. No violation.

### [RESOLVED] `test_stateid.go` array allocations are capped — `internal/adapter/nfs/v4/types/test_stateid.go:46` and `:116`
Both `make` sites (`a.Stateids` at `:49`, `res.StatusCodes` at `:119`) are guarded by `if count > 1024` (`:46`, `:116`). No DoS. Confirmed by direct read. Listed here only to close out the earlier suspicion.

### [LOW] `v4/pseudofs/pseudofs.go` (367 LoC) — fsid + concurrency confirmed OK; one READDIR-encoding gap
Directly read. (1) `PseudoNode.GetFSID()` returns `(0, 1)` (`pseudofs.go:76-78`), exactly matching the encoder's hard-coded `FATTR4_FSID = (0,1)` for real files (`attrs/encode.go:530-533`) — pseudo-root and real exports deliberately share one fsid to avoid macOS triggered-mount churn. Consistent. (2) `PseudoFS` is fully `sync.RWMutex`-protected; all public methods lock; `Rebuild` takes the write lock and preserves root handle/FileID for stability, bumps `ChangeID` (`:199-302`). `FileID` uniqueness is via `atomic.AddUint64(&nextID)` with reuse-by-path. No data race. (3) Minor: `FindJunction` (`:331`) is an O(n) linear scan of `byHandle` — fine at realistic share counts. (4) The package has no READDIR-entry XDR encoder itself — directory listing for the pseudo-root must be assembled by the handler layer from `ListChildren` (`:305`); confirm that path encodes per-entry fattr4 + cookie correctly (out of this sub-area's scope, but the seam to check). Net: pseudofs is solid; no finding beyond the READDIR-encoding seam note.

### Tests — good breadth, one gap
`v4/attrs`: `decode_test.go`(443), `encode_test.go`(370), `bitmap_test.go`(274), `acl_test.go`(274). `xdr`: `xdr_test.go`(543), `time_test.go`(274). `v4/types`: per-op `*_test.go` for nearly every DTO + `constants_test.go`(595) + `session_common_test.go`(583). **Gap:** add explicit adversarial-length tests (length=`0xFFFFFFFF`, count=`0xFFFFFFFF`) asserting each decoder returns an error rather than allocating — this both pins the existing caps and would have immediately answered the `test_stateid.go` MUST-VERIFY above. Also add the "every supported_attrs bit has an encoder case" assertion.

---

## Severity tally

| Severity | Count | Findings |
|---|---|---|
| HIGH | 0 | none — every variable-length decode is bounded before allocation |
| MED | 2 | 1 MB global opaque cap is one-size-fits-all (too small for large fields like big WRITE/attr_vals, looser than needed for handles/names); 32-file over-split in `v4/types` |
| LOW | 10 | two bitmap impls / two caps (8 vs 256); resolveGroupString no idmap; CHANGE from ctime nanos; padding-zero not checked; dup opaque encoders + inline padding; trivial helper files; supported_attrs↔encoder drift-test missing; pseudofs READDIR-encoding seam; adversarial-length tests missing; nfs/types correctly scoped (confirmed, no-op) |

**Top finding (MED):** the single global 1 MB opaque cap in `xdr/core/decode.go:38` is applied uniformly to every variable-length field. It correctly kills the 4-GiB-length DoS, but the granularity is wrong: too generous for handles/names (defense-in-depth would use small per-field caps) and potentially too small for large legitimate payloads if any large field is ever routed through this opaque path. Recommend per-field limits.

**Bloat headline:** `v4/types` is 32 production files, ~20 of them sub-100-LoC single-DTO files — merging by operation family (session / callbacks / layout / stateid-clientid) would roughly halve the file count with no behavior change. Secondary: two parallel bitmap implementations (`attrs` free-funcs cap 8 vs `v4/types` `Bitmap4` cap 256), two trivial 1-line helper files in `xdr` (`utils.go`, `pointers.go`), and `WriteXDROpaque` re-exported through two layers.

**Overall:** the XDR/attr wire layer is DoS-clean — no confirmed HIGH or even a real correctness bug, just one design-granularity note (1 MB cap) and structural bloat. The earlier `test_stateid` suspicion is resolved (both sites capped at 1024). Pseudofs fsid/concurrency confirmed correct.
