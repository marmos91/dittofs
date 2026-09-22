# `pkg/block/middleware/compression`

A `remote.RemoteStore` decorator that compresses chunk bodies before they
leave the host and decompresses them on the way back, preserving the
BLAKE3-over-plaintext CAS key. It is installed once per remote by the
controlplane share service. Engine, cache, GC and the metadata stores see
only plaintext.

The decorator contract, the composition order against `encryption` and
the CAS-key invariant live in [`../README.md`](../README.md).

## Wire format

Per chunk, when compression actually shrinks the body:

```
offset 0..4   magic       5 bytes "DFCMP"
offset 5      algo        1 byte   1 = zstd, 2 = lz4
offset 6..    orig_size   uvarint (1-10 bytes)
offset N..    body        compressed bytes
```

Compression is per-chunk adaptive: when the framed form is not strictly
smaller than the plaintext, the decorator stores the raw plaintext with
no header at all. The read path tells framed from raw by checking the
5-byte magic prefix.

A block stores each chunk's full self-framed blob (or its raw
passthrough) verbatim, so decoding a chunk's `[offset, length)` slice out
of a packed block is identical to decoding it standalone.

## Configuration

Opt-in per remote, via the `BlockStoreConfig.Config` JSON:

```json
{ "compression": { "algo": "zstd" } }
```

Absence of the `compression` key means no wrapping, and no behaviour
change. The algorithm defaults to zstd when the key is present without an
explicit `algo`.
