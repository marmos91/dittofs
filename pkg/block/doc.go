// Package blockstore defines the unified content-addressed block storage
// contract DittoFS uses across every storage tier. It is the
// single source of truth for FileChunk, BlockState, ContentHash, BlockSize
// the Store interface, the minimal Meta struct, the error sentinels
// (ErrStopWalk, ErrFutureFormat
// ErrChunkNotFound, …), and the on-disk format-version convention.
//
// # Interface roles
//
// One hash-keyed interface replaces the earlier v0.15 split
// (LocalStore: 22 methods, RemoteStore: 12 methods):
//
//   - Store — the unified surface for content-addressed CRUD
//     (Put / Get / GetRange / Has / Delete / Head / Walk). Idempotent
//     same-bytes Put, no opaque "block key" strings, every method
//     takes a context.Context first. Implemented by:
//     *pkg/block/remote/s3.Store and *pkg/block/remote/memory.Store
//     (behind the hash-keyed CAS path), and by the
//     compression / encryption decorators.
//
// The local random-write absorber tier (per-file append log + FastCDC
// rollup on the fs backend) is payload-keyed, not hash-keyed: it lives
// on pkg/block/local.LocalStore and is not part of this contract.
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
// Two conversions into the current remote layout shipped and have since been
// removed: the offline .blk-to-CAS tool (`dfs migrate-to-cas`, through v0.21)
// and the automatic cas->blocks conversion that folded standalone CAS objects
// into packed blocks/<id> containers. A share still on either older layout must
// be staged through a release that carries them, or re-ingested; this build
// refuses the reads rather than guessing.
//
// # Error sentinels
//
// The package exports these sentinels for callers to match via
// errors.Is. See errors.go for full doc paragraphs and protocol-error
// mappings.
//
//   - ErrStopWalk — Walk callback early-exit signal.
//   - ErrFutureFormat — a store refused on-disk state written by a
//     newer release than this build can read.
//   - ErrChunkNotFound — content-addressed chunk is absent
//     from the store (local or remote).
//   - ErrChunkContentMismatch — recomputed BLAKE3 disagreed with the
//     expected ContentHash on read (fail-closed).
//   - ErrCASKeyMalformed — ParseCASKey (cas_key.go) rejected an input
//     that did not match the "cas/" key shape.
//   - ErrChunkRefMissing — ChunkRef.Hash referred to an absent
//     FileChunk (mapped to NFS3ERR_IO / STATUS_DATA_ERROR by the
//     adapter errmap).
//
// # Sub-packages
//
//   - local: the payload-keyed LocalStore interface + the *fs.FSStore
//     implementation.
//   - remote: the block-keyed RemoteStore contract, its backend
//     implementations (s3, memory), and Passthrough, the forwarding
//     base the compression / encryption decorators embed.
//   - blockstoretest: conformance suites — BlockStoreConformance for
//     the hash-keyed Store surface and RemoteBlockStoreConformance for
//     the block-keyed one — let backends opt into the contract surface
//     they claim.
//   - engine: BlockStore engine composing local store + syncer +
//     unified Cache + metadata.
//   - journal: the append-log write-back store behind local/fs —
//     records, shards, carve, eviction and GC.
//   - chunker: the FastCDC chunker the carve pass runs over dirty
//     ranges.
//   - blockcodec: the packed-block wire framing.
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
// Apply the markers on the symbol's godoc, not on internal callers:
// the goal is for `grep -rn 'TRANSITIONAL-' ./pkg/block` to
// enumerate every deferral surface in one pass and for new contributors
// to recognize the convention without consulting a roadmap.
package block
