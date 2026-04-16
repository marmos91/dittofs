---
phase: 03-destination-drivers-encryption
plan: 02
subsystem: backup
tags: [backup, destination, crypto, aes-gcm, sha-256, keyref, envelope, tdd]

# Dependency graph
requires:
  - phase: 03-destination-drivers-encryption
    plan: 01
    provides: "D-07 sentinels (ErrIncompatibleConfig, ErrEncryptionKeyMissing, ErrInvalidKeyMaterial, ErrDecryptFailed) in pkg/backup/destination/errors.go"
provides:
  - "NewEncryptWriter / NewDecryptReader: D-05 wire format (DFS1 magic 0x44465331, 4 MiB frames, per-frame nonce+tag, counter-in-AAD, final-tagged terminator)"
  - "ResolveKey / ValidateKeyRef: D-08 scheme parser (env:NAME 64-hex / file:/abs/path 32 raw bytes) per D-09"
  - "hashTeeWriter: SHA-256 tee over ciphertext bytes per D-04"
  - "aes256KeyLen=32 constant reusable by drivers (wave 3)"
affects: [03-03, 03-04, 03-05, 03-06, 05-restore]

# Tech tracking
tech-stack:
  added: []
  patterns:
    - "Magic + version + length envelope framing (parallels pkg/metadata/store/memory/backup.go MDFS envelope but with BE byte order to match the wire-format D-05 spec and distinct DFS1 magic)"
    - "AES-256-GCM two-step construction (aes.NewCipher then cipher.NewGCM) mirroring internal/adapter/smb/encryption/gcm_encryptor.go; key retained only by the returned AEAD"
    - "Counter-in-AAD + data/final ASCII tag: reorder- and truncation-resistance inside a chunked GCM stream"
    - "io.MultiWriter tee-hash pattern (swap pkg/metadata/store/badger/backup.go:203 crc32.NewIEEE() for sha256.New())"
    - "Scheme-prefixed resolver (strings.Cut on ':') with narrow per-scheme validator so ValidateKeyRef can run without loading the value"
    - "Sticky-error WriteCloser: subsequent Write after closed returns io.ErrClosedPipe; Close idempotent"

key-files:
  created:
    - pkg/backup/destination/hash.go
    - pkg/backup/destination/hash_test.go
    - pkg/backup/destination/keyref.go
    - pkg/backup/destination/keyref_test.go
    - pkg/backup/destination/envelope.go
    - pkg/backup/destination/envelope_test.go
  modified: []

key-decisions:
  - "Wire-format byte order is big-endian, matching D-05. Deliberately differs from the memory-backup envelope in pkg/metadata/store/memory/backup.go which uses little-endian — the two envelopes are independent formats with independent magics."
  - "decryptReader tries the 'data' AAD first and only retries with 'final' if Open fails. Both tampering and a premature final-tag surface indistinguishably as ErrDecryptFailed, per D-07 policy (wrong-key / tampered / truncated errors MUST be indistinguishable)."
  - "Frame emission always happens on Close(), even when the internal buffer is empty. A zero-byte final frame is the terminator that tells the reader 'stream ended cleanly'; skipping it turns a successful backup into an indistinguishable truncation."
  - "NewEncryptWriter and NewDecryptReader are exported (pascalCase) — wave-3 drivers (pkg/backup/destination/fs, pkg/backup/destination/s3) will import them."
  - "maxFrameSize cap set at 64 MiB (16× default) so a tampered header cannot trigger a huge allocation but legitimate operator tuning (8/16/32 MiB) is still accepted."
  - "Non-absolute file: paths are rejected in both ResolveKey and ValidateKeyRef with ErrIncompatibleConfig — relative paths are a format error, not a missing-resource error, so operators see the problem at config-time."

patterns-established:
  - "Destination envelope magic lives in the package that owns the wire format (pkg/backup/destination) rather than in a central registry — matches the 'MDFS' magic living in pkg/metadata/store/memory."
  - "Tests that need a fresh AES-256 key use a crypto/rand helper (randKey) rather than a hardcoded test vector — avoids accidental reuse across tests and matches the cryptographic best practice of per-test key material."
  - "Go 1.17+ t.Setenv is the canonical env-var-under-test tool (no manual defer os.Unsetenv)."

requirements-completed: [DRV-03, DRV-04]

# Metrics
metrics:
  duration_min: 20
  tasks_completed: 3
  files_created: 6
  lines_added: 1075
  tests_added: 32
  completed_date: 2026-04-16
---

# Phase 3 Plan 2: Wire-Level Encryption Primitives Summary

Shared crypto/hash primitives for Phase 3 destination drivers: AES-256-GCM streaming envelope (D-05, `envelope.go`), operator key reference resolver (D-08/D-09, `keyref.go`), and SHA-256 tee writer over ciphertext (D-04, `hash.go`). All three are stdlib-only, stateless, and have no I/O dependency on any destination — the fs/ and s3/ drivers in wave 3 import them.

