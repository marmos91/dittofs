// Package destination provides the driver contract for publishing backup
// archives to a backing store (local FS, S3). Drivers own atomic publish
// ordering, AES-256-GCM encryption, and SHA-256 integrity. See Phase 3
// CONTEXT.md for design decisions D-01..D-14.
package destination

import "errors"

// Transient / retryable errors. Orchestrator (Phase 4 scheduler, Phase 5
// restore trigger) is the single place where retry is implemented. Drivers
// do not retry internally beyond the AWS SDK's own MaxRetries.
var (
	// ErrDestinationUnavailable indicates the destination is not reachable
	// (network failure, 5xx response, DNS lookup failure). Callers may retry.
	ErrDestinationUnavailable = errors.New("destination unavailable")

	// ErrDestinationThrottled indicates the destination rejected the
	// request for rate-limit reasons (HTTP 429, S3 SlowDown). Callers may
	// retry after backoff.
	ErrDestinationThrottled = errors.New("destination throttled")
)

// Permanent errors. The orchestrator must NOT retry these.
var (
	// ErrIncompatibleConfig indicates the driver configuration is
	// structurally invalid or references resources that do not exist
	// (missing bucket, non-writable path, bucket/prefix collision with
	// a registered block store).
	ErrIncompatibleConfig = errors.New("incompatible destination config")

	// ErrPermissionDenied indicates the destination rejected the request
	// for authorization reasons (HTTP 403, EACCES).
	ErrPermissionDenied = errors.New("permission denied")

	// ErrDuplicateBackupID indicates a backup with the same ULID already
	// exists at the destination. Vanishingly rare (ULID collision).
	ErrDuplicateBackupID = errors.New("duplicate backup id")

	// ErrSHA256Mismatch indicates the SHA-256 computed while streaming
	// the payload does not match manifest.sha256. Signals corruption on
	// storage or in flight.
	ErrSHA256Mismatch = errors.New("sha256 mismatch on read-back")

	// ErrManifestMissing indicates no manifest.yaml exists for the
	// requested backup id. Restore callers receive this when asked for
	// a non-existent or orphaned backup.
	ErrManifestMissing = errors.New("manifest.yaml missing for backup id")

	// ErrEncryptionKeyMissing indicates the encryption_key_ref referenced
	// a key source that could not be resolved (env var unset, file
	// missing or unreadable).
	ErrEncryptionKeyMissing = errors.New("encryption key not resolvable")

	// ErrInvalidKeyMaterial indicates the resolved key material is not
	// a valid AES-256 key (file not exactly 32 bytes; env var not
	// exactly 64 lowercase hex characters).
	ErrInvalidKeyMaterial = errors.New("invalid key material (not 32 bytes)")

	// ErrDecryptFailed indicates AES-256-GCM decryption failed. Causes
	// are indistinguishable by design: wrong key, tampered ciphertext,
	// or truncated stream (missing final-tagged frame).
	ErrDecryptFailed = errors.New("decrypt failed (wrong key, tampered, or truncated)")

	// ErrIncompleteBackup indicates a backup directory/prefix contains
	// payload.bin but no manifest.yaml. Published backups always have
	// a manifest (manifest-last invariant, D-02/D-03); an entry without
	// one is an orphan and must not be restored.
	ErrIncompleteBackup = errors.New("incomplete backup (payload present, manifest absent)")
)
