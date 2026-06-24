# Client-side block encryption

> For the envelope-encryption design, decorator pattern, wire frame layout, KMIP/HSM integration details, and key hierarchy, see [../internals/encryption-design.md](../internals/encryption-design.md).

DittoFS can encrypt every block before it leaves the server using a per-remote, decorator-based encryption layer. Encryption is opt-in per remote block store.

## What encryption protects (and what it does not)

Encryption protects block payloads against:

- Operators of the remote block store (S3 provider, MinIO admins).
- Anyone with read access to the bucket / prefix where blocks are stored.
- Theft of the underlying storage media.

Encryption does **not** protect:

- **Metadata.** Filenames, directory structure, file sizes, and timestamps are stored in the metadata backend in plaintext.
- **In-memory state.** Plaintext blocks live in the cache (RAM and disk tier) while a share is mounted. For full at-rest protection of the cache, place each share's local storage directory on an encrypted filesystem (FileVault / LUKS / dm-crypt).
- **Compromised dfs daemons.** The master key bytes live in process memory for the daemon's lifetime; anyone with `ptrace` against the daemon can recover them.

## Enabling encryption

Encryption is enabled per remote block store by setting an `encryption` block in the remote's config. Add it via `dfsctl` at remote-store creation time:

```bash
# Generate a passphrase-protected key file.
# DittoFS derives the file-encryption key with Argon2id,
# so high passphrase entropy gives high real entropy.
read -srp 'passphrase: ' DITTOFS_ENCRYPTION_PASSPHRASE; export DITTOFS_ENCRYPTION_PASSPHRASE

# Local-file provider
dfsctl store block remote add \
  --name s3-encrypted --type s3 --bucket prod-data \
  --encryption-aead aes-256-gcm \
  --encryption-key-kind local \
  --encryption-key-file /etc/dittofs/keys/share.key

# KMIP provider (HSM-backed master key)
dfsctl store block remote add \
  --name s3-hsm --type s3 --bucket regulated-data \
  --encryption-aead aes-256-gcm \
  --encryption-key-kind kmip \
  --encryption-kmip-endpoint kms.example.com:5696 \
  --encryption-kmip-cert /etc/dittofs/kmip/client.pem \
  --encryption-kmip-key  /etc/dittofs/kmip/client.key \
  --encryption-kmip-ca   /etc/dittofs/kmip/ca.pem \
  --encryption-kmip-key-uid 12345-abcde-...
```

Generate a fresh key file (no dedicated subcommand — call the Go helper directly):

```go
import "github.com/marmos91/dittofs/pkg/block/encryption/keyprovider"

bytes, _ := keyprovider.GenerateKeyFile("your-strong-passphrase")
os.WriteFile("/etc/dittofs/keys/share.key", bytes, 0o600)
```

## Configuration reference

```yaml
encryption:
  aead: aes-256-gcm           # aes-256-gcm | chacha20-poly1305 | xchacha20-poly1305
  key:
    kind: local               # local | kmip
    # kind=local
    file: /etc/dittofs/keys/share.key
    # kind=kmip
    endpoint: kms.example.com:5696
    server_ca: /etc/dittofs/kmip/ca.pem
    client_cert: /etc/dittofs/kmip/client.pem
    client_key:  /etc/dittofs/kmip/client.key
    key_uid: 12345-abcde-...
    timeout_ms: 5000
```

### AEAD cipher choices

| Cipher | Notes |
|--------|-------|
| `aes-256-gcm` | Hardware-accelerated on most CPUs; recommended default |
| `chacha20-poly1305` | Software-friendly; good where AES-NI is absent |
| `xchacha20-poly1305` | Extended nonce (24 bytes); lower collision probability for large volumes |

### Passphrase handling

The passphrase that unlocks a local key file is read **only** from the `DITTOFS_ENCRYPTION_PASSPHRASE` environment variable. The daemon (and `dfsctl` when it loads a provider) will fail to start if the variable is unset.

Argon2id parameters (m = 64 MiB, t = 3, p = 4) match the OWASP 2024 password-storage guidance.

## Operational warnings

Read this section before turning encryption on in production.

### Enable encryption at remote-store creation time only

Adding an `encryption` block to a remote store that already contains plaintext blocks will make every existing block **permanently unreadable** through the share — `Get` will return `ErrCiphertextWithoutFrame` because the stored bytes lack the DFENC frame header. The decorator refuses to interpret unframed bytes on an encryption-enabled share; that is intentional (any other behaviour would let a tampered-S3 actor force a plaintext downgrade).

Recommendation: create new remote stores with encryption enabled, migrate data across, then decommission the unencrypted store.

### Master-key rotation requires a full re-encrypt

There is no key-rotation tooling in this release. Every stored frame records the master-key identifier that wrapped its block key; after rotating to a new master key (writing a new `key_file` or registering a new KMIP key UID), `Unwrap` will return `ErrWrongMasterKey` for every block written under the prior key. The data is **not recoverable** without the prior master key.

If you must rotate today: keep the old master key available, stage a new remote store under the new key, and copy data across before retiring the old store. A future release will ship a bulk re-wrap command and multi-key `Unwrap`.

### AAD is per-block, not per-share

The associated data bound into the AEAD is the 32-byte BLAKE3 plaintext hash. It binds ciphertext to its CAS address but does **not** bind it to a share identity. Two shares that reference the same remote store config — and therefore share the same master key — could decrypt each other's blocks if an attacker with direct object-store write access moved blocks between share namespaces. This is acceptable for the supported configuration (one remote-store config per workload) but is a hazard if you reuse one master key across security-domain-distinct shares. Do not do that.

## What's not in scope (yet)

- **Master-key rotation tooling** — the frame already records `master_key_id`, so a future bulk rewrite job can re-wrap.
- **Filename / size / timestamp encryption** — out of scope; metadata stays unencrypted.
- **Encrypted disk cache tier** — current cache holds plaintext in RAM / disk; use an encrypted filesystem underneath if needed.
- **FIPS 140-3 mode** — would require swapping Argon2id for PBKDF2-SHA256, pinning AES-only AEADs, and building with the BoringCrypto tag.
