# Plan — #1285 NFSv4.2 xattr + unified cross-protocol xattr abstraction

## Context

#1248 shipped SMB xattr (named streams) + capability honesty (PR #1286, develop `0cfa5206`). **NFS still has no xattr surface** — xattr ops exist only in NFSv4.2 (RFC 8276); DittoFS does v3/v4.0/v4.1 only. This delivers NFSv4.2 xattr **and** unifies the three xattr representations behind one metadata abstraction so a value set on one protocol is visible on the others (Mac↔NFS parity).

**Design decided with user (brainstorming):**
- **Unify all three** xattr stores behind a shared metadata abstraction; each adapter translates to it (the existing metadata→adapter pattern).
- **Named streams STAY full file entities** — DittoFS's `smb2.streams` 14/14 conformance depends on stream identity (handles, share modes, locks, delete-on-close), and streams carry arbitrary-size content. So unification = a **resolver/view layer over two physical backings**, NOT a storage merge.
- **Skip pNFS** (a v4.1 optional feature, unrelated; DittoFS already omits it). v4.2 features are independent/optional — implement xattr only, `NOTSUPP` the rest.

**Outcome:** Linux `setfattr`/`getfattr` over `mount -o vers=4.2` round-trips and survives restart; a Finder tag / `xattr -w` set on a Mac over SMB is readable via NFS `getfattr` (and vice-versa) for shareable names.

⚠️ Read/verify all facts via `git show origin/develop:` (local tree diverged). Work in a fresh worktree off `origin/develop`.

## Architecture — unified resolver over two backings

One canonical xattr namespace per inode, backed by:
1. **Inline K/V** = `FileAttr.EAs` (`pkg/metadata/file_types.go`) — small values (≤64 KiB), default home for new xattrs. Reuse `LookupEA`/`ApplyEAMutations`/`findEAKey` (case-insensitive, casing-preserving).
2. **Stream entities** — colon-named `base:name` child Files (block-backed); unchanged. Enumerated via `ListChildren` colon-prefix scan (mirror `query_info.go:buildFileStreamInformation` ~963). Content read via `Service.GetBlockStoreForHandle` + `engine.Store.ReadAt` (`pkg/block/engine/readwrite.go`).

**Resolver** presents one namespace: `Get` finds in either backing; `List` merges both; **precedence stream-wins-else-inline**; `Set` writes ≤64 KiB inline, larger → stream (or `NFS4ERR_XATTR2BIG` if the NFS adapter forbids spill — see PR2). All three adapters write to these same backings, so they all see each other — **no SMB handler change needed** (SMB EA already writes `FileAttr.EAs`; SMB streams already create entities; the resolver just reads both).

## PR sequence

### PR1 — Unified xattr abstraction in the metadata layer (no protocol change)
- Add to `Files` interface (`pkg/metadata/store.go`): `GetXattr / SetXattr / RemoveXattr / ListXattr(ctx, handle, …)`. Signatures: `GetXattr→([]byte,bool,error)`; `ListXattr(cursor,limit)→(names,nextCursor,error)`.
- New `pkg/metadata/xattr.go` — resolver: inline path reuses `ApplyEAMutations`/`LookupEA`; stream path enumerates colon-children + reads content. Document precedence + 64 KiB inline threshold.
- Service wrapper in `pkg/controlplane/runtime/shares/service.go` for block-store access when surfacing stream-backed values (`GetBlockStoreForHandle`).
- Implement across **memory/badger/postgres** (inline trivially rides existing EA JSON/JSONB; stream enumeration uses existing `ListChildren`/`GetChild`).
- Conformance: new `pkg/metadata/storetest/xattr_ops.go` (inline get/set/delete, stream-backed get/list, merged List, case-insensitive, large-value→stream), wired into the suite for all 3 backends (mirror `ea_ops.go`).
- **No SMB change** — assert existing `ea_ops.go` + `smb2.streams` stay green.

### PR2 — NFSv4.2 minorversion + 4 xattr ops
- **Constants** (`internal/adapter/nfs/v4/types/constants.go`): `OP_GETXATTR=72/SETXATTR=73/LISTXATTRS=74/REMOVEXATTR=75`, `NFS4_MINOR_VERSION_2=2`, `NFS4ERR_XATTR2BIG=10095` (verify number vs RFC 8276) `+NFS4ERR_NOXATTR=10095/…`, `OpName` cases, ACCESS bits `ACCESS4_XAREAD/XAWRITE/XALIST`.
- **Minorversion** (`compound.go` `ProcessCompound` ~316/325 + `maxValidOpCode`; `handler.go` `maxMinorVersion`→2): accept minorversion 2; route via `isV42` flag (reuse v4.1 loop) or a small `v42DispatchTable`; gate ops 72–75 to v4.2 only. `adapter_settings.go` validation `[0,1]→[0,2]`.
- **4 handlers** (`internal/adapter/nfs/v4/handlers/{getxattr,setxattr,listxattrs,removexattr}.go`) → call the metadata Service Xattr methods. **RFC 8276 XDR (corrected):**
  - GETXATTR: args `name`; res `value` (opaque). Missing → `NFS4ERR_NOXATTR`.
  - SETXATTR: args `setxattr_option4` (CREATE/REPLACE/EITHER) + `name` + `value`; res `change_info4`. **No stateid.** Honor option (CREATE-on-existing→`EXIST`; REPLACE-on-missing→`NOXATTR`).
  - LISTXATTRS: args `cookie`(uint64) + `maxcount`; res `cookie` + `names<>` + `eof`. **No stateid.**
  - REMOVEXATTR: args `name`; res `change_info4`. **No stateid.** Missing→`NOXATTR`.
  - `change_info4` uses the **file's** change attr before/after (reuse `encodeChangeInfo4` helper ~helpers.go:211). Pseudo-fs → `NFS4ERR_NOTSUPP`/`ROFS`.
