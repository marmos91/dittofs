package remotetest

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"lukechampine.com/blake3"

	"github.com/marmos91/dittofs/pkg/blockstore"
	"github.com/marmos91/dittofs/pkg/blockstore/remote"
	"github.com/marmos91/dittofs/pkg/health"
)

// blake3Sum256 is a thin wrapper so the conformance suite can compute
// content hashes for ReadBlockVerified test fixtures without leaking the
// blake3 import into every helper signature.
func blake3Sum256(data []byte) [32]byte {
	return blake3.Sum256(data)
}

// Factory creates a new RemoteStore instance for testing.
type Factory func(t *testing.T) remote.RemoteStore

// MetadataInspector is an optional capability a RemoteStore implementation
// can advertise so the conformance suite can verify the x-amz-meta-content-hash
// header round-trip (BSCAS-06). For S3 backends this is satisfied by an
// out-of-band HeadObject; for the memory backend this is the in-process
// metadata map. Stores that cannot expose per-object metadata (or where
// inspection is too expensive for the suite) simply do not implement this
// interface — the suite's WriteBlockWithHash sub-tests then skip the header
// assertion but still exercise the write path itself.
type MetadataInspector interface {
	GetObjectMetadata(blockKey string) map[string]string
}

// RunSuite runs the full conformance test suite against a RemoteStore implementation.
func RunSuite(t *testing.T, factory Factory) {
	t.Run("WriteAndRead", func(t *testing.T) { testWriteAndRead(t, factory) })
	t.Run("ReadNotFound", func(t *testing.T) { testReadNotFound(t, factory) })
	t.Run("ReadBlockRange", func(t *testing.T) { testReadBlockRange(t, factory) })
	t.Run("DeleteBlock", func(t *testing.T) { testDeleteBlock(t, factory) })
	t.Run("DeleteByPrefix", func(t *testing.T) { testDeleteByPrefix(t, factory) })
	t.Run("ListByPrefix", func(t *testing.T) { testListByPrefix(t, factory) })
	t.Run("HealthCheck", func(t *testing.T) { testHealthCheck(t, factory) })
	t.Run("HealthcheckReport", func(t *testing.T) { testHealthcheckReport(t, factory) })
	t.Run("CopyBlock", func(t *testing.T) { testCopyBlock(t, factory) })
	t.Run("CopyBlockNotFound", func(t *testing.T) { testCopyBlockNotFound(t, factory) })
	t.Run("ClosedOperations", func(t *testing.T) { testClosedOperations(t, factory) })
	t.Run("DataIsolation", func(t *testing.T) { testDataIsolation(t, factory) })
	t.Run("WriteBlockWithHash", func(t *testing.T) { RunWriteBlockWithHashSuite(t, factory) })
	t.Run("ReadBlockVerified", func(t *testing.T) { RunReadBlockVerifiedSuite(t, factory) })
	t.Run("ListByPrefixWithMeta_LastModifiedNonZero", func(t *testing.T) {
		testListByPrefixWithMetaLastModifiedNonZero(t, factory)
	})
}

// testListByPrefixWithMetaLastModifiedNonZero asserts the WR-4-02 contract:
// every object surfaced by ListByPrefixWithMeta MUST carry a non-zero
// LastModified. The GC sweep (D-05) fails closed on a zero value because it
// cannot evaluate the snapshot - GracePeriod TTL filter without one.
// Backends that cannot natively report a timestamp must stamp time.Now()
// at WriteBlock / WriteBlockWithHash time.
func testListByPrefixWithMetaLastModifiedNonZero(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	hash, key := fixedHash(t)
	if err := store.WriteBlockWithHash(ctx, key, hash, []byte("wr-4-02 fixture")); err != nil {
		t.Fatalf("WriteBlockWithHash setup failed: %v", err)
	}

	objs, err := store.ListByPrefixWithMeta(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefixWithMeta failed: %v", err)
	}
	if len(objs) == 0 {
		t.Fatalf("ListByPrefixWithMeta returned 0 objects after WriteBlockWithHash; expected >=1")
	}
	for _, o := range objs {
		if o.LastModified.IsZero() {
			t.Errorf("object %q has zero LastModified — WR-4-02 contract violation: "+
				"GC sweep would fail closed and capture this as an error rather than "+
				"applying the grace TTL filter", o.Key)
		}
	}
}

// RunReadBlockVerifiedSuite exercises the INV-06 contract: ReadBlockVerified
// MUST return ErrCASContentMismatch if the body's BLAKE3 hash does not match
// the expected hash, and MUST surface bytes intact on the happy path.
func RunReadBlockVerifiedSuite(t *testing.T, factory Factory) {
	t.Run("HappyPath", func(t *testing.T) { testReadBlockVerifiedHappyPath(t, factory) })
	t.Run("BodyMismatch", func(t *testing.T) { testReadBlockVerifiedBodyMismatch(t, factory) })
	t.Run("HeaderPreCheck", func(t *testing.T) { testReadBlockVerifiedHeaderPreCheck(t, factory) })
	t.Run("NotFound", func(t *testing.T) { testReadBlockVerifiedNotFound(t, factory) })
}

