# Trash / Recycle Bin — Design

- **Issue:** #190
- **Date:** 2026-06-01
- **Status:** Approved (design)

## Summary

A per-share, opt-in recycle bin that mimics the Synology / classic-NAS experience:
deleted files move into a real, browsable `#recycle` directory at the share root
instead of being destroyed immediately. Users restore by moving a file back out
over the same NFS/SMB mount; operators tune retention and purge via `dfsctl`. The
feature targets edge-NAS / CTERA-style deployments where a self-service safety net
is a primary value, not an operator-only tool.

The design reuses the existing atomic, metadata-only `Move` primitive, so deletion
becomes a metadata move (faster deletes — a stated #190 motivation) and block
deletion is **deferred for free**: recycled nodes simply stay in the tree until
reaped, at which point the normal delete path frees their CAS blocks.

## Goals

- Real, visible `#recycle` directory per share — restorable over NFS and SMB with
  no special tooling (drag the file back in Finder/Explorer).
- Per-share `enable-trash` flag, default **off**, toggleable on a live, already
  populated share at runtime (not only at share creation).
- Recycle on unlink (NFS `REMOVE`/`RMDIR`, SMB delete-on-close) **and** on
  replace-overwrite (a rename/copy that would clobber an existing file → the victim
  is recycled). In-place truncates/writes are *not* trapped.
- Configurable per share: `retention-days`, `restrict-empty-to-admin`, `max-size`,
  `exclude-patterns`.
- Single shared `#recycle` per share, preserving each item's original path subtree
  and the deleting user's ownership.
- Disabling trash **auto-empties** the bin (permanent delete of all recycled items
  + frees their deferred blocks, then stops recycling).
- `dfsctl trash` admin surface: list / restore / empty / status.

## Non-Goals

- Per-user bins (single shared `#recycle` only — accepted Synology trade-off).
- Versioning / recycling of in-place overwrites or truncates.
- Cross-share trash; the bin is strictly per-share at its own root.
- A hidden/CLI-only trash (rejected: forfeits the self-service UX that is the point).

## Decisions

| Decision | Choice | Rationale |
|---|---|---|
| Storage model | Real `#recycle` dir via `Move` (Approach A) | Smallest change; browse/restore free over both protocols; defers block deletion as a side effect; reuses tested primitive |
| Visibility | Genuinely visible (not Hidden-attributed) | Discoverability *is* the feature for non-technical edge users |
| Opt-in | Per-share `enable-trash`, default off, live-toggleable | Matches Synology per-folder model; #220 established the live config-push pattern |
| Recycle triggers | Unlink + replace-overwrite victim | Mirrors Synology; in-place writes excluded |
| Bin scoping | Single shared bin per share, preserve path + owner | Synology default; per-user deferred |
| Disable | Auto-empty the bin | No lingering orphan bin |
| Block deletion | Deferred until reap/purge | Recycled node stays in tree; real delete on reap frees blocks |

## Architecture

A new `trash` sub-service under `pkg/controlplane/runtime/trash/`, following the
existing sub-service pattern (`shares/`, `clients/`). Protocol adapters are
untouched: NFS v3/v4 and SMB already converge at `MetadataService.RemoveFile`,
`RemoveDirectory`, and `Move`, so the recycle trap lives **below** the protocol
layer inside those service methods, gated on the share's `TrashEnabled`. One trap,
all protocols.

```
REMOVE/RMDIR/delete-on-close ─┐
replace-overwrite (Move) ─────┼─> MetadataService.{RemoveFile,RemoveDirectory,Move}
                              │      └─ if TrashEnabled && !inRecycle && !excluded:
                              │            recycle()  → Move node into #recycle/<orig-path>, stamp trash meta
                              │         else: real delete (frees blocks)
Runtime ──> trash.Service ────┴─> Restore / Empty / Reaper(retention + max-size) / accounting
lifecycle.Serve ──> trash.Service.Start(ctx)   lifecycle.shutdown ──> trash.Service.Stop()
```

## Components

- **`trash.Service`** — `Recycle(ctx, share, file)`, `Restore(ctx, share, binPath, dest)`,
  `Empty(ctx, share, force)`, `Start(ctx)/Stop()` (reaper). Holds a per-share config
  snapshot + incremental size accounting.
- **Recycle hook** — a small branch in `RemoveFile` / `RemoveDirectory` /
  `Move`-overwrite delegating to `Recycle` when enabled.
- **Reaper** — ticker goroutine using the lifecycle `Start/Stop` pattern (as in
  `clients.Registry.StartSweeper`). Per tick, per trash-enabled share: evict items
  older than `retention-days`, then evict oldest-first until under `max-size`.
  Reaping calls the real `RemoveFile`, which frees CAS blocks.
- **Bin resolver** — lazily ensures `#recycle` exists at the share root on first
  recycle (or on enable); resolves the original-path subtree under it.
- **Guard layer** — recursion / permanence / exclude rules (see below).
- **REST + apiclient + dfsctl** — config knobs on the share + a `dfsctl trash` verb
  group.

## Config Surface

Add to `shares.Share` + `ShareConfig` (`pkg/controlplane/runtime/shares/service.go`),
persisted in the shares DB row, runtime-toggleable via a `SetShareTrashConfig`
live-push under the share lock (mirroring #220's `SetShareNetgroup`):

```go
TrashEnabled         bool      // "enable-trash", default false
TrashRetentionDays   int       // 0 = keep forever (manual empty only)
TrashRestrictToAdmin bool      // only admins empty/force-delete; users may still restore
TrashMaxBytes        int64     // 0 = unbounded; else oldest-first eviction
TrashExcludePatterns []string  // globs deleted immediately, never recycled
```

Exposed via the existing share-edit handler (`GET/PATCH /api/v1/shares/{name}` — no
new endpoint) and `dfsctl share edit --enable-trash --trash-retention-days N
--trash-restrict-empty-to-admin --trash-max-size SZ --trash-exclude '*.tmp'`. All
trash settings are no-ops while `TrashEnabled=false`.

## Data Model

Recycled nodes stay as real files in the tree. Add three nullable fields to the file
metadata record (`FileAttr`), with a Postgres migration `000027` and badger JSON
tags (memory backend is plain struct fields):

```go
DeletedAt    *time.Time `json:"deleted_at,omitempty"`    // recycle timestamp → retention age
OriginalPath string     `json:"original_path,omitempty"` // restore target
DeletedBy    string     `json:"deleted_by,omitempty"`    // AuthContext principal (display / per-restore)
```

These are set only on the top-level recycled entry (the moved node's root). A node
with `DeletedAt != nil` living under `#recycle` is a trash item — this is also how the
reaper and `dfsctl trash list` enumerate, with no side table.

## Data Flow

### Recycle (delete)

In `RemoveFile` / `RemoveDirectory`, when `TrashEnabled && !inRecycle(path) && !excluded(name)`:

1. Ensure `#recycle` exists (lazy mkdir at share root; mode/ACL per `restrict-to-admin`).
2. Compute bin dest = `#recycle/<original-relative-path>`; create intermediate dirs.
3. On name collision in the bin → suffix ` (<deleted_at unix>)` (Synology-style).
4. `Move(node → dest)` — atomic, metadata-only; **blocks untouched → deferred**.
5. Stamp `DeletedAt=now`, `OriginalPath=<orig>`, `DeletedBy=ctx.Principal` on the moved root.
6. `RMDIR` of a non-empty directory recycles the **whole subtree** as one entry
   (single `DeletedAt` on the subtree root).

### Replace-overwrite

In `Move` (and SMB rename-with-replace / create-overwrite disposition), when the
destination exists and would be clobbered → recycle the **victim** first (same
steps), then proceed with the rename. Truncates / in-place writes are not trapped.

### Restore

`Move(binEntry → OriginalPath)`, then clear the three trash fields.

- Conflict (original path occupied) → default **fail** with `ErrExist` + a clear
  message; `dfsctl trash restore --to <path>` overrides the destination.
- Original parent directory missing → recreate the parent chain (Synology recreates).
- Permissions: restore re-asserts the entry's stored ACL/owner. `restrict-to-admin`
  does **not** block restore (users restore their own) — only empty/purge.

## Reaper (retention + max-size)

A ticker goroutine via lifecycle `Start(ctx)` / `Stop()`. Default interval hourly
(`min(retention granularity, 1h)`). Per tick, for each trash-enabled share:

1. **Retention:** enumerate `#recycle` entries with `DeletedAt != nil`; any
   `now - DeletedAt > retention-days` → permanent `RemoveFile` (frees CAS blocks).
   `retention-days=0` → skip (manual only).
2. **Max-size:** if `TrashMaxBytes > 0` and bin total > cap → evict oldest `DeletedAt`
   first until under cap.
3. **Accounting:** bin size tracked incrementally (add on recycle, subtract on
   reap/restore/empty); recomputed by a walk on startup.

Reaping reuses the real delete path, so block-GC / refcount / snapshot-hold
semantics already hold — no new block logic.

## Guard Layer

- **`#recycle` is reserved:** cannot be recycled, renamed, or recycled into itself.
- **Deletes *inside* `#recycle` are permanent** (`inRecycle(path)` short-circuits the
  trap) — prevents infinite nesting and is how manual purge-over-the-mount works.
- **Exclude-patterns:** a `name` matching a configured glob bypasses the bin
  (immediate delete).
- **Recursion/depth:** original-path subtree recreation under the bin is bounded by
  source depth; no symlink following.
- **Cross-share:** the bin is strictly per-share at its own root; never spans shares.

## Disable → Auto-empty

On `SetShareTrashConfig(enabled=false)`: flip `TrashEnabled=false` under the share
lock (stops trapping immediately), then enqueue a full bin purge on the reaper —
permanent `RemoveFile` of every `#recycle` entry plus removal of the `#recycle` dir,
freeing all deferred blocks. Running the purge on the reaper keeps a slow purge from
blocking the config call; progress is surfaced in `dfsctl trash status`.

## Cross-protocol Concerns

- **Naming:** `#recycle` (Synology convention; sorts high; `#` legal on NFS and SMB).
- **READDIR:** no entry filtering needed (it is a real, visible directory). The share
  **root** shows `#recycle` only while `TrashEnabled`.
- **Perms:** recycled items keep their original ACL/owner so a user sees and restores
  their own; `DeletedBy` drives display. `restrict-to-admin` sets the bin-dir ACL so
  non-admins cannot purge.
- **Entry points:** SMB delete-on-close and NFS silly-rename both funnel through
  `RemoveFile`, so they are trapped uniformly.

## `dfsctl` Surface

Mirrors the `share` / `store block` cobra groups:

```
dfsctl trash list <share> [-o json|table]        # path, size, deleted_at, deleted_by, expires_in
dfsctl trash restore <share> <bin-path> [--to P]  # move back
dfsctl trash empty <share> [--force]              # purge all (admin-gated if restrict-to-admin)
dfsctl trash status <share>                       # bin size, item count, oldest, next-reap, purge state
```

Backed by REST `/api/v1/shares/{name}/trash[...]`, apiclient methods, and runtime →
`trash.Service`.

## Error Handling

Use `metadata.ExportError` codes throughout:

- restore conflict → `ErrExist`
- trash op on a disabled share → `ErrNoEntity`
- non-admin purge under `restrict-to-admin` → `ErrAccess`
- recycle of `#recycle` itself → `ErrAccess`

A recycle failure must **not** be swallowed: if `Move`-to-bin fails, surface the
error — never silently hard-delete and never silently leak. Expected errors log at
`Debug`, unexpected at `Error`.

## Testing

- **Conformance (`pkg/metadata/storetest/`, all of memory/badger/postgres):** recycle
  round-trip, restore, collision suffix, subtree recycle, retention reap frees
  blocks, max-size eviction, exclude bypass, `inRecycle` permanence,
  disable-auto-empty. (Closes the cross-backend gap area #6 flagged.)
- **Block deferral:** assert blocks survive recycle and are freed only on reap/purge
  (byte-verify; ties into the existing snapshot/GC matrix).
- **Adapter:** NFS `REMOVE`/`RMDIR` + SMB delete-on-close + rename-replace all land in
  the bin; deletes inside the bin are permanent.
- **Reaper:** time-injected retention expiry; max-size oldest-first eviction.
- **E2E:** mount, `rm` over NFS/SMB, see the item in `#recycle`, restore it, verify
  bytes identical.

## Build Sequence (high level)

1. Metadata fields + migration `000027` + storetest conformance cases (red).
2. `trash.Service` recycle/restore/empty + guard layer; wire the hook into
   `RemoveFile`/`RemoveDirectory`/`Move`.
3. Reaper + lifecycle Start/Stop + accounting.
4. Share config fields + `SetShareTrashConfig` live-push + REST share-edit wiring.
5. `dfsctl trash` group + apiclient + REST trash endpoints.
6. Disable-auto-empty.
7. E2E + docs (`CLI.md`, `CONFIGURATION.md`, `ARCHITECTURE.md`, `FAQ.md`).
