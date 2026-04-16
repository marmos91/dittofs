package memory_test

import (
	"bytes"
	"context"
	"encoding/gob"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/memory"
	"github.com/marmos91/dittofs/pkg/metadata/storetest"
)

// Compile-time assertion that *memory.MemoryMetadataStore satisfies
// storetest.BackupTestStore (= MetadataStore + Backupable + io.Closer).
// If MemoryMetadataStore ever drifts away from this union, this line fails
// at build time — no drive-by regression of the Backupable contract can
// merge undetected.
var _ storetest.BackupTestStore = (*memory.MemoryMetadataStore)(nil)

// TestBackupConformance runs the shared 5-subtest conformance suite against
// the memory store: RoundTrip, ConcurrentWriter, Corruption, NonEmptyDest,
// PayloadIDSet (D-08).
func TestBackupConformance(t *testing.T) {
	storetest.RunBackupConformanceSuite(t, func(t *testing.T) storetest.BackupTestStore {
		return memory.NewMemoryMetadataStoreWithDefaults()
	})
}

// TestBackupMemory_RestoreIntoSelfRejected confirms that calling Restore on
// a store that already holds its own backup returns ErrRestoreDestinationNotEmpty
// rather than wiping the live state (D-06).
func TestBackupMemory_RestoreIntoSelfRejected(t *testing.T) {
	ctx := context.Background()
	store := memory.NewMemoryMetadataStoreWithDefaults()

	if err := store.CreateShare(ctx, &metadata.Share{Name: "/export"}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	var buf bytes.Buffer
	if _, err := store.Backup(ctx, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	err := store.Restore(ctx, &buf)
	if !errors.Is(err, metadata.ErrRestoreDestinationNotEmpty) {
		t.Fatalf("expected ErrRestoreDestinationNotEmpty, got %v", err)
	}

	// Pre-existing share must still be readable.
	shares, err := store.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares after rejected Restore: %v", err)
	}
	if len(shares) != 1 || shares[0] != "/export" {
		t.Fatalf("share state mutated by rejected Restore: got %v", shares)
	}
}

// TestBackupMemory_CtxCancelBeforeBackup confirms that a pre-cancelled
// context short-circuits Backup at the entry gate and returns the ctx
// error — NOT ErrBackupAborted. ErrBackupAborted is reserved for failures
// that surface from the encoder after RLock was acquired.
func TestBackupMemory_CtxCancelBeforeBackup(t *testing.T) {
	store := memory.NewMemoryMetadataStoreWithDefaults()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	_, err := store.Backup(ctx, &buf)
	if err == nil {
		t.Fatal("Backup(cancelled ctx) returned nil error; want ctx error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Backup(cancelled ctx) returned %v; want context.Canceled", err)
	}
	if errors.Is(err, metadata.ErrBackupAborted) {
		t.Fatalf("Backup(cancelled ctx) returned ErrBackupAborted; want plain ctx error")
	}
}

// TestBackupMemory_EmptyStoreRoundTrip confirms that an empty source store
// still produces a non-empty gob stream (header + schema fields) which
// decodes cleanly into another empty store.
func TestBackupMemory_EmptyStoreRoundTrip(t *testing.T) {
	ctx := context.Background()
	src := memory.NewMemoryMetadataStoreWithDefaults()

	var buf bytes.Buffer
	ids, err := src.Backup(ctx, &buf)
	if err != nil {
		t.Fatalf("Backup(empty) failed: %v", err)
	}
	if ids.Len() != 0 {
		t.Fatalf("Backup(empty) returned non-empty PayloadIDSet: %v", ids)
	}
	if buf.Len() == 0 {
		t.Fatal("Backup(empty) produced a 0-byte stream; expected gob header + root scaffold")
	}

	dest := memory.NewMemoryMetadataStoreWithDefaults()
	if err := dest.Restore(ctx, &buf); err != nil {
		t.Fatalf("Restore(empty archive) failed: %v", err)
	}

	shares, err := dest.ListShares(ctx)
	if err != nil {
		t.Fatalf("dest.ListShares: %v", err)
	}
	if len(shares) != 0 {
		t.Fatalf("empty-archive Restore produced shares: %v", shares)
	}
}

// TestBackupMemory_EnvelopeShape confirms the Backup output begins with the
// MDFS envelope header (magic + version + length + CRC32) and that the
// payload decodes as a gob memoryBackupRoot. Combined with the Corruption
// conformance subtest (negative path) this pins the on-disk format.
func TestBackupMemory_EnvelopeShape(t *testing.T) {
	ctx := context.Background()
	src := memory.NewMemoryMetadataStoreWithDefaults()

	if err := src.CreateShare(ctx, &metadata.Share{Name: "/stream-shape"}); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	var buf bytes.Buffer
	if _, err := src.Backup(ctx, &buf); err != nil {
		t.Fatalf("Backup: %v", err)
	}

	raw := buf.Bytes()
	if len(raw) < 20 {
		t.Fatalf("stream too short (%d bytes) to carry envelope header", len(raw))
	}
	// Magic "MDFS" in little-endian: 0x53 0x46 0x44 0x4d.
	if raw[0] != 0x53 || raw[1] != 0x46 || raw[2] != 0x44 || raw[3] != 0x4d {
		t.Fatalf("envelope magic mismatch: got %#x %#x %#x %#x", raw[0], raw[1], raw[2], raw[3])
	}
	// Envelope FormatVersion at bytes 4..8 must equal 1.
	if raw[4] != 1 || raw[5] != 0 || raw[6] != 0 || raw[7] != 0 {
		t.Fatalf("envelope FormatVersion bytes not LE(1): got %#x %#x %#x %#x",
			raw[4], raw[5], raw[6], raw[7])
	}

	// Skip past the 20-byte envelope header and decode the payload as gob.
	dec := gob.NewDecoder(bytes.NewReader(raw[20:]))
	var sink struct {
		FormatVersion    uint32
		GobSchemaVersion uint32
		GoVersion        string
	}
	if err := dec.Decode(&sink); err != nil {
		t.Fatalf("envelope payload is not a gob-encoded struct: %v", err)
	}
	if sink.FormatVersion == 0 {
		t.Fatalf("decoded FormatVersion=0; archive header is missing required fields")
	}
}
