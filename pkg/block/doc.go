// Package blockstore defines the unified content-addressed block storage
// contract DittoFS uses across every storage tier. It is the
// single source of truth for FileChunk, BlockState, ContentHash, BlockSize
// the BlockStore + BlockStoreAppend interfaces, the minimal Meta struct
// the error sentinels (ErrStopWalk, ErrFutureFormat
// ErrChunkNotFound, …), and the on-disk irreversible-state-transition
// conventions (sentinel marker files).
//
// # Interface roles
//
// Two interfaces, both keyed by ContentHash, replace the earlier
// v0.15 split (LocalStore: 22 methods, RemoteStore: 12 methods):
//
//   - BlockStore — the unified surface for content-addressed CRUD
//     (Put / Get / GetRange / Has / Delete / Head / Walk). Idempotent
//     same-bytes Put, no opaque "block key" strings, every method
//     takes a context.Context first. Implemented by:
//     *pkg/block/local/fs.FSStore (local CAS chunks);
//     *pkg/block/remote/s3.Store (S3-backed CAS);
//     *pkg/block/remote/memory.Store (in-memory CAS for tests).
//
//   - BlockStoreAppend — embeds BlockStore and adds AppendWrite +
//     DeleteAppendLog for the random-write absorber tier (the per-file
//     append log + FastCDC rollup loop on the fs backend). s3 and
//     memory backends do NOT implement this — they only see rolled-up
//     Put calls.
//
// Meta (the value returned by Head and surfaced via Walk) carries
// minimal fields:
//
//	type Meta struct {
//	    Size         int64
//	    LastModified time.Time
//	}
//
// The lookup key (ContentHash) is NEVER echoed inside Meta — it is
// the input, not output. S3's x-amz-meta-content-hash header is
// preserved inside the s3 backend internals for defense-in-depth
// verification on reads, but is not exposed through Meta.
//
// Backends MUST stamp a non-zero Meta.LastModified for every object;
// the mark-sweep GC fails closed on a zero timestamp.
//
// # Walk semantics
//
// BlockStore.Walk enumerates every object in unspecified order. The
// callback returns errors to drive control flow (mirroring
// filepath.SkipDir and fs.SkipAll):
//
//   - return block.ErrStopWalk → Walk exits cleanly (returns nil
//     to the outer caller). Idiomatic use case: GC has found its
//     target and wants to short-circuit the remaining enumeration.
//
//   - return any other non-nil error → Walk halts and returns it
//     wrapped: fmt.Errorf("walk halted at %s: %w", hash, err).
//
//   - ctx cancellation → Walk aborts immediately. The callback is NOT
//     re-invoked after ctx.Err() != nil; Walk surfaces ctx.Err()
//     without one final spurious callback.
//
// See BlockStore.Walk godoc for the full contract.
//
// # On-disk format versions
//
// Every store stamps the format version of the state it writes and checks
// that stamp when it opens. A stamp NEWER than the running build is refused
// with ErrFutureFormat: the alternative is not an error but silence, because
// a record whose layout moved to a sibling key decodes cleanly into a file
// with the right size and no content, and the store then serves zeros. Boot
// treats the refusal as fatal for the whole daemon rather than skipping the
// share, so a downgraded box cannot come up looking healthy.
//
// The reverse direction — state OLDER than the build — is a migration, not a
// refusal, and runs automatically at share startup. It is one-way: once it
// has run, the previous release can no longer read the result. The migration
// warns about that before it starts.
//
// The offline .blk-to-CAS migration tool (`dfs migrate-to-cas`) shipped
// through dittofs v0.21 and has been removed; shares still on the `.blk`
// layout must be migrated with an older release before upgrading. The
// follow-on cas->blocks conversion (standalone CAS objects into packed
// blocks/<id> containers) is automatic: it runs in the background from
// engine.Store.Start, is resumable and idempotent, and needs no tooling.
//
// # Error sentinels
//
// The package exports these sentinels for callers to match via
// errors.Is. See errors.go for full doc paragraphs and protocol-error
// mappings.
//
// - ErrStopWalk — Walk callback early-exit signal.
//   - ErrFutureFormat — a store refused on-disk state written by a
//     newer release than this build can read.
//   - ErrChunkNotFound — content-addressed chunk is absent
//     from the store (local or remote).
//   - ErrChunkContentMismatch — recomputed BLAKE3 disagreed with the
//     expected ContentHash on read (fail-closed).
//   - ErrCASKeyMalformed — ParseCASKey (migration-only, legacy_cas.go)
//     rejected an input that did not match the legacy key shape.
//   - ErrChunkRefMissing — ChunkRef.Hash referred to an absent
//     FileChunk (mapped to NFS3ERR_IO / STATUS_DATA_ERROR by the
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
//     unified Cache + metadata.
//   - chunker: FastCDC chunker used by both writes and by the
//     migration tool.
//   - migrate: Migration library and shared utilities (journal
//     walk helpers, MigrateShareToCAS).
//   - gc: Mark-sweep garbage collection, fail-closed against the
//     union of live ContentHashes.
//   - storetest: Legacy conformance test suites for higher-level
//     FileChunkStore implementations.
//
// # Transitional-marker convention
//
// Code that must compile today but is slated for deletion at a known
// future milestone carries a plain-text grep marker on its godoc:
//
//	TRANSITIONAL-PHASE-N: scheduled deletion in milestone N
//	                              (substitute N with the concrete
//	                              milestone number owning the cleanup
//	                              wave)
//	TRANSITIONAL-NEXT-MILESTONE: deletion scheduled for the next
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
// Apply the markers on the symbol's godoc, not on internal callers
// the goal is for `grep -rn 'TRANSITIONAL-' ./pkg/block` to
// enumerate every deferral surface in one pass and for new contributors
// to recognize the convention without consulting a roadmap.
//
// audit: at the close of the write-path RAM-optimization
// phase, every TRANSITIONAL-NEXT-MILESTONE marker in pkg/block/
// points at #519 ("Deferred to v0.17+") — the five v0.17 anchor sites
// are pinned hot-tail RAM + zstd compression (chunkstore.go), O_DIRECT
// (appendwrite.go), tmpfs spill (appendlog.go), and cold-cache prefetch
// (engine/cache.go). All five carry the generic NEXT-MILESTONE marker
// (the inline `see #519` reference documents the v0.17+ target without
// burning a TRANSITIONAL-V0.17 tag into the grep namespace until the
// v0.17 planning pass commits to a concrete deletion plan).
//
// also closed the `claim_batch_size`
// deprecation cycle (the SyncerConfig field was set/defaulted but
// never read by the syncer claim path). That cycle did not use a
// TRANSITIONAL- marker — it relied on an inline godoc note — and the
// field plus its defaults/validate paths are gone as of this phase.
package block