// payloadAndHash returns a deterministic payload and its BLAKE3-256 hash
// (already wrapped as a ContentHash). Used by the verified-read sub-suite
// so the assertions are in terms of "this exact payload should hash to
// this exact value", without brute-forcing arbitrary fixed hashes.
func payloadAndHash(_ *testing.T, payload []byte) (blockstore.ContentHash, string) {
	sum := hashBytes(payload)
	return sum, blockstore.FormatCASKey(sum)
}

// hashBytes computes BLAKE3-256 of data and returns it as a ContentHash.
func hashBytes(data []byte) blockstore.ContentHash {
	sum := blake3Sum256(data)
	var h blockstore.ContentHash
	copy(h[:], sum[:])
	return h
}

func testReadBlockVerifiedHappyPath(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("verified-read happy-path payload — INV-06 round trip")
	hash, key := payloadAndHash(t, data)

	if err := store.WriteBlockWithHash(ctx, key, hash, data); err != nil {
		t.Fatalf("WriteBlockWithHash: %v", err)
	}
	got, err := store.ReadBlockVerified(ctx, key, hash)
	if err != nil {
		t.Fatalf("ReadBlockVerified: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("ReadBlockVerified bytes mismatch: got %q, want %q", got, data)
	}
}

func testReadBlockVerifiedBodyMismatch(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Compute a hash for one payload, then stash a DIFFERENT payload at
	// that key via legacy WriteBlock (no header). The header pre-check
	// is therefore inert; the body recompute must surface the mismatch.
	expectedPayload := []byte("payload that the caller expects")
	hash, key := payloadAndHash(t, expectedPayload)
	wrongPayload := []byte("DIFFERENT bytes — should fail body recompute")

	if err := store.WriteBlock(ctx, key, wrongPayload); err != nil {
		t.Fatalf("WriteBlock setup: %v", err)
	}
	_, err := store.ReadBlockVerified(ctx, key, hash)
	if err == nil {
		t.Fatal("ReadBlockVerified: expected ErrCASContentMismatch, got nil")
	}
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("ReadBlockVerified err = %v, want wrapped ErrCASContentMismatch", err)
	}
}

func testReadBlockVerifiedHeaderPreCheck(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	// Body bytes hash to wrongHash; header is x-amz-meta-content-hash =
	// wrongHash.CASKey(). We then ASK for `expectedHash`, which differs.
	// The header pre-check (D-19) MUST fire before body recompute.
	bodyBytes := []byte("body bytes; their hash is what gets in the header")
	wrongHash, key := payloadAndHash(t, bodyBytes)
	expectedHash := wrongHash
	expectedHash[0] ^= 0xFF // flip a bit so they differ

	if err := store.WriteBlockWithHash(ctx, key, wrongHash, bodyBytes); err != nil {
		t.Fatalf("WriteBlockWithHash setup: %v", err)
	}
	_, err := store.ReadBlockVerified(ctx, key, expectedHash)
	if err == nil {
		t.Fatal("ReadBlockVerified: expected ErrCASContentMismatch, got nil")
	}
	if !errors.Is(err, blockstore.ErrCASContentMismatch) {
		t.Fatalf("ReadBlockVerified err = %v, want wrapped ErrCASContentMismatch", err)
	}
}

func testReadBlockVerifiedNotFound(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	hash, key := fixedHash(t)
	_, err := store.ReadBlockVerified(ctx, key, hash)
	if err == nil {
		t.Fatal("ReadBlockVerified: expected error for nonexistent block")
	}
	if !errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Fatalf("ReadBlockVerified err = %v, want wrapped ErrBlockNotFound", err)
	}
}

// RunWriteBlockWithHashSuite exercises the BSCAS-06 contract: WriteBlockWithHash
// must stamp x-amz-meta-content-hash atomically with the PUT, the legacy
// WriteBlock path must NOT set the header, and re-uploading the same key with
// the same hash must succeed (CAS idempotency).
//
// Backends that implement MetadataInspector get exact header assertions;
// backends that do not still exercise the upload path and the data round-trip.
func RunWriteBlockWithHashSuite(t *testing.T, factory Factory) {
	t.Run("WriteBlockWithHash_SetsHeader", func(t *testing.T) {
		testWriteBlockWithHashSetsHeader(t, factory)
	})
	t.Run("WriteBlock_NoHeader", func(t *testing.T) {
		testWriteBlockNoHeader(t, factory)
	})
	t.Run("WriteBlockWithHash_OverwriteSafe", func(t *testing.T) {
		testWriteBlockWithHashOverwriteSafe(t, factory)
	})
}

