# `pkg/block/middleware`

The two decorators that transform block bodies on their way to and from a
remote store: `compression` and `encryption`. Both wrap a
`remote.RemoteStore` and are themselves a `remote.RemoteStore`, so a
decorated store is indistinguishable from a bare one to everything above
it — engine, cache, GC and the metadata stores see only plaintext.

This directory holds no Go code of its own. It groups the two packages
and is the single home for the contract they share, which used to be
stated twice, once in each package doc.

## What a decorator is

A decorator embeds `remote.Passthrough` and overrides exactly the two
per-chunk operations its transform touches:

- `SealChunk(ctx, hash, plaintext)` — apply this layer's transform, then
  hand the result to the inner store's `SealChunk`.
- `ReadChunk(ctx, blockID, offset, length, hash)` — read the inner
  store's bytes, then invert this layer's transform.

Everything else forwards verbatim. The block-keyed operations
(`PutBlock`, `GetBlock`, `GetBlockRange`, `DeleteBlock`, `WalkBlocks`)
MUST forward untransformed: a packed block object carries per-chunk
bodies that `SealChunk` already transformed, so transforming the
assembled block again would double-seal it. `Close`, `HealthCheck`,
`Healthcheck` and `Durable` forward because a transform changes the shape
of the bytes, not where they land or whether the backend is reachable.
`remote.Passthrough` is where all of that lives, and its doc comment is
the authority on it.

A decorator keeps its own `inner remote.RemoteStore` field alongside the
embedded `Passthrough`, because `Passthrough` deliberately keeps its copy
unexported: embedding must not promote a handle to the untransformed
store onto a decorator's public surface, where a caller could read and
write bytes straight past the transform.

## There is no `Layer` interface, and no `Compose`

The shape the two decorators share is already an interface pair —
`remote.ChunkSealer` and `remote.ChunkReader` — and the shared forwarding
is already a type, `remote.Passthrough`. What is left in each decorator
after those is the transform itself: zstd/lz4 framing on one side, an
AEAD seal with the content hash as additional authenticated data on the
other. Those share a signature, not any logic.

A `Layer` interface over the two transforms would be a second abstraction
across the same seam, and it would cost more than it saves: compression
needs neither the context nor the content hash, so it would have to
accept and ignore both, and encryption's `Close` — which must also close
the key provider — would have to be recovered through an optional
`io.Closer` assertion. The generic decorator, the interface and its
documentation together come to more lines than the per-decorator glue
they would replace.

Add one when a third transform appears, or when a transform needs to be
selected at runtime rather than fixed at store construction. Neither is
true today: the stack is two layers deep and built once per remote.

## Composition order

Compression is the OUTERMOST wrapper and encryption the INNERMOST, so the
write path is:

```
caller plaintext
  → compression.Decorator     (compress)
  → encryption.EncryptedRemote (encrypt the compressed bytes)
  → inner remote.RemoteStore
```

Reads invert it: fetch, decrypt, then decompress.

AEAD output has near-maximum entropy, so compressing ciphertext yields a
ratio of ~1.0 and burns CPU for nothing. Compressing plaintext first is
the only ordering that preserves any space saving.

Encryption operates on whatever the compression layer hands it — the
compressed body when the chunk was compressible, the raw plaintext
otherwise, since compression skips its frame when the body would not
shrink. The encryption layer neither knows nor cares which shape it sees.

The order is established once, in
`pkg/controlplane/runtime/shares/blockstore_config.go`, and is immutable
for the lifetime of the remote. There is no runtime toggle.

## The CAS key is BLAKE3 over the plaintext

Both decorators preserve that. Compression leaves the hash untouched;
encryption binds it into the AEAD's additional authenticated data, so a
swapped block at the inner store fails authentication on read because its
declared hash will not match the AAD bound when it was sealed.

Neither the compression framing nor the encryption keys influence the
hash, so dedup works across remotes that differ in compression algorithm,
encryption key or AEAD choice: identical plaintexts always map to the
same content hash. No single layer holds both the wire bytes and the
plaintext hash domain, so no layer verifies; the engine read path hashes
the recovered plaintext after the full stack has unsealed it.
