# `pkg/block/middleware`

A generic pipeline for transforming block bodies on their way to and from a
remote store.

The interface is deliberately not specific to any particular transform. A
`Transform` is any reversible per-chunk byte transform, and `Pipeline` is a
`remote.RemoteStore` that runs a fixed stack of them. Everything above the
pipeline — engine, cache, GC, metadata stores — sees only plaintext, so a
pipelined store is indistinguishable from a bare one.

## Current implementations

| Stage | Package | What it does |
|---|---|---|
| compression | [`compression/`](compression) | zstd/lz4 framing; adaptive, skips its frame when the body would not shrink |
| encryption | [`encryption/`](encryption) | AEAD seal with the plaintext content hash bound as additional authenticated data |

That is the whole list today. Nothing in this package knows either of them
exists — a third stage is one `Transform` implementation and one more argument
to `New`.

## The contract

```go
type Transform interface {
	Seal(ctx context.Context, hash block.ContentHash, data []byte) ([]byte, error)
	Open(ctx context.Context, hash block.ContentHash, data []byte) ([]byte, error)
}
```

`Seal` runs on the way out, `Open` inverts it on the way back in. Both take
`ctx` and `hash` because some stages need them — encryption binds `hash` as AEAD
AAD — and a stage needing neither ignores both, as compression does.

`New(inner, stages...)` takes stages in **seal order**; `Open` runs them in
reverse. A stage holding resources may implement `io.Closer`, and
`Pipeline.Close` closes every stage that does — that is how the encryption
stage's key provider is released.

Only `SealChunk` and `ReadChunk` are intercepted. The block-keyed operations
(`PutBlock`, `GetBlock`, `GetBlockRange`, `DeleteBlock`, `WalkBlocks`) forward
untransformed through the embedded `remote.Passthrough`: a packed block object
carries per-chunk bodies that `SealChunk` has already transformed, so
transforming the assembled block again would double-seal it.

## Each stage owns its unframed-body policy

**A stage must frame its own output, and must decide for itself what an
unrecognised body means. The pipeline does not decide this, and must not.**

The two current stages disagree, correctly:

- **compression** treats an unframed body as plaintext and passes it through,
  because it skips its own frame whenever the body would not shrink.
- **encryption** *rejects* an unframed body (`ErrCiphertextWithoutFrame`),
  because on an encryption-enabled share one means external mutation or a stale
  policy.

Hoisting a single "not my frame → pass it through" rule into the pipeline would
turn the second behaviour into the first and make encryption **fail open**. Any
new stage must state which of the two it is and why.

## Order matters and nothing can check it

The write path is caller → compression → encryption → inner store; reads invert
it. AEAD output has near-maximum entropy, so compressing ciphertext yields a
ratio of ~1.0 and burns CPU for nothing. Compressing plaintext first is the only
ordering that preserves any space saving.

A `[]Transform` makes the wrong order expressible in one line, and it fails
*silently*: both arrangements round-trip, neither errors, and no log line
distinguishes them. The only symptom is a compression ratio that never improves.
So the order is pinned by tests rather than by the type system:

- `TestPipeline_CompressBeforeEncrypt` shows the two orders differ, by size.
- `shares.TestRemoteStages_CompressionBeforeEncryption` pins which order
  actually ships.

Give the order its own type the moment a second site builds a stack;
`remoteStages` in `pkg/controlplane/runtime/shares/blockstore_config.go` is the
only one today.

## The CAS key stays BLAKE3 over plaintext

Every stage preserves that. Compression leaves the hash untouched; encryption
binds it as AAD, so a block swapped at the inner store fails authentication on
read because its declared hash will not match the AAD bound when it was sealed.

Neither compression framing nor encryption keys influence the hash, so dedup
works across remotes differing in algorithm, key or AEAD choice: identical
plaintexts always map to the same content hash. No single stage holds both the
wire bytes and the plaintext hash domain, so no stage verifies — the engine
hashes the recovered plaintext after the whole stack has run.
