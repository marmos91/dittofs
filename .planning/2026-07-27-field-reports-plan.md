# Field reports 2026-07-27 — #1850 (P0 zeros), #1843 (backup/gc-state cleanup), Proxmox report

Three independent reports. **Streams A / B / C run in parallel** (no shared files). Within a
stream, steps are ordered.

---

## Stream A — P0 #1850: share serves all-zeros after the pre-journal→journal migration

### Root cause (verified in code, v0.29.0)

The cold-interval marker that makes a remote-only read fetch instead of zero-fill is
**memory-only and is lost on the first restart after the migration**:

1. `pkg/block/local/fs/fs.go:101` (`MigrateLegacyLayout`, remote-backed shares) →
   `archiveLegacyLayout` renames `blobs/`+`logs/` to `*.pre-journal-backup`; the journal opens
   empty and `migratedFromLegacy = true` (a plain in-memory bool, `fs.go:143`).
2. `pkg/controlplane/runtime/shares/service.go:1192` — **only on that one migrating open** —
   calls `SeedColdFromManifest` (`service.go:3094`), which walks the manifest and calls
   `bs.SeedCold` per extent.
3. `pkg/block/journal/store.go:363 SeedCold` does **only** `fi.insert(interval{cold: true})`.
   No record is appended, so nothing lands in `.seg`/`.idx`. Recovery rebuilds the index from
   segment records alone → **every cold interval evaporates on restart**.
4. On the next start `hasLegacyLocalLayout` is false (dirs already renamed) → no reseed. The
   journal index is empty → `fileIndex.plan` (`index.go:255`) classifies the range as a `hole`
   → `Store.ReadAt` zero-fills and returns `cold=false` → `readAtInternal`
   (`read_internal.go:45`) returns those zeros. **No fetch, no error, correct length.**
   Exactly the reported `rx 0 KB / non-zero 0 of 1048576`.

Same latent bug on the snapshot-restore path (`snapshot.go:1584` seeds the same way): reads work
until the first restart, then zeros.

### A1 — field recovery for the reporter (no new build; do first, it unblocks them)

`Syncer.WarmAll` (`pkg/block/engine/warm.go:52`) walks the **manifest**, not cold intervals, and
`Hydrate`s each chunk as a real journal record — so it is durable and immune to this bug.
`dfsctl share warm` is therefore the working recovery path today. Disk is the constraint (4 GiB
free), so sequence it:

1. Reclaim the non-golden cruft only: stale `gc-state` (7.2 G) + old `snapshots` (1.4 G).
   **Verify first** what `gc-state` deletion costs (`pkg/block/engine/gcstate.go`) before advising.
   → ~11 GiB free, `*.pre-journal-backup` untouched.
2. `dfsctl share warm` (partial is fine) → read a known file → assert non-zero **and** byte-compare
   against the same file's bytes in `blobs.pre-journal-backup`. This is the end-to-end content
   check they asked for, and it proves the Cubbit copy + manifest are good.
