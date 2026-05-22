// Package blockstore defines the unified content-addressed block storage
// contract DittoFS uses across every storage tier. It is the
// single source of truth for FileBlock, BlockState, ContentHash, BlockSize,
// the BlockStore + BlockStoreAppend interfaces, the minimal Meta struct,
// the error sentinels (ErrStopWalk, ErrLegacyLayoutDetected,
// ErrChunkNotFound, …), and the on-disk irreversible-state-transition
// conventions (sentinel marker files).
//
// # Interface roles
//
// Phase 17 collapsed the v0.15 split (LocalStore: 22 methods, RemoteStore:
// 12 methods) onto two interfaces both keyed by ContentHash:
//
//   - BlockStore — the unified surface for content-addressed CRUD
//     (Put / Get / GetRange / Has / Delete / Head / Walk). Idempotent
//     same-bytes Put, no opaque "block key" strings, every method takes
//     a context.Context first. Implemented by:
//
//     *pkg/blockstore/local/fs.FSStore          (local CAS chunks)
//     *pkg/blockstore/remote/s3.Store           (S3-backed CAS)
//     *pkg/blockstore/remote/memory.Store       (in-memory CAS for tests)
//
//   - BlockStoreAppend — embeds BlockStore and adds AppendWrite +
//     DeleteLog for the random-write absorber tier (the per-file
//     append log + FastCDC rollup loop on the fs backend). s3 and
//     memory backends do NOT implement this — they only see rolled-up
//     Put calls.
//
// Meta (the value returned by Head and surfaced via Walk) carries
// minimal fields per Phase 17 D-08:
//
//	type Meta struct {
//	    Size         int64
//	    LastModified time.Time
//	}
//
// The lookup key (ContentHash) is NEVER echoed inside Meta — it is the
// input, not output. S3's x-amz-meta-content-hash header is preserved
// inside the s3 backend internals for defense-in-depth verification on
// reads (BSCAS-06), but is not exposed through Meta.
//
// Backends MUST stamp a non-zero Meta.LastModified for every object;
// the mark-sweep GC fails closed on a zero timestamp (mirrors Phase 11
// WR-4-02 / INV-04).
//
// # Walk semantics
//
// BlockStore.Walk enumerates every object in unspecified order. The
// callback returns errors to drive control flow (Phase 17 D-07,
// mirroring filepath.SkipDir and fs.SkipAll):
//
//   - return blockstore.ErrStopWalk → Walk exits cleanly (returns nil
//     to the outer caller). Idiomatic use case: GC has found its
//     target and wants to short-circuit the remaining enumeration.
//
//   - return any other non-nil error → Walk halts and returns it
//     wrapped:
//
//     fmt.Errorf("walk halted at %s: %w", hash, err)
//
//   - ctx cancellation → Walk aborts immediately. The callback is NOT
//     re-invoked after ctx.Err() != nil; Walk surfaces ctx.Err()
//     without one final spurious callback.
//
// See BlockStore.Walk godoc for the full contract.
//
// # Sentinel-file convention (.cas-migrated-vN)
//
// Phase 17 establishes a project-wide pattern for irreversible
// on-disk state transitions: a dot-prefixed sentinel marker file
// named .cas-migrated-vN — where N is the layout-schema version —
// that proves a state transition completed atomically.
//
// Lifecycle:
//
//   - Written by migration tooling (e.g., `dfs migrate-to-cas`) via
//     atomic rename ONLY at successful completion of the transition.
//     A crash mid-migration cannot leave the sentinel behind because
//     the tooling writes <name>.tmp first, fsyncs, then renames into
//     place and fsyncs the parent directory.
//
//   - Read by backend constructors at open time. *fs.FSStore (via
//     NewFSStore → newFSStoreInternal) stats <baseDir>/.cas-migrated-v1
//     before any other I/O. Presence is the ground-truth proof of
//     completion. Cost is O(1).
//
//   - On absence, the constructor runs a depth-capped probe for
//     legacy `.blk` files in baseDir; if any are found and no
//     sentinel exists, it returns ErrLegacyLayoutDetected. The boot
//     guard in cmd/dfs/commands/start.go unwraps via errors.Is,
//     prints an operator directive, and exits 78 (EX_CONFIG per
//     sysexits(3)). Per-share fail-fast: the first un-migrated
//     share halts boot.
//
// Per-share placement: the sentinel lives at the share root that
// production passes to fs.NewFSStore as baseDir. Per-share semantics
// (not per-storage-dir global) mean `--share <name>` migrations
// produce per-share sentinels and partial multi-share runs leave
// already-migrated shares boot-able while unmigrated ones remain
// refused.
//
// Sentinel contents (JSON):
//
//	{
//	    "Version":     "v1",
//	    "CompletedAt": "2026-05-20T14:30:00Z",
//	    "ToolVersion": "v1.0.0",
//	    "ShareDir":    "/path/to/share"
//	}
//
// Hand-editing the sentinel is a footgun — it bypasses the boot guard
// without curing the underlying layout mismatch, leaving a partially
// migrated store that will surface I/O errors on the first legacy
// FileBlock access. Treat the file as a one-way irreversibility
// marker. The recovery procedure for a failed migration lives in
// docs/CONFIGURATION.md §Migration.
//
// Future schema bumps: increment N. New constructors stat the highest
// version they recognize and refuse anything below. A v2 backend
// reading a v1 sentinel must surface an explicit
// "downgrade not supported" error rather than silently accepting it.
//
// # Migration tooling
//
// The offline one-shot legacy-to-CAS migration ships as the cobra
// subcommand `dfs migrate-to-cas`, backed by the shared library at
// pkg/blockstore/migrate/migrate_to_cas.go (MigrateShareToCAS). The
// command is server-side and offline (refuses to run while the dfs
// PID lockfile exists), idempotent via a per-share journal
// (.dittofs-migrate-to-cas.state), and writes the .cas-migrated-v1
// sentinel via atomic rename only on full per-share success.
//
// Operators who arrive here via `go doc` should next read
// docs/CONFIGURATION.md §Migration for the boot-guard contract,
// exit code 78, recovery procedure, and crash-safety guarantees, and
// docs/CLI.md for the full `dfs migrate-to-cas` flag reference.
//
// # Error sentinels
//
// The package exports these sentinels for callers to match via
// errors.Is. See errors.go for full doc paragraphs and protocol-error
// mappings.
//
//   - ErrStopWalk             — Walk callback early-exit signal.
//   - ErrLegacyLayoutDetected — backend constructor refused an
//     un-migrated `.blk` layout; operator must run
//     `dfs migrate-to-cas`.
//   - ErrChunkNotFound        — content-addressed chunk is absent
//     from the store.
//   - ErrBlockNotFound        — remote-side block-miss error.
//   - ErrCASContentMismatch   — recomputed BLAKE3 disagreed with the
//     expected ContentHash on read (INV-06 fail-closed).
//   - ErrCASKeyMalformed      — ParseCASKey rejected an input that
//     did not match the cas/{hh}/{hh}/{hex} shape.
//   - ErrBlockRefMissing      — BlockRef.Hash referred to an absent
//     FileBlock (mapped to NFS3ERR_IO / STATUS_DATA_ERROR by the
//     adapter errmap).
//
// # Sub-packages
//
//   - local: LocalStore admin-superset interface + the *fs.FSStore
//     implementation (the only BlockStoreAppend).
//   - remote: Remote backend implementations (s3, memory) that
//     implement BlockStore only.
//   - blockstoretest: Unified conformance suite. Two entrypoints —
//     BlockStoreConformance(t, factory) and
//     BlockStoreAppendConformance(t, factory) — let backends opt
//     into the contract surface they claim.
//   - engine: BlockStore engine composing local store + syncer +
//     unified Cache + metadata (Phase 12 / A3 unified Cache).
//   - chunker: FastCDC chunker used by both writes and by the
//     migration tool.
//   - migrate: Migration library and shared utilities (journal,
//     walk helpers, MigrateShareToCAS).
//   - gc: Mark-sweep garbage collection (Phase 11 / A2 fail-closed
//     against the union of live ContentHashes).
//   - storetest: Legacy conformance test suites for higher-level
//     FileBlockStore implementations.
//
// # Transitional-marker convention
//
// Code that must compile today but is slated for deletion at a known
// future milestone carries a plain-text grep marker on its godoc:
//
//	TRANSITIONAL-PHASE-N:         scheduled deletion in milestone N
//	                              (substitute N with the concrete
//	                              milestone number owning the cleanup
//	                              wave)
//	TRANSITIONAL-NEXT-MILESTONE:  deletion scheduled for the next
//	                              major milestone planning sweep
//	                              (generic; use when no specific
//	                              milestone number applies yet)
//
// Markers are plain text — not godoc "Deprecated:" tags — so
// staticcheck SA1019 does not fire on existing call sites that the
// cleanup wave will rewrite. The next milestone's planning pass greps
// for both markers and either retires them (deletion) or re-targets
// them to a specific milestone tag.
//
// Apply the markers on the symbol's godoc, not on internal callers;
// the goal is for `grep -rn 'TRANSITIONAL-' ./pkg/blockstore` to
// enumerate every deferral surface in one pass and for new contributors
// to recognize the convention without consulting a roadmap.
//
// Phase 19 D-25 audit: at the close of the write-path RAM-optimization
// phase, every TRANSITIONAL-NEXT-MILESTONE marker in pkg/blockstore/
// points at #519 ("Deferred to v0.17+") — the five v0.17 anchor sites
// are pinned hot-tail RAM + zstd compression (chunkstore.go), O_DIRECT
// (appendwrite.go), tmpfs spill (appendlog.go), and cold-cache prefetch
// (engine/cache.go). All five carry the generic NEXT-MILESTONE marker
// (the inline `see #519` reference documents the v0.17+ target without
// burning a TRANSITIONAL-V0.17 tag into the grep namespace until the
// v0.17 planning pass commits to a concrete deletion plan).
//
// Phase 19 D-23 also closed the Phase 18 D-16 `claim_batch_size`
// deprecation cycle (the SyncerConfig field was set/defaulted but
// never read by the syncer claim path). That cycle did not use a
// TRANSITIONAL- marker — it relied on an inline godoc note — and the
// field plus its defaults/validate paths are gone as of this phase.
package blockstore
