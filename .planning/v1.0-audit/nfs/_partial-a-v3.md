# NFSv3 handlers — findings

Audit scope: `internal/adapter/nfs/v3/handlers/` (52 files) + `internal/adapter/nfs/mount/handlers/`.
Cross-checked against RFC 1813 and Linux `fs/nfsd/{nfs3proc,nfs3xdr}.c`. READ-ONLY audit; no source modified.

Overall the handlers are disciplined about the architecture invariants: business logic (permission checks,
traversal, dedup) lives in `pkg/metadata`; handlers do XDR/type-conversion + WCC capture + error mapping;
`*metadata.AuthContext` is threaded everywhere; handles are treated as opaque (only share/id decode for
routing, never path parsing); WRITE follows the prescribed Prepare→BlockStore→Commit order. The findings
below are mostly correctness/interop drift and resource concerns, not invariant violations.

---

**[HIGH] WCC pre-op attrs (`wccBefore`) are captured AFTER a separate GetFile, not atomically with the mutation — TOCTOU window corrupts client cache** — `remove.go:115-121`, `rename.go:146-152,184-197`, `mkdir.go:142-148`, `rmdir.go:125-132`, `create.go:127-133`, `setattr.go:142-148`, `symlink.go:138-144`, `mknod.go:180-186`, `link.go:188-195`, `commit.go:135-142`
Every mutating handler does `GetFile`/`getFileOrError` to read current attrs, builds `wccBefore` from that snapshot, then calls the store mutation. RFC 1813 §2.6 wcc_data requires pre-op attrs to reflect the file state *immediately before* the operation so the client can chain `before == its cached after`. Because the pre-op read and the mutation are two separate store calls with no lock held in the handler, a concurrent mutation between them yields a `wccBefore` that never actually preceded this op → client silently keeps a stale cache (the exact failure WCC exists to prevent). Linux nfsd captures pre-op attrs inside the same `fh_lock`/vfs operation. Suggested fix: have the metadata-store mutation methods return pre-op attrs (as `PrepareWrite` already does for WRITE via `writeIntent.PreWriteAttr`) and use those instead of a pre-read; i.e. extend the `RemoveFile`/`Move`/`CreateDirectory`/`RemoveDirectory`/`SetFileAttributes` contracts to surface WCC-before. WRITE (`write.go:253`) is the correct model.

**[HIGH] WRITE rejects any request larger than a hardcoded 1 MB with NFS3ERR_FBIG, ignoring the store's advertised `wtmax`** — `write.go:152-156` + `write_validation.go:64-72`
`const maxWriteSize uint32 = 1 << 20` is hardcoded and oversized writes return `NFS3ErrFBig`. But FSINFO advertises `capabilities.MaxWriteSize` from store config (`fsinfo.go:192`), which can be > 1 MB. If an operator configures a larger wtmax, a spec-conformant client that respects the advertised wtmax will have valid writes rejected. Two bugs: (1) the cap is decoupled from the value advertised in FSINFO (the comment claims "matches default config" but nothing enforces that); (2) FBIG ("file too large") is the wrong code for an over-large *request* — Linux/RFC expect the server to advertise wtmax and either accept or short-write, not FBIG. Suggested fix: derive the cap from `GetFilesystemCapabilities().MaxWriteSize` (cache it once at adapter init), and on overflow either short-write up to wtmax or return `NFS3ErrInval`, not FBIG.

**[HIGH] READDIRPLUS does not enforce the client's `maxcount` byte budget — response can exceed the limit the client allocated for** — `readdirplus.go:279,302-365` + `readdirplus_codec.go:155-251`
The handler calls `ReadDirectory(authCtx, dirHandle, req.Cookie, req.DirCount)` (sizing the page by *dircount*, which per RFC bounds only name+cookie bytes) then encodes **all** returned entries plus their full fattr3 + filehandle, with no running tally against `req.MaxCount`. `Encode()` likewise emits every entry unconditionally. RFC 1813 §3.3.17: "maxcount … the maximum size of the READDIRPLUS3resok structure"; the server must stop adding entries before exceeding it and set eof=false. A directory whose attrs+handles overflow the client's maxcount buffer can produce a reply the client rejects/truncates, breaking large-directory listing. READDIR (`readdir.go:237`) has the same shape but is less acute (no per-entry attrs/handles). Suggested fix: track encoded bytes while building entries, stop at `DirCount`/`MaxCount` respectively, and set `eof=false` + truncate, so the client re-issues from the last accepted cookie.

**[MED] READDIRPLUS uses uncached `GetFile` for the directory and per-entry, and falls back to per-entry `Lookup`/`GetFile` — hot-path amplification on large dirs** — `readdirplus.go:191,326,341`
Unlike READDIR/SETATTR/COMMIT which use `getFileOrError`/`GetFileCached`, READDIRPLUS calls `metaSvc.GetFile` directly for the dir (line 191) and, when `entry.Attr`/`entry.Handle` are unpopulated, does a per-entry `Lookup` (326) or `GetFile` (341). On a backend where `ListChildren` doesn't pre-populate attrs/handles this becomes O(n) store round-trips per READDIRPLUS — the operation READDIRPLUS exists to avoid. Suggested fix: use the cached getter for the dir, and treat unpopulated entry attrs as a contract violation worth a single warning + bounded batch fetch rather than silent per-entry fan-out.

