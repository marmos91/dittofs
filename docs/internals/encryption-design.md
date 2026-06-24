# Encryption Design Internals

> This document is for contributors. For user/operator guidance on enabling encryption, configuration, and operational warnings, see [../guide/encryption.md](../guide/encryption.md).

## Envelope encryption design

Standard envelope encryption, matching AWS SSE-KMS, MinIO + KES, and HashiCorp Vault Transit:

1. A **master key** is held by a key provider (local file or KMIP-speaking HSM). The master key never directly encrypts a block.
2. For each block, a fresh 32-byte **block key** is generated from `crypto/rand` and used with an AEAD to encrypt the payload.
3. The block key is **wrapped** under the master key. The wrapped bytes live in the block frame header, alongside the master-key identifier.
4. On read: parse the frame → unwrap the block key via the provider → AEAD-decrypt the payload.

The plaintext BLAKE3 hash binds the ciphertext to its CAS address — a swapped block fails authentication.

## Decorator pattern

The encryption layer is implemented as a decorator that wraps the remote block store. It composes with the existing compression decorator:

```
PUT  plaintext  →  compression  →  encryption  →  S3
GET  S3         →  decryption    →  decompression → plaintext
```

Compression must run **before** encryption. Encrypted bytes are statistically indistinguishable from random data, which a compressor cannot shrink.

## Wire frame

Every encrypted block is prefixed with a frame header:

```
offset 0..4   magic              5 bytes  "DFENC"
offset 5      version            1 byte   0x01
offset 6      aead algorithm     1 byte   1: AES-256-GCM, 2: ChaCha20-Poly1305, 3: XChaCha20-Poly1305
offset 7      wrap kind          1 byte   0x01 (keyprovider managed)
offset 8..    master-key-id      uvarint length + bytes
offset ..     wrapped block key  uvarint length + bytes
offset ..     nonce              1-byte length + bytes (12 for GCM/Poly1305, 24 for XChaCha20)
offset ..     ciphertext + tag   rest of the body
```

The magic bytes and version allow the decorator to detect and reject plaintext blocks on an encryption-enabled store (`ErrCiphertextWithoutFrame`), which prevents a tampered-storage actor from forcing a plaintext downgrade.

## Key hierarchy

```
master key  (held by key provider; local file or KMIP HSM)
    └── wraps ──► block key  (fresh 32-byte random per block; stored in frame header)
                     └── AEAD-encrypts ──► block payload
```

The master key identifier stored in each frame allows future multi-key `Unwrap` support (needed for key rotation) without changing the frame format.

## KMIP provider behavior and HSM integration

The KMIP provider fetches the configured master key from the HSM at startup using the standard `Get` operation and caches the bytes in process memory for the daemon's lifetime. All wrap / unwrap happens locally with AES-256-GCM.

This is a real KMIP integration (mutual-TLS, KMIP 1.4 protocol via `github.com/gemalto/kmip-go`) but it is **not** HSM-resident envelope encryption — the master-key bytes do live in the daemon's address space while it runs. A future iteration can move wrap / unwrap into the HSM via KMIP `Encrypt` / `Decrypt` operations without changing the `KeyProvider` interface; the public surface stays the same.

To rotate: write a new key to the HSM, update the `key_uid` in the remote config, and restart the share. Existing blocks remain decryptable because every frame carries the master-key identifier that wrapped its block key.

### Validating against a KMIP server

```bash
docker run --rm -d --name pykmip -p 5696:5696 pykmip/pykmip:latest

DITTOFS_TEST_KMIP=1 \
  DITTOFS_TEST_KMIP_ENDPOINT=localhost:5696 \
  DITTOFS_TEST_KMIP_CERT=test/fixtures/kmip/client.pem \
  DITTOFS_TEST_KMIP_KEY=test/fixtures/kmip/client.key \
  DITTOFS_TEST_KMIP_CA=test/fixtures/kmip/ca.pem \
  DITTOFS_TEST_KMIP_KEY_UID=<your-key-uid> \
  go test ./pkg/block/encryption/keyprovider/...
```

## Prior art

The design is intentionally derivative — envelope encryption is the well-trodden path for client-side encryption of object storage:

- **AWS S3 SSE-KMS** — per-object random data key wrapped by a customer master key.
- **MinIO + KES** — Go-stack precedent for the KMIP-backed envelope model.
- **HashiCorp Vault Transit** — wrap / unwrap API with master keys held server-side.
- **age (filippo.io/age)** — informed the AEAD choice (ChaCha20-Poly1305) but its stream-only shape was wrong for per-block random access.