3. Only then delete the two `*.pre-journal-backup` dirs (→ ~62 GiB back), raise the local cap to
   ≥ share size, re-run `warm` to completion for full offline reads (feeds their #1843 24/24 test).

### A2 — the fix: stop depending on an in-memory cold marker

The manifest is the durable authority for "these bytes exist"; the journal index is a cache.
Make the read path consult it when it is about to serve a hole.

- `pkg/block/journal/store.go` — `ReadAt` also reports whether any piece was a `hole`
  (`plan()` already computes it; just surface it).
- `pkg/block/engine/read_internal.go` — `readAtInternal`: `hole && bs.HasRemoteStore()` takes the
  same branch as `cold`. `ensureAndReadFromLocal` → `EnsureAvailableAndRead` already resolves the
  covering `FileChunk` and hydrates, and already leaves a genuine sparse hole (no covering chunk)
  zero-filled — RFC-safe, no new logic.
- Fail closed: if a range the manifest **does** cover is still a hole after hydrate, return an
  error. Never silently serve zeros for manifest-covered bytes.
- **Delete** `SeedCold`, `SeedColdFromManifest`, the `MigratedFromLegacy()` gate at
  `service.go:1192` and the restore-path seed at `snapshot.go:1584` — all dead once the read path
  self-heals. Net deletion, and it kills the restore variant of the bug too.

Cost: one covering-chunk lookup per read that contains a hole, remote-backed shares only (badger
has the indexed `GetFileChunkAtOffset`). Warm reads are untouched.

### A3 — tests

- Engine unit test: remote-backed store + populated manifest + **empty journal** (post-restart
  state) → read returns the remote bytes, not zeros. This is the regression that would have caught
  #1850 and the restore variant.
- Sparse hole with no covering chunk → zeros, no fetch (keeps POSIX sparse behaviour).
- Restart-survival test over the legacy-migration fixture in
  `pkg/controlplane/runtime/shares/legacy_migrate_test.go`: migrate, close, reopen, read.

### A4 — migration should verify itself (the reporter's own point)

After a migration/restore, sample-read N extents and compare against the manifest hash before
logging success, and never log "migrated" on the strength of a rename alone. Keep the archive
until that check passes.

---

## Stream B — #1843 reply (no code): backups + gc-state

Their two questions, now answerable because Stream A explains the 18 MB journal / `Blocks Local 0`:

1. **Are `*.pre-journal-backup` safe to delete?** Yes in principle for a remote-backed, fully
   drained share (every block is on Cubbit, manifest intact) — but **not before the A1 content
   check passes on this box**, because right now the journal is empty and reads are broken, which
   is exactly when a golden copy earns its keep. Give them the A1 sequence. Then: the migration
   should delete the archive itself on verified success (A4), and until it does there should be a
   documented `dfsctl` cleanup rather than operators guessing.
2. **Stale `gc-state`** (7.2 G, June entries with 769 M DISCARD + KEYREGISTRY): needs a reaper —
   confirm current behaviour in `pkg/block/engine/gcstate.go`, then file an issue. Badger value-log
   GC on that store is the likely miss.

Also correct the record from my earlier #1843 reply: the "disk mystery" was neither dead segments
(#1844) nor a lost index — it was the un-reaped migration archive plus gc-state, and the
`Blocks Local 0` was the #1850 bug, not cosmetics.

---

## Stream C — Proxmox report (Jonas)

### C1 — `dfsctl share nfs-config show` nil-pointer panic (fix now)

`cmd/dfsctl/commands/share/nfs_config.go:108` calls
`cmdutil.PrintResource(os.Stdout, cfg, nil)` unconditionally; in the default (table) format
`PrintResource` (`cmd/dfsctl/cmdutil/util.go:279`) hands the nil `TableRenderer` to
`output.PrintTable`, which calls `data.Headers()` on a nil interface → SIGSEGV.

Root-cause fix at the shared seam, since 5 call sites pass nil:

- `PrintResource`: nil renderer → fall back to the key/value dump instead of a nil deref. A `show`
  command must never be able to panic on its default output format.
- Then audit the 5 nil sites and give each a real table renderer where table output is reachable:
  `share/nfs_config.go:108` (broken today — unconditional), `netgroup/show.go:94`,
  `adapter/settings.go:356`, `adapter/settings.go:419`, `share/show.go:161` (guarded by a format
  check — fine, but make it obvious).
- Test: table-format `PrintResource` with a nil renderer returns without panicking.

### C2 — docs / help / log messages referencing flags and subcommands that don't exist

Mechanical audit: extract every `dfs`/`dfsctl` command+flag string appearing in `docs/`, in Cobra
`Long`/`Example` blocks, and in log/error messages, and diff against the actual command trees
(`cmd/gendocs` already walks them — reuse it as the source of truth). Anything that doesn't
resolve is a bug. Ask Jonas for the specific ones he hit so the audit is validated against real
examples, not just self-consistency.

### C3 — NFS interop with Proxmox VE

Known gaps, to confirm against a real PVE box:

- **NFS over UDP is not implemented** (`internal/adapter/nfs/doc.go:133`). The portmapper does
  serve TCP+UDP on 111 (`internal/adapter/nfs/portmap/server.go`), so `EnableUDP` covers rpcbind
  but not the NFS/MOUNT data path.
- PVE's storage layer probes with `showmount -e <host>` and mounts on the standard port; our
  default is `12049`. That combination, not a PVE misconfiguration, is the most likely reason it
  never worked.

Work: stand up dfs with port 2049 + portmapper enabled, verify `showmount -e` returns the export
and that `pvesm add nfs` succeeds; fix whatever the probe needs (MOUNT program registration /
default port advice), then add an explicit **Proxmox VE** section to `docs/guide/nfs.md` with the
exact working config. Decide separately whether NFS-over-UDP is worth implementing — PVE defaults
to TCP, so it is probably not the blocker.

---

## Issues to open

- reuse **#1850** for Stream A (cold-interval durability; note the snapshot-restore variant).
- new: `dfsctl` nil-renderer panic in table output (C1).
- new: command/flag references in docs, help text and log messages drift from the Cobra trees (C2).
- new: Proxmox VE / `showmount` NFS interop (C3).
- new: stale `gc-state` is never reaped (B2).
- new: migration must content-verify before declaring success and must reap its own archive (A4).

## Per-PR bar

simplifier → reviewer → **Copilot** → CI green → squash → close issue → `graphify update`.
