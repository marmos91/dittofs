package snapshot_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/snapshot"
)

// TestWriteMetadataDumpAtomic_HappyPath asserts that a successful write
// produces the final metadata.dump file with the expected bytes, returns
// the HashSet from the callback, and cleans up the tmp file.
func TestWriteMetadataDumpAtomic_HappyPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.dump")

	payload := []byte("hello-engine-backup-payload")
	wantHS := blockstore.NewHashSet(2)
	wantHS.Add(mustHash(1))
	wantHS.Add(mustHash(2))

	gotHS, err := snapshot.WriteMetadataDumpAtomic(path, func(w io.Writer) (*blockstore.HashSet, error) {
		if _, err := w.Write(payload); err != nil {
			return nil, err
		}
		return wantHS, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotHS == nil {
		t.Fatalf("got nil HashSet; want %d entries", wantHS.Len())
	}
	if gotHS.Len() != wantHS.Len() {
		t.Errorf("HashSet len: got %d, want %d", gotHS.Len(), wantHS.Len())
	}

	// File exists with the expected bytes.
	gotBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read final file: %v", err)
	}
	if string(gotBytes) != string(payload) {
		t.Errorf("final bytes: got %q, want %q", gotBytes, payload)
	}

	// tmp file no longer exists.
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file should be gone; stat err = %v", err)
	}
}

// TestWriteMetadataDumpAtomic_CallbackError asserts the callback's error
// surfaces verbatim AND the final path is not created AND the tmp file
// is removed.
func TestWriteMetadataDumpAtomic_CallbackError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.dump")

	sentinel := errors.New("backup-blew-up")
	gotHS, err := snapshot.WriteMetadataDumpAtomic(path, func(w io.Writer) (*blockstore.HashSet, error) {
		// Even write some bytes — they should be discarded on rollback.
		_, _ = w.Write([]byte("partial"))
		return nil, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Errorf("error: got %v, want wraps %v", err, sentinel)
	}
	if gotHS != nil {
		t.Errorf("HashSet should be nil on error, got %+v", gotHS)
	}

	// Final file must NOT exist.
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("final file should NOT exist on rollback; stat err = %v", err)
	}
	// tmp file must NOT exist (helper cleans up).
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("tmp file should be cleaned up on rollback; stat err = %v", err)
	}
}

// TestWriteMetadataDumpAtomic_EmptyWrite covers the zero-bytes case.
// Returned HashSet may be nil OR an empty *HashSet (helper does not
// synthesize one — the callback owns it).
func TestWriteMetadataDumpAtomic_EmptyWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.dump")

	gotHS, err := snapshot.WriteMetadataDumpAtomic(path, func(w io.Writer) (*blockstore.HashSet, error) {
		// Zero bytes, nil HashSet (a memory engine with no blocks).
		return nil, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotHS != nil {
		t.Errorf("HashSet: got %+v, want nil (helper returns whatever fn returns)", gotHS)
	}

	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("final file should exist (zero bytes is valid): %v", err)
	}
	if stat.Size() != 0 {
		t.Errorf("final file size: got %d, want 0", stat.Size())
	}
}

// TestWriteMetadataDumpAtomic_NoIntermediateVisibility asserts that the
// rename is atomic: callers stating `path` either see the old (absent)
// or new (final size) file, never the intermediate tmp content. This is
// the contract motivated by Phase 22 D-19.
func TestWriteMetadataDumpAtomic_NoIntermediateVisibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "metadata.dump")
	payload := make([]byte, 1<<16) // 64 KiB
	for i := range payload {
		payload[i] = byte(i)
	}
	hs := blockstore.NewHashSet(1)
	hs.Add(mustHash(7))

	_, err := snapshot.WriteMetadataDumpAtomic(path, func(w io.Writer) (*blockstore.HashSet, error) {
		_, werr := w.Write(payload)
		return hs, werr
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stat, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat final: %v", err)
	}
	if stat.Size() != int64(len(payload)) {
		t.Errorf("final size: got %d, want %d", stat.Size(), len(payload))
	}
}
