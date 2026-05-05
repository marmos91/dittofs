package migrate

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
)

// makeEntry builds a minimal JournalEntry keyed by handle.
func makeEntry(handle string) JournalEntry {
	return JournalEntry{
		Kind:       "file_done",
		FileHandle: handle,
		PayloadID:  "share/" + handle,
		Blocks: []blockstore.BlockRef{
			{Offset: 0, Size: 4 * 1024 * 1024},
		},
	}
}

// TestJournal_Append_Replay_J1 covers J1: Append + Replay round-trips
// entries in order.
func TestJournal_Append_Replay_J1(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	for _, h := range []string{"A", "B", "C"} {
		if err := j.Append(makeEntry(h)); err != nil {
			t.Fatalf("Append(%s): %v", h, err)
		}
	}
	_ = j.Close()

	j2, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("re-OpenJournal: %v", err)
	}
	defer func() { _ = j2.Close() }()
	got, err := j2.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("Replay returned %d entries, want 3", len(got))
	}
	for i, h := range []string{"A", "B", "C"} {
		if got[i].FileHandle != h {
			t.Errorf("Replay[%d].FileHandle = %q, want %q", i, got[i].FileHandle, h)
		}
	}
}

// TestJournal_SnapshotAtThreshold_J2 covers J2: Snapshot is auto-fired
// when appended count reaches the threshold; the journal log is
// truncated to zero; Replay still returns all entries.
func TestJournal_SnapshotAtThreshold_J2(t *testing.T) {
	dir := t.TempDir()
	const threshold = 5
	j, err := OpenJournalWithInterval(dir, threshold)
	if err != nil {
		t.Fatalf("OpenJournalWithInterval: %v", err)
	}
	defer func() { _ = j.Close() }()

	for i := 0; i < threshold; i++ {
		if err := j.Append(makeEntry(fmt.Sprintf("file-%03d", i))); err != nil {
			t.Fatalf("Append(%d): %v", i, err)
		}
	}
	// Snapshot file should exist.
	if _, err := os.Stat(filepath.Join(dir, SnapshotFile)); err != nil {
		t.Fatalf("expected snapshot file present, got stat err %v", err)
	}
	// Journal log should be truncated.
	size, err := j.JournalSize()
	if err != nil {
		t.Fatalf("JournalSize: %v", err)
	}
	if size != 0 {
		t.Errorf("journal log size = %d after snapshot rotation, want 0", size)
	}
	// Replay returns all 5 entries.
	got, err := j.Replay()
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if len(got) != threshold {
		t.Errorf("Replay returned %d entries, want %d", len(got), threshold)
	}
}

// TestJournal_CorruptSnapshotFallsBackToJournal_J3 covers J3: a corrupt
// snapshot file is silently ignored; Replay falls back to the journal
// contents.
func TestJournal_CorruptSnapshotFallsBackToJournal_J3(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := j.Append(makeEntry("A")); err != nil {
		t.Fatalf("Append A: %v", err)
	}
	if err := j.Append(makeEntry("B")); err != nil {
		t.Fatalf("Append B: %v", err)
	}
	_ = j.Close()

	// Corrupt snapshot file.
	spath := filepath.Join(dir, SnapshotFile)
	if err := os.WriteFile(spath, []byte("not-json{{"), 0o644); err != nil {
		t.Fatalf("write corrupt snapshot: %v", err)
	}

	j2, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("re-OpenJournal with corrupt snapshot: %v", err)
	}
	defer func() { _ = j2.Close() }()
	got, _ := j2.Replay()
	if len(got) != 2 {
		t.Errorf("Replay after corrupt snapshot: got %d entries, want 2", len(got))
	}
}

