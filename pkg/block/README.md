# `pkg/block`

The shared vocabulary of the block layer: `ContentHash`, `BlockState`,
`BlockSize`, `FileChunk`, `ChunkRef`, `ChunkLocator`, `BlockRecord`, `Meta`, the
error sentinels and the on-disk format-version convention.

The package is a **leaf** — it imports only the standard library and
`lukechampine.com/blake3`, never another package of this repository.
`TestNoForeignImports` asserts that, so the claim cannot rot.

## `Meta`

`Meta` is the minimal per-object metadata a remote store reports from
`HeadBlock` / `WalkBlocks`:

```go
type Meta struct {
	Size         int64
	LastModified time.Time
}
```

The lookup key is never echoed inside `Meta` — it is the input, not the output.
S3 stamps `x-amz-meta-content-hash` on every `PutObject` for defense in depth,
but that header stays inside the s3 backend.

Backends MUST stamp a non-zero `LastModified` for every object: the mark-sweep
GC fails closed on a zero timestamp.

## Walk semantics

`remote.RemoteBlockStore.WalkBlocks` enumerates every object in unspecified
order. The callback returns errors to drive control flow, mirroring
`filepath.SkipDir` and `fs.SkipAll`:

- `block.ErrStopWalk` → the walk exits cleanly and returns nil to the outer
  caller. The idiomatic use is a GC pass that found its target and wants to
  short-circuit the rest of the enumeration. Wrapping is fine; implementations
  match with `errors.Is`.
- any other non-nil error → the walk halts and returns it wrapped.
- context cancellation → the walk aborts immediately, surfacing `ctx.Err()`
  without one final spurious callback.

## On-disk format versions

Every store stamps the format version of the state it writes and checks that
stamp when it opens. A stamp NEWER than the running build is refused with
`ErrFutureFormat`: the alternative is not an error but silence, because a
record whose layout moved to a sibling key decodes cleanly into a file with the
right size and no content, and the store then serves zeros. Boot treats the
refusal as fatal for the whole daemon rather than skipping the share — matching
both `block.ErrFutureFormat` and `journal.ErrFutureFormat` — so a downgraded box
cannot come up looking healthy.

The reverse direction, state OLDER than the build, is a migration rather than a
refusal and runs automatically at share startup. It is one-way: once it has run,
the previous release can no longer read the result, and the migration warns
about that before it starts.

Two conversions into the current remote layout shipped and have since been
removed: the offline `.blk`-to-CAS tool (`dfs migrate-to-cas`, through v0.21) and
the automatic cas→blocks conversion that folded standalone CAS objects into
packed `blocks/<id>` containers. A share still on either older layout must be
staged through a release that carries them, or re-ingested; this build refuses
the reads rather than guessing.

## Error sentinels

`errors.go` carries the full doc paragraph and protocol mapping for each one.
The ones with a contract beyond "this went wrong":

- `ErrStopWalk` — the walk callback early-exit signal (above).
- `ErrFutureFormat` — a store refused on-disk state written by a newer release
  than this build can read. The journal side-log names live on
  `journal.ErrFutureFormat`; boot matches both.
- `ErrChunkNotFound` — a content-addressed chunk is absent, local or remote.
- `ErrChunkContentMismatch` — the recomputed BLAKE3 disagreed with the expected
  `ContentHash` on read. Fail-closed: the bytes are discarded.
- `ErrManifestInconsistent` — a manifest row that cannot be placed in the file.
  Deliberately not treated as a hole, which would serve zeros for a range the
  store still holds bytes for.
- `ErrUnknownHash` — `FileChunkStore.AddRef` found no row for the hash; the
  read-through hit path must fall back to the full `Put`.

## Sub-packages

- `remote` — the block-keyed `RemoteBlockStore` contract, its backends (`s3`,
  `memory`) and `Passthrough`, the forwarding base the decorators embed.
- `local` — the payload-keyed `LocalStore` interface, plus `local/memory`, the
  in-memory test double.
- `journal` — the append-log write-back local store: records, shards, carve,
  eviction and GC.
- `engine` — the composition root: local store + syncer + cache + metadata.
- `carver`, `chunker` — the FastCDC carve pass over dirty ranges.
- `middleware` — the two decorators wrapping a remote store, `middleware/compression`
  and `middleware/encryption`, and the composition order they must be stacked in.
- `blockcodec` — the packed-block wire framing.
- `syncer` — the adaptive upload window.
- `blockstoretest` — `RemoteBlockStoreConformance`, the one conformance suite,
  pinning the block-keyed remote contract. There is no hash-keyed suite.