- **Namespace** — Linux NFS client only carries the `user.` namespace and strips the prefix on the wire. Adapter **canonicalizes to `user.<name>`** before the store and strips on read → matches SMB EA/stream names so `user.*` is the shared cross-protocol namespace. Reject non-user namespaces → `NFS4ERR_NOXATTR`/`ACCES`.
- **Capability** (`attrs/encode.go`): add `FATTR4_XATTR_SUPPORT` (bit 82, word 2) → true; set in `SupportedAttrs()` + encode case.
- Per-op auth via `buildV4AuthContext`; map store errors (ErrAccess→`ACCES`, ErrNoEntity→`NOXATTR`, size→`XATTR2BIG`).

### PR3 — Cross-protocol parity + interop tests + docs
- Linux **e2e** (`test/e2e`): `mount -o vers=4.2`; `setfattr -n user.foo -v bar` / `getfattr` round-trip + server restart (badger). Gate like other e2e (sudo/kernel client).
- **Parity test** (in-process/CI): value written via SMB (EA *and* stream) is readable via NFS `getfattr`, and an NFS-set `user.*` is readable via SMB — proves the resolver unification.
- Docs: `docs/NFS.md` (v4.2 xattr + `user.` namespace + size limit), `docs/FAQ.md` (NFS xattr now supported), regenerate `docs/CLI.md` if minorversion settings surface changes.

### PR4 / live gate — Scaleway VM real cross-protocol acceptance (per AD-work precedent)
CI uses kernel clients in containers; a real VM is the honest proof the feature works end-to-end and is cross-compatible. Provision **one** scw VM (Ubuntu 24.04, kernel ≥6.8 for NFSv4.2 + cifs); run DittoFS with a **badger** metadata store and **one share** exported over **both NFS and SMB** simultaneously. Terminate the VM after (precedent: AD acceptance VMs torn down). Runbook → `~/.claude/plans/`.
Acceptance matrix (same file, same share, both mounts):
1. **NFS native**: `mount -t nfs -o vers=4.2 …`; `setfattr -n user.foo -v bar f` → `getfattr -n user.foo f` == bar.
2. **SMB native**: `mount -t cifs …` (or `smbclient`); set/read a `user.foo` xattr over SMB.
3. **Cross SMB→NFS**: set `user.tag` over the SMB mount → read identical value via `getfattr` on the NFS mount.
4. **Cross NFS→SMB**: set `user.tag2` over NFS → read via SMB.
5. **macOS-stream parity** (if a Mac/`xattr` path is reachable, else simulate via SMB named-stream create): a stream-backed xattr is visible through NFS `getfattr` (resolver stream-read path).
6. **Restart persistence**: `setfattr`, restart `dfs`, re-mount both, re-read on both protocols.
7. **Limits/negatives**: oversized value → `NFS4ERR_XATTR2BIG`/EFBIG; non-`user.` namespace rejected; `vers=4.1` mount sees no xattr op (graceful).
Capture before/after command transcripts in the runbook. scw live-ops lessons (admin pw via env `DITTOFS_ADMIN_INITIAL_PASSWORD`; `dfs` daemonizes; `pkill -x`) carried from AD memory.

## Critical files
- Metadata: `pkg/metadata/store.go` (interface), `pkg/metadata/xattr.go` (NEW resolver), `pkg/metadata/file_types.go` (reuse EA helpers — no change), `pkg/metadata/store/{memory,badger,postgres}/` (implement), `pkg/metadata/storetest/xattr_ops.go` (NEW), `pkg/controlplane/runtime/shares/service.go` (block-access wrapper).
- NFS: `internal/adapter/nfs/v4/types/constants.go`, `internal/adapter/nfs/v4/handlers/{compound,handler}.go`, `…/handlers/{getxattr,setxattr,listxattrs,removexattr}.go` (NEW), `…/attrs/encode.go`, `internal/controlplane/api/handlers/adapter_settings.go`.
- SMB: **none** (resolver reads the same backings SMB already writes).

## Verification
1. `go test ./pkg/metadata/...` — new `xattr_ops.go` green on memory/badger/postgres; existing `ea_ops.go` unchanged.
2. `go test ./... && go test -race ./...`; per-op NFS XDR encode/decode unit tests (mirror existing v4 handler tests); `go vet`/`go fmt`.
3. **SMB no-regress**: `test/smb-conformance/smbtorture/run.sh --filter smb2.streams` stays 14/14 (run serially, `docker rm -f` between; full-suite for `create_no_streams` share routing).
4. **NFS e2e**: `cd test/e2e && sudo ./run-e2e.sh` with a vers=4.2 + `setfattr`/`getfattr` round-trip + restart.
5. **Cross-protocol parity** (PR3): SMB-set xattr (EA + Mac-style stream) readable via NFS `getfattr` and vice-versa.
6. **Live scw VM acceptance** (PR4): one VM, one share exported over NFS+SMB at once; run the 7-step matrix above (native both, cross both directions, stream parity, restart, negatives); VM terminated after, transcripts in the runbook.

## PR hygiene
Worktree off `origin/develop`; signed commits (`git -c user.signingkey=~/.ssh/id_rsa -c gpg.format=ssh commit -S`); per PR: simplifier + reviewer + Copilot + green CI + squash-merge. After approval: **update issue #1285** with this plan, then implement PR1→PR2→PR3.