// TestJournal_AtomicRenameInvariant_J4 covers J4: snapshot writes go to
// a .tmp first; absence of a .tmp residue after Snapshot indicates the
// rename completed atomically.
func TestJournal_AtomicRenameInvariant_J4(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if err := j.Append(makeEntry("A")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := j.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	tmp := filepath.Join(dir, SnapshotFile+".tmp")
	if _, err := os.Stat(tmp); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("expected no residual .tmp file after Snapshot, got stat err %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, SnapshotFile)); err != nil {
		t.Errorf("expected snapshot file present, got %v", err)
	}
}

// TestJournal_IsFileDone_J5 covers J5: IsFileDone returns true after a
// commit and false otherwise.
func TestJournal_IsFileDone_J5(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if j.IsFileDone("A") {
		t.Error("IsFileDone(A) returned true before any Append")
	}
	if err := j.Append(makeEntry("A")); err != nil {
		t.Fatalf("Append A: %v", err)
	}
	if !j.IsFileDone("A") {
		t.Error("IsFileDone(A) returned false after Append")
	}
	if j.IsFileDone("B") {
		t.Error("IsFileDone(B) returned true without any Append")
	}
}

// TestJournal_CompactionFloor_J6 covers J6: post-Snapshot the .jsonl
// file size is 0.
func TestJournal_CompactionFloor_J6(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	if err := j.Append(makeEntry("A")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	size, _ := j.JournalSize()
	if size == 0 {
		t.Fatal("expected non-zero journal size after Append")
	}
	if err := j.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	size, _ = j.JournalSize()
	if size != 0 {
		t.Errorf("journal size after Snapshot = %d, want 0", size)
	}
}

// TestJournal_ReadOnlyRejectsWrites verifies the OpenJournalReadOnly
// invariant — Append + Snapshot return ErrJournalReadOnly.
func TestJournal_ReadOnlyRejectsWrites(t *testing.T) {
	dir := t.TempDir()
	// Seed via writer.
	w, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	if err := w.Append(makeEntry("A")); err != nil {
		t.Fatalf("seed Append: %v", err)
	}
	_ = w.Close()

	r, err := OpenJournalReadOnly(dir)
	if err != nil {
		t.Fatalf("OpenJournalReadOnly: %v", err)
	}
	defer func() { _ = r.Close() }()

	if !r.IsFileDone("A") {
		t.Error("read-only IsFileDone(A) = false, want true")
	}
	if err := r.Append(makeEntry("B")); !errors.Is(err, ErrJournalReadOnly) {
		t.Errorf("read-only Append: err = %v, want ErrJournalReadOnly", err)
	}
	if err := r.Snapshot(); !errors.Is(err, ErrJournalReadOnly) {
		t.Errorf("read-only Snapshot: err = %v, want ErrJournalReadOnly", err)
	}
}

// TestJournal_AggregateReportsPresenceFlags exercises Aggregate for the
// REST handler in Plan 14-06.
func TestJournal_AggregateReportsPresenceFlags(t *testing.T) {
	dir := t.TempDir()
	j, err := OpenJournal(dir)
	if err != nil {
		t.Fatalf("OpenJournal: %v", err)
	}
	defer func() { _ = j.Close() }()

	entries, hasJournal, hasSnapshot, _ := j.Aggregate()
	if len(entries) != 0 {
		t.Errorf("fresh Aggregate entries = %d, want 0", len(entries))
	}
	if hasJournal {
		t.Error("fresh Aggregate hasJournal = true, want false")
	}
	if hasSnapshot {
		t.Error("fresh Aggregate hasSnapshot = true, want false")
	}

	if err := j.Append(makeEntry("A")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	entries, hasJournal, hasSnapshot, lastTS := j.Aggregate()
	if len(entries) != 1 {
		t.Errorf("post-append entries = %d, want 1", len(entries))
	}
	if !hasJournal {
		t.Error("post-append hasJournal = false, want true")
	}
	if hasSnapshot {
		t.Error("post-append hasSnapshot = true, want false (no Snapshot yet)")
	}
	if lastTS.IsZero() {
		t.Error("post-append lastTS is zero")
	}

	if err := j.Snapshot(); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	_, _, hasSnapshot, _ = j.Aggregate()
	if !hasSnapshot {
		t.Error("post-Snapshot hasSnapshot = false, want true")
	}
}