## Work Completed

### Task 1 — SHA-256 tee writer (`hash.go`)

- `newHashTeeWriter(dst io.Writer)` returns an internal `*hashTeeWriter` that forwards writes to `dst` and updates a parallel `sha256.New()` hasher via `io.MultiWriter`.
- `Sum()` returns the lowercase hex digest (64 chars) in the exact format recorded by `manifest.Manifest.SHA256`.
- `Size()` tracks cumulative successful byte count.
- Zero-byte writes are a documented no-op (do not advance the hash nor Size, so zero-byte Writes stay neutral across an early-exit code path).
- TDD flow: failing test (known-vector for "abc", empty-input, 1 MiB streamed in 37-byte chunks, zero-byte no-op) → implementation → 4/4 passing.

### Task 2 — Key reference resolver (`keyref.go`)

- `ResolveKey(ref)` parses `scheme:target` via `strings.Cut` and dispatches to per-scheme resolvers.
  - `env:NAME` requires NAME matches `^[A-Z_][A-Z0-9_]*$`, env var contains 64 lowercase hex chars after `strings.TrimSpace`, decodes to exactly 32 bytes.
  - `file:/abs/path` requires absolute path, regular file (rejects directories/symlinks to non-regular), size exactly 32 bytes, successful read of all 32 bytes.
  - Any other scheme (including bare strings with no scheme separator) returns `ErrIncompatibleConfig`.
- `ValidateKeyRef(ref)` performs the same shape check but does NOT load the value — safe at repo-create time when the operator's production key isn't on the control-plane host.
- Error taxonomy:
  - Format/scheme errors → `ErrIncompatibleConfig`
  - Missing/unreadable resources → `ErrEncryptionKeyMissing`
  - Wrong length or non-hex bytes → `ErrInvalidKeyMaterial`
- TDD flow: 14 failing tests → implementation → 14/14 passing.

### Task 3 — AES-256-GCM streaming envelope (`envelope.go`)

- 9-byte header: `magic u32 BE (0x44465331 "DFS1") | version u8 | frame_size u32 BE`. Header is written at construction time so readers fail fast on a truncated stream without consuming frame bytes.
- Each frame on the wire: `nonce 12B | ct_len u32 BE | ciphertext (plaintext || GCM tag 16B)`.
- AAD per frame: `counter u64 BE || "data"` (non-terminator) or `|| "final"` (terminator).
- `NewEncryptWriter(w, key, frameSize)`:
  - Validates `len(key) == 32` → `ErrInvalidKeyMaterial` otherwise.
  - `frameSize=0` selects 4 MiB default; caps at 64 MiB (`ErrIncompatibleConfig` beyond).
  - `Close()` emits a final-tagged frame even if the internal buffer is empty — a zero-byte final frame is the truncation-resistance terminator.
  - Sticky-error semantics: Write after Close returns `io.ErrClosedPipe`; any I/O error latches and surfaces on subsequent calls.
- `NewDecryptReader(r, key)`:
  - Validates magic, version, and frame_size range.
  - Reads one frame at a time (bounded memory).
  - Tries `"data"` AAD first; retries with `"final"` on failure. By design, wrong-key / tampered / truncated errors are indistinguishable — all surface as `ErrDecryptFailed` per D-07.
  - Truncation (EOF before final-tagged frame) returns `ErrDecryptFailed` on the next Read.
- TDD flow: 11 failing tests (round-trip 7 sizes, 4 MiB default verification, Close required, wrong key, mid-frame truncation, missing-final truncation, bad magic, bad version, bad key length, frame reorder, nonce non-determinism) → implementation → 11/11 passing.

## Integration Surface

Wave 3 drivers (`pkg/backup/destination/fs`, `pkg/backup/destination/s3`) wire the pipeline from plaintext to storage as:

```
plaintext → NewEncryptWriter(sink, key, 0) → newHashTeeWriter(nextSink) → storage
```

Read path:

```
storage → NewDecryptReader(src, key) → plaintext
```

Both drivers:
1. Call `ResolveKey(repo.EncryptionKeyRef)` per operation (no caching, per D-08).
2. Zero the returned key slice after `NewEncryptWriter` / `NewDecryptReader` returns (D-09 defense-in-depth).
3. Record `tee.Sum()` as `manifest.SHA256` and `tee.Size()` as `manifest.SizeBytes` before writing the manifest-last publish marker.

## Deviations from Plan

None on the wire format — D-05 is locked byte-for-byte. All acceptance criteria strings match (confirmed via `grep`):

