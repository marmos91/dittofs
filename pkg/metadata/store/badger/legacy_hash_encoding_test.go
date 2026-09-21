package badger

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/marmos91/dittofs/pkg/block"
)

// legacyArrayHashRow renders a FileChunk row the way builds without a custom
// ContentHash MarshalJSON wrote it: the hash as a JSON array of byte values
// rather than the "blake3:{hex}" string. Built from the hash itself so the
// fixture cannot drift from the bytes it claims to encode.
func legacyArrayHashRow(id string, hash block.ContentHash, dataSize uint32) string {
	elems := make([]string, len(hash))
	for i, b := range hash {
		elems[i] = fmt.Sprintf("%d", b)
	}
	return fmt.Sprintf(
		`{"ID":%q,"Hash":[%s],"DataSize":%d,"StartOffset":0,"RefCount":1,"state":2}`,
		id, strings.Join(elems, ","), dataSize,
	)
}

// TestListFileChunks_ReadsLegacyArrayHashEncoding pins the compatibility
// contract at the layer that consumes it rather than at UnmarshalJSON, which
// produces it. listFileChunksTxn skips any fb: row whose value fails to
// unmarshal and returns a nil error, so a hash encoding this build cannot
// decode does not surface as an error here: the row simply leaves the listing.
// An extents caller reading off that list then reports the range as a hole, so
// the observable symptom of dropping the legacy encoding is stored bytes
// reading back as zeros, not a failure. Asserting on the returned rows is
// therefore the assertion that catches such a removal.
func TestListFileChunks_ReadsLegacyArrayHashEncoding(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "metadata.db")
	store, err := NewBadgerMetadataStoreWithDefaults(ctx, dbPath)
	if err != nil {
		t.Fatalf("NewBadgerMetadataStoreWithDefaults: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })

	const payloadID = "legacy-payload"
	const chunkID = payloadID + "/0"
	hash, err := block.ParseContentHash(
		"af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262")
	if err != nil {
		t.Fatalf("ParseContentHash: %v", err)
	}

	// Write the row exactly as a legacy build left it: the raw value plus the
	// per-file secondary index Put would have maintained.
	if err := store.db.Update(func(txn *badgerdb.Txn) error {
		if err := txn.Set([]byte(fileChunkPrefix+chunkID),
			[]byte(legacyArrayHashRow(chunkID, hash, 4096))); err != nil {
			return err
		}
		return txn.Set([]byte(fileChunkFilePrefix+payloadID+":0"), []byte(chunkID))
	}); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	got, err := store.ListFileChunks(ctx, payloadID)
	if err != nil {
		t.Fatalf("ListFileChunks: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListFileChunks returned %d rows, want 1 — a legacy-encoded row "+
			"was dropped from the listing with a nil error, which surfaces as a "+
			"hole rather than an error", len(got))
	}
	if got[0].ID != chunkID {
		t.Errorf("ID = %q, want %q", got[0].ID, chunkID)
	}
	if got[0].Hash != hash {
		t.Errorf("Hash = %x, want %x", got[0].Hash[:], hash[:])
	}
	if got[0].DataSize != 4096 {
		t.Errorf("DataSize = %d, want 4096", got[0].DataSize)
	}
}
