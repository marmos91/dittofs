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
    retired_files:            # optional, decrypt-only (see rotation below)
      - /etc/dittofs/keys/share-2026-02.key
    # kind=kmip
    endpoint: kms.example.com:5696
    server_ca: /etc/dittofs/kmip/ca.pem
    client_cert: /etc/dittofs/kmip/client.pem
    client_key:  /etc/dittofs/kmip/client.key
    key_uid: 12345-abcde-...
    retired_key_uids:         # optional, decrypt-only
      - 09876-zyxwv-...
    timeout_ms: 5000
```

`file` / `key_uid` name the **current** master key: everything written from
now on is wrapped under it. `retired_files` / `retired_key_uids` name keys
that are used for decryption only. Both retired lists default to empty, so
a config written before rotation existed keeps behaving exactly as it did.

All retired keys share the current key's passphrase
(`DITTOFS_ENCRYPTION_PASSPHRASE`); there is no per-file passphrase.

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

### Retiring a master key is one-way — you cannot un-retire what you deleted

Rotation is supported, and does not re-encrypt any data. Every stored frame
records the identifier of the master key that wrapped its block key, so
`Unwrap` routes each block to the key it was written under.

To rotate:

1. Generate the new key file the same way you generated the first one (the
   `GenerateKeyFile` helper above — there is no dedicated subcommand), or
   register the new key UID with the HSM.
2. Move the **current** `file` / `key_uid` value into `retired_files` /
   `retired_key_uids`.
3. Point `file` / `key_uid` at the new key.
4. Restart the share.

From that point, new blocks are wrapped under the new key and old blocks
keep decrypting under the retired one. Nothing is ever wrapped under a
retired key again.

The hazard is step 2 in reverse. Dropping a key from `retired_files`, or
deleting the key file it names, makes every block still wrapped under that
key **permanently unreadable** — there is no bulk re-wrap command yet, so
there is no supported way to move existing blocks onto the current key and
no way to enumerate which blocks still reference an old one. Until that
ships, treat retired keys as keep-forever: leave them configured and keep
the key material backed up. Retiring a key costs one HSM fetch or one file
read at startup, so a handful of them is not a burden worth trimming.

Two operational notes:

- A retired key that cannot be read at startup is logged and skipped rather
  than being fatal — blocks under it become unreadable, but the share (and
  every other share on the daemon) still starts. A missing **current** key
  is still fatal.
- Two keys claiming the same identifier is rejected at startup, because
  which one `Unwrap` picks would otherwise be arbitrary. This is what
  listing the same file twice, or listing the current key as retired, will
  produce.

### AAD is per-block, not per-share

The associated data bound into the AEAD is the 32-byte BLAKE3 plaintext hash. It binds ciphertext to its CAS address but does **not** bind it to a share identity. Two shares that reference the same remote store config — and therefore share the same master key — could decrypt each other's blocks if an attacker with direct object-store write access moved blocks between share namespaces. This is acceptable for the supported configuration (one remote-store config per workload) but is a hazard if you reuse one master key across security-domain-distinct shares. Do not do that.

## What's not in scope (yet)

- **Bulk re-wrap** — rotation works (see above), but there is no job that reads blocks under a retired key and rewrites them under the current one, and no way to enumerate which blocks still reference a given key. Both are required before a retired key can ever be safely dropped.
- **Filename / size / timestamp encryption** — out of scope; metadata stays unencrypted.
- **Encrypted disk cache tier** — current cache holds plaintext in RAM / disk; use an encrypted filesystem underneath if needed.
- **FIPS 140-3 mode** — would require swapping Argon2id for PBKDF2-SHA256, pinning AES-only AEADs, and building with the BoringCrypto tag.