// fixedHash returns a deterministic ContentHash and its CAS object key for
// use in BSCAS-06 sub-tests.
func fixedHash(t *testing.T) (blockstore.ContentHash, string) {
	t.Helper()
	const hex = "af1349b9f5f9a1a6a0404dea36dcc9499bcb25c9adc112b7cc9a93cae41f3262"
	h, err := blockstore.ParseContentHash(hex)
	if err != nil {
		t.Fatalf("setup: ParseContentHash(%q) error: %v", hex, err)
	}
	return h, blockstore.FormatCASKey(h)
}

func testWriteBlockWithHashSetsHeader(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	hash, key := fixedHash(t)
	data := []byte("payload bytes")

	if err := store.WriteBlockWithHash(ctx, key, hash, data); err != nil {
		t.Fatalf("WriteBlockWithHash failed: %v", err)
	}

	// Round-trip the data to confirm the PUT actually persisted.
	got, err := store.ReadBlock(ctx, key)
	if err != nil {
		t.Fatalf("ReadBlock(%q) failed: %v", key, err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("ReadBlock returned %q, want %q", got, data)
	}

	inspector, ok := store.(MetadataInspector)
	if !ok {
		t.Skip("backend does not implement MetadataInspector; skipping header assertion (still exercised the write path)")
	}
	md := inspector.GetObjectMetadata(key)
	if md == nil {
		t.Fatalf("GetObjectMetadata(%q) returned nil; expected content-hash entry", key)
	}
	want := hash.CASKey()
	if got := md["content-hash"]; got != want {
		t.Fatalf("content-hash header = %q, want %q", got, want)
	}
}

func testWriteBlockNoHeader(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	const blockKey = "legacy/file.bin/block-0"
	if err := store.WriteBlock(ctx, blockKey, []byte("legacy bytes")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	inspector, ok := store.(MetadataInspector)
	if !ok {
		t.Skip("backend does not implement MetadataInspector; skipping negative header assertion")
	}
	md := inspector.GetObjectMetadata(blockKey)
	if md != nil {
		if _, present := md["content-hash"]; present {
			t.Fatalf("legacy WriteBlock set content-hash header: %v", md)
		}
	}
}

func testWriteBlockWithHashOverwriteSafe(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	hash, key := fixedHash(t)
	data := []byte("idempotent payload")

	if err := store.WriteBlockWithHash(ctx, key, hash, data); err != nil {
		t.Fatalf("first WriteBlockWithHash failed: %v", err)
	}
	if err := store.WriteBlockWithHash(ctx, key, hash, data); err != nil {
		t.Fatalf("second WriteBlockWithHash failed (CAS overwrite must be safe): %v", err)
	}

	got, err := store.ReadBlock(ctx, key)
	if err != nil {
		t.Fatalf("ReadBlock failed after overwrite: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("data after overwrite = %q, want %q", got, data)
	}

	if inspector, ok := store.(MetadataInspector); ok {
		md := inspector.GetObjectMetadata(key)
		if md == nil {
			t.Fatalf("GetObjectMetadata(%q) nil after overwrite", key)
		}
		if got, want := md["content-hash"], hash.CASKey(); got != want {
			t.Fatalf("content-hash after overwrite = %q, want %q", got, want)
		}
	}
}

func testWriteAndRead(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("hello world")
	blockKey := "test/block-0"

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	read, err := store.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if !bytes.Equal(read, data) {
		t.Fatalf("ReadBlock returned %q, want %q", read, data)
	}
}

func testReadNotFound(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	_, err := store.ReadBlock(ctx, "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent block")
	}
}

func testReadBlockRange(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("hello world")
	blockKey := "test/block-0"

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	read, err := store.ReadBlockRange(ctx, blockKey, 0, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "hello" {
		t.Fatalf("ReadBlockRange returned %q, want %q", read, "hello")
	}

	read, err = store.ReadBlockRange(ctx, blockKey, 6, 5)
	if err != nil {
		t.Fatalf("ReadBlockRange failed: %v", err)
	}
	if string(read) != "world" {
		t.Fatalf("ReadBlockRange returned %q, want %q", read, "world")
	}
}

func testDeleteBlock(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	blockKey := "test/block-0"
	if err := store.WriteBlock(ctx, blockKey, []byte("data")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if err := store.DeleteBlock(ctx, blockKey); err != nil {
		t.Fatalf("DeleteBlock failed: %v", err)
	}

	_, err := store.ReadBlock(ctx, blockKey)
	if err == nil {
		t.Fatal("expected error after delete")
	}
}

func testDeleteByPrefix(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		key := "prefix1/block-" + string(rune('0'+i))
		if err := store.WriteBlock(ctx, key, []byte("data")); err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}
	}
	if err := store.WriteBlock(ctx, "prefix2/block-0", []byte("data")); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if err := store.DeleteByPrefix(ctx, "prefix1/"); err != nil {
		t.Fatalf("DeleteByPrefix failed: %v", err)
	}

	keys, err := store.ListByPrefix(ctx, "prefix1/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 0 {
		t.Fatalf("expected 0 keys after delete, got %d", len(keys))
	}

	keys, err = store.ListByPrefix(ctx, "prefix2/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 1 {
		t.Fatalf("expected 1 key remaining, got %d", len(keys))
	}
}

func testListByPrefix(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	for _, key := range []string{"a/block-0", "a/block-1", "b/block-0"} {
		if err := store.WriteBlock(ctx, key, []byte("data")); err != nil {
			t.Fatalf("WriteBlock failed: %v", err)
		}
	}

	keys, err := store.ListByPrefix(ctx, "a/")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("expected 2 keys, got %d", len(keys))
	}

	keys, err = store.ListByPrefix(ctx, "")
	if err != nil {
		t.Fatalf("ListByPrefix failed: %v", err)
	}
	if len(keys) != 3 {
		t.Fatalf("expected 3 keys, got %d", len(keys))
	}
}

func testHealthCheck(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.HealthCheck(ctx); err != nil {
		t.Fatalf("HealthCheck failed: %v", err)
	}
}

// testHealthcheckReport is the conformance test for the new
// Healthcheck (lowercase 'c') method that returns a health.Report.
// Implementations must populate Status correctly and stamp CheckedAt
// — without this assertion the conformance suite would silently
// accept a broken Healthcheck that returns a zero-value Report.
func testHealthcheckReport(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	rep := store.Healthcheck(ctx)
	if rep.Status != health.StatusHealthy {
		t.Fatalf("Healthcheck on fresh store: got status %q (%q), want healthy", rep.Status, rep.Message)
	}
	if rep.CheckedAt.IsZero() {
		t.Fatal("Healthcheck must populate CheckedAt")
	}
}

func testClosedOperations(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	if err := store.Close(); err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	if err := store.WriteBlock(ctx, "key", []byte("data")); err == nil {
		t.Error("WriteBlock should fail after Close")
	}

	var zero blockstore.ContentHash
	if err := store.WriteBlockWithHash(ctx, "key", zero, []byte("data")); err == nil {
		t.Error("WriteBlockWithHash should fail after Close")
	}

	if _, err := store.ReadBlock(ctx, "key"); err == nil {
		t.Error("ReadBlock should fail after Close")
	}

	if err := store.CopyBlock(ctx, "src", "dst"); err == nil {
		t.Error("CopyBlock should fail after Close")
	}
}

func testCopyBlock(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("source content")
	srcKey := "src/block-0"
	dstKey := "dst/block-0"

	if err := store.WriteBlock(ctx, srcKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	if err := store.CopyBlock(ctx, srcKey, dstKey); err != nil {
		t.Fatalf("CopyBlock failed: %v", err)
	}

	// Verify destination has the same data
	read, err := store.ReadBlock(ctx, dstKey)
	if err != nil {
		t.Fatalf("ReadBlock on destination failed: %v", err)
	}
	if !bytes.Equal(read, data) {
		t.Fatalf("destination data = %q, want %q", read, data)
	}

	// Verify source is unchanged
	read, err = store.ReadBlock(ctx, srcKey)
	if err != nil {
		t.Fatalf("ReadBlock on source failed: %v", err)
	}
	if !bytes.Equal(read, data) {
		t.Fatalf("source data changed after copy: got %q, want %q", read, data)
	}
}

func testCopyBlockNotFound(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	err := store.CopyBlock(ctx, "nonexistent/block-0", "dst/block-0")
	if err == nil {
		t.Fatal("CopyBlock should fail for nonexistent source")
	}
	if !errors.Is(err, blockstore.ErrBlockNotFound) {
		t.Fatalf("CopyBlock error = %v, want blockstore.ErrBlockNotFound", err)
	}
}

func testDataIsolation(t *testing.T, factory Factory) {
	store := factory(t)
	ctx := context.Background()

	data := []byte("original")
	blockKey := "test/block-0"

	if err := store.WriteBlock(ctx, blockKey, data); err != nil {
		t.Fatalf("WriteBlock failed: %v", err)
	}

	// Modify original
	data[0] = 'X'

	read, err := store.ReadBlock(ctx, blockKey)
	if err != nil {
		t.Fatalf("ReadBlock failed: %v", err)
	}
	if read[0] != 'o' {
		t.Fatalf("expected 'o' after mutation, got %c", read[0])
	}
}