**[MED] CREATE/MKNOD/MKDIR/LINK do a non-atomic Lookup-then-create existence check (TOCTOU) and CREATE ignores client atime/mtime even in EXCLUSIVE mode** — `create.go:174-291,393-457`, `mkdir.go:195-224`, `mknod.go:228-242`, `link.go:224-238`
(a) Existence is probed with `metaSvc.Lookup` then the file is created in a second call; two clients racing GUARDED/MKDIR can both pass the probe. The store's atomic create is the real guard (it returns `ErrExist`), so the handler probe is redundant *and* races — but it can also mask the store's authoritative result. Rely on the store's atomic create + `ErrExist→NFS3ERR_EXIST` mapping and drop the probe (or keep it purely as a fast-path, never as the decision). (b) `createNewFile` (`create.go:401-457`) only applies Mode/UID/GID via `applySetAttrsToFileAttr` (line 566-578 explicitly drops Size and never sets Atime/Mtime). RFC 1813 §3.3.8 permits the client to set atime/mtime on CREATE (all modes); the dedicated-`IdempotencyToken` design note even claims this is preserved, but the code path discards them. Suggested fix: thread `req.Attr.Atime/Mtime` into `createNewFile`.

**[MED] EXCLUSIVE-create idempotency token is not stored when client verifier is 0, and retry-match requires `Verf != 0`** — `create.go:223,450-452`
`createNewFile` stores the token only `if req.Mode == types.CreateExclusive && req.Verf != 0`, and the retry path matches only `if existingFile.IdempotencyToken == req.Verf && req.Verf != 0`. A client legitimately sending an all-zero verifier (rare but legal — the verifier is opaque 8 bytes) gets no idempotency: a retried EXCLUSIVE create after a lost reply returns `NFS3ERR_EXIST` instead of success. Suggested fix: track "verifier present" separately from value, or store unconditionally for EXCLUSIVE.

**[MED] FSSTAT free-bytes computed by unchecked subtraction — underflow wraps to ~16 EB if used > total** — `fsstat.go:190,199`
`freeBytes := stats.TotalBytes - stats.UsedBytes` and `Ffiles: stats.TotalFiles - stats.UsedFiles` are unchecked. If a backend ever reports `UsedBytes > TotalBytes` (over-provisioned/thin S3, accounting skew), the uint64 subtraction wraps and the client sees a near-infinite free space. Suggested fix: clamp with `if used > total { free = 0 }`.

**[MED] `buildWccAttr`/`CaptureWccAttr`/`MetadataToNFS` truncate 64-bit Unix seconds to uint32 — breaks after 2038** — `utils.go:70-82`, `xdr/attributes.go:94-105,270-280`
All time conversions do `uint32(attr.Mtime.Unix())`. nfstime3 seconds is unsigned 32-bit per RFC 1813, so this is wire-correct *today*, but timestamps past 2106 (and negative pre-1970) wrap. Linux nfsd has the same wire constraint but guards against negative/overflow. Low urgency, noting for completeness — flag if v1.0 wants Y2038+ safety. (Borderline LOW.)

**[MED] `Fsid` is hardcoded to 0 for every file across all shares/exports** — `xdr/attributes.go:64,92`
`MetadataToNFS` always sets `Fsid: 0`. Block stores are per-share and multiple exports are served by one adapter; with a single shared FSID, clients that key cache/mount identity on (fsid,fileid) can alias objects across distinct shares when fileids collide between metadata stores. Linux nfsd derives fsid per export. Suggested fix: derive a stable per-share fsid (hash of share name) and plumb it through `convertFileAttrToNFS`.

**[LOW] `truncateExistingFile` swallows block-store truncate errors as "non-fatal", leaving metadata size and content out of sync** — `create.go:543-551`
UNCHECKED-create-over-existing updates metadata size to target, then truncates content best-effort; on truncate failure it logs a Warn and returns success. The client sees size=0 (or target) but stale bytes remain in the block store until GC — a subsequent READ can return wrong data length vs. attrs. Same best-effort pattern in REMOVE content delete (`remove.go:227-239`) is acceptable (orphan only), but truncate divergence is a data-correctness issue. Suggested fix: fail the CREATE (return IO) or order truncate before the metadata size commit.

**[LOW] READ does not clamp `Count` to the advertised `rtmax` before computing the read window** — `read.go:128-231` + `read_validation.go:55-63`
Validation caps Count at 1 GB; the handler then clamps to file size (`min(offset+count, file.Size)`) which bounds the actual allocation, so this is not a DoS in practice. But RFC 1813/Linux clamp Count to rtmax and the server's advertised value is `capabilities.MaxReadSize`; a client ignoring rtmax gets served an arbitrarily large single read up to file size. Minor; the file-size clamp makes it benign. Suggested fix: clamp `actualLength` to advertised rtmax for symmetry with WRITE/FSINFO.

**[LOW] WRITE always returns `committed=UNSTABLE` regardless of requested `stable` (incl. FILE_SYNC)** — `write.go:303-322`
Documented design decision (cache always on, durability via COMMIT). RFC-permissible since `committed` reports what the server actually did and `stable` is advisory. Noted only because a client requesting FILE_SYNC for a single durable write now *must* issue a COMMIT it didn't expect to need; correct per spec but worth a conformance note. No change required.

**[LOW] READDIR/READDIRPLUS cookieverf mismatch is intentionally ignored (never returns NFS3ERR_BAD_COOKIE)** — `readdir.go:173-179`, `readdirplus.go:223-229`
Deliberate (mtime-based verifier changes on every dir write; returning BAD_COOKIE broke macOS Finder, matches Linux knfsd leniency). Correct trade-off but means a genuinely stale cookie after heavy churn silently resumes at a possibly-shifted position (entries skipped/duplicated). Acceptable; documented inline. No change.

---

Severity tally: 3 HIGH, 6 MED, 4 LOW.