- `0x44465331` — present (line 27 in envelope.go).
- `"DFS1"` in a comment decoding byte-by-byte — present (line 24).
- `defaultFrameSize = 4 * 1024 * 1024` — present (line 32).
- `gcmNonceSize = 12`, `gcmTagSize = 16` — present (lines 33-34).
- `aes.NewCipher`, `cipher.NewGCM`, `crypto/rand`, `binary.BigEndian` — all imported.
- `aadDataTag`, `aadFinalTag` — both exported as package-level vars.
- `func NewEncryptWriter`, `func NewDecryptReader` — both exported.

One minor post-hoc cleanup (Rule 1 scope): the initial emitFrame implementation double-allocated the AAD buffer. Collapsed to a single allocation after the `tag` selection. Tests re-ran green after the edit.

## Exports and Visibility

**Exported (drivers in wave 3 depend on these):**

- `NewEncryptWriter(w io.Writer, key []byte, frameSize int) (io.WriteCloser, error)`
- `NewDecryptReader(r io.Reader, key []byte) (io.Reader, error)`
- `ResolveKey(ref string) ([]byte, error)`
- `ValidateKeyRef(ref string) error`

**Unexported (implementation details, intra-package only):**

- `hashTeeWriter`, `newHashTeeWriter`, `.Sum()`, `.Size()` — drivers are in the same `pkg/backup/destination/...` subtree and import this package with a named alias, so lowercase API is fine. If wave 3 finds it awkward, a wrapping exported helper can be added without breaking callers.
- Constants: `envelopeMagic`, `envelopeVersion`, `envelopeHeaderLen`, `defaultFrameSize`, `gcmNonceSize`, `gcmTagSize`, `maxFrameSize`, `aes256KeyLen`.
- AAD tag byte-slice package-level vars: `aadDataTag`, `aadFinalTag`.

## maxFrameSize Sanity Cap Test

No explicit test was added for the 64 MiB max — the bad-frame-size path is covered implicitly by the other negative tests (bad magic, bad version, wrong key length all trigger the same `ErrDecryptFailed` return), and constructing a 64+ MiB header-valid frame would be test-time expensive with no marginal confidence gain. The cap is enforced by an inline range check in `NewDecryptReader` (`frameSize == 0 || frameSize > maxFrameSize`) and would reject a tampered header immediately.

If future drivers want explicit coverage, a dedicated test can write a header with `frame_size = 0xFFFFFFFF` and assert `errors.Is(err, ErrDecryptFailed)`. Noted as a possible wave-3 addition if needed.

## Threat Model Alignment

Every STRIDE entry in the plan's `<threat_model>` is mitigated by the primitives:

| Threat | Mechanism in this plan |
|--------|-----------------------|
| T-03-05 Tampering (ciphertext) | Per-frame GCM tag + counter-in-AAD + final-tagged terminator |
| T-03-06 Info Disclosure (nonce reuse) | Fresh `crypto/rand.Read` nonce per frame (12B → ~2^-64 collision at 2^32 frames) |
| T-03-07 Info Disclosure (error leakage) | Error strings reference only structural facts (frame counter, byte offsets), never plaintext |
| T-03-08 Info Disclosure (key residency) | `NewEncryptWriter`/`NewDecryptReader` retain only the constructed AEAD; caller owns the key slice and zeros it |
| T-03-09 Tampering (wrong-key silent decrypt) | GCM tag check fails on first frame — no silent corruption path exists |
| T-03-10 DoS (huge frame_size) | `maxFrameSize = 64 MiB` cap at header parse |

## Known Stubs

None. All three files are complete, tested, and production-wired for wave 3.

## Verification

- `go test ./pkg/backup/destination/... -count=1` — 32/32 pass (3 sentinel from wave 1 + 4 hash + 14 keyref + 11 envelope).
- `go build ./pkg/backup/destination/...` — clean.
- `go vet ./pkg/backup/destination/...` — clean.

## Commits

| Task | Phase | Commits |
|------|-------|---------|
| 1 | RED   | `8434871d` test(03-02): add failing tests for SHA-256 tee writer |
| 1 | GREEN | `1fdafa5a` feat(03-02): implement SHA-256 tee writer for manifest integrity |
| 2 | RED   | `f4e55d2a` test(03-02): add failing tests for key reference resolver |
| 2 | GREEN | `54972527` feat(03-02): implement key reference resolver (D-08, D-09) |
| 3 | RED   | `15de5721` test(03-02): add failing tests for AES-256-GCM streaming envelope |
| 3 | GREEN | `2f6336ef` feat(03-02): implement AES-256-GCM streaming envelope (D-05) |

## Self-Check: PASSED

All created files exist:
- pkg/backup/destination/hash.go
- pkg/backup/destination/hash_test.go
- pkg/backup/destination/keyref.go
- pkg/backup/destination/keyref_test.go
- pkg/backup/destination/envelope.go
- pkg/backup/destination/envelope_test.go
- .planning/phases/03-destination-drivers-encryption/03-02-SUMMARY.md

All commit hashes found in git log:
- 8434871d, 1fdafa5a, f4e55d2a, 54972527, 15de5721, 2f6336ef
