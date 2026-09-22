# `pkg/block/middleware/encryption`

A `remote.RemoteStore` decorator that encrypts chunk bodies before they
leave the host and decrypts them on the way back, preserving the
BLAKE3-over-plaintext CAS key. It is installed once per remote by the
controlplane share service.

The decorator contract, the composition order against `compression` and
the CAS-key invariant live in [`../README.md`](../README.md).

## Envelope encryption

- A master key is held by a `keyprovider.KeyProvider` — a local key file
  or an external KMIP HSM. The master key never directly encrypts a
  chunk.
- Per chunk, a fresh 32-byte block key is generated and used with an AEAD
  to encrypt the body. The block key is wrapped under the master key and
  the wrapped bytes live in the frame header.
- On read: decode the header, unwrap the block key via the provider,
  authenticated-decrypt the body with the content hash as AAD.

### Per-chunk cryptographic material

- **Block key** — 32 bytes from `crypto/rand`, fresh per seal, wrapped
  under the share's master key by the configured `KeyProvider` and stored
  in the frame header.
- **Nonce** — AEAD-specific length from `crypto/rand`, fresh per seal,
  stored alongside the ciphertext: 12 bytes for AES-256-GCM and
  ChaCha20-Poly1305, 24 bytes for XChaCha20-Poly1305. XChaCha's larger
  nonce is safe to draw at random at very high chunk counts; the 12-byte
  variants assume a per-share chunk budget well inside the birthday bound
  for random nonces.

### Master-key rotation

A provider holds one current master key plus a set of retired,
decrypt-only ones. Frames record the id of the key that wrapped them, so
retiring a key keeps existing blocks readable while new writes move onto
the new key. There is no re-wrap tooling, so a retired key must stay
configured for as long as any block references it — dropping it makes
those blocks permanently unreadable. See
[`keyprovider`](keyprovider/) for the full behaviour.

## Wire format

Per chunk:

```
offset 0..4   magic              5 bytes "DFENC"
offset 5      version            1 byte 0x01
offset 6      aead algorithm     1 byte  1: AES-256-GCM, 2: ChaCha20-Poly1305, 3: XChaCha20-Poly1305
offset 7      wrap kind          1 byte 0x01 (keyprovider managed)
offset 8..    master-key-id      uvarint length + bytes
offset ..     wrapped block key  uvarint length + bytes
offset ..     nonce              1-byte length + bytes
offset ..     ciphertext + tag   rest of the body
```

A block stores each chunk's full self-framed blob verbatim, so decrypting
a chunk's `[offset, length)` slice out of a packed block is identical to
decrypting it standalone.

An unframed body on an encryption-enabled remote is rejected rather than
passed through: it means external mutation or a stale policy.

## Configuration

Opt-in per remote, via the `BlockStoreConfig.Config` JSON:

```json
{
  "encryption": {
    "aead": "aes-256-gcm",
    "key": {
      "kind": "local",
      "file": "/etc/dittofs/keys/share.key"
    }
  }
}
```

The passphrase that unlocks a local key file is read from the
`DITTOFS_ENCRYPTION_PASSPHRASE` environment variable.

Absence of the `encryption` key means no wrapping, and no behaviour
change. The AEAD defaults to AES-256-GCM when the key is present without
an explicit `aead`.
