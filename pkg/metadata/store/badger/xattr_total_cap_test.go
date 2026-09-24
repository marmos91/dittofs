package badger

import (
	"context"
	"fmt"
	"strings"
	"testing"

	badgerdb "github.com/dgraph-io/badger/v4"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// These guard the per-file extended-attribute total against the same growth the
// manifest segmentation stops one key over. The attr blob embeds the EA map as
// JSON, so the EA set is what decides whether the f: value stays in the LSM —
// where compaction reclaims superseded copies — or crosses ValueThreshold into
// the value log, where every subsequent attr-only write appends a fresh copy of
// the whole blob and nothing ever reclaims it.
//
// Neither test asserts correctness: the uncapped store stored and returned every
// xattr exactly right while leaking. What they measure is the stored value's size
// and the bytes an attr-only write puts in the value log.

// attrBlobSize reports the encoded size of a file's f: value, which is what
// badger compares against ValueThreshold.
func attrBlobSize(t *testing.T, s *BadgerMetadataStore, id uuid.UUID) int64 {
	t.Helper()
	var size int64
	require.NoError(t, s.db.View(func(txn *badgerdb.Txn) error {
		item, err := txn.Get(keyFile(id))
		if err != nil {
			return err
		}
		size = item.ValueSize()
		return nil
	}))
	return size
}

// fillMaxSizeXattrs sets xattrs at the per-value ceiling one at a time until one
// is refused, returning how many landed and that refusal. Nothing else about the
// store is assumed: each call is the ordinary SetXattr path a client drives.
func fillMaxSizeXattrs(t *testing.T, s *BadgerMetadataStore, h metadata.FileHandle, limit int) (int, error) {
	t.Helper()
	ctx := context.Background()
	value := make([]byte, metadata.XattrInlineMaxBytes)
	for i := range value {
		value[i] = byte('a' + i%26)
	}
	for i := range limit {
		if err := s.SetXattr(ctx, h, fmt.Sprintf("user.a%06d", i), value); err != nil {
			return i, err
		}
	}
	return limit, nil
}

// smallXattrChain builds count mutations whose values are one byte each and whose
// names are long, so the set's cost is almost entirely names and JSON framing
// rather than value bytes. This is the regime a bound measured on value bytes
// alone cannot see.
func smallXattrChain(count int) ([]metadata.EAMutation, int) {
	muts := make([]metadata.EAMutation, count)
	valueBytes := 0
	for i := range muts {
		muts[i] = metadata.EAMutation{
			Name:  fmt.Sprintf("user.%s%06d", strings.Repeat("n", 180), i),
			Value: []byte{byte('a' + i%26)},
		}
		valueBytes += 1
	}
	return muts, valueBytes
}

// TestXattrTotalStaysUnderValueThreshold is the size guard. However many
// individually-legal xattrs a client writes, the resulting f: value must stay
// below ValueThreshold — that is what keeps the attr blob in the LSM.
func TestXattrTotalStaysUnderValueThreshold(t *testing.T) {
	ctx := context.Background()

	store, err := NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	threshold := store.BadgerOptions().ValueThreshold
	root := mkPayloadShare(t, store, "/s")
	t.Logf("ValueThreshold = %d bytes, XattrTotalMaxBytes = %d bytes",
		threshold, metadata.XattrTotalMaxBytes)

	// Regime one, the reported case: xattrs at the per-value ceiling, each of
	// them legal on its own, driven through SetXattr until one is refused.
	t.Run("MaxSizeValues", func(t *testing.T) {
		h := mkPayloadFile(t, store, "/s", root, "max.bin", "/max.bin", metadata.PayloadID("/max.bin"))
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(t, err)

		set, setErr := fillMaxSizeXattrs(t, store, h, 24)
		require.ErrorIs(t, setErr, metadata.ErrXattrTooLarge,
			"the set must be bounded; %d values at %d bytes were all accepted",
			set, metadata.XattrInlineMaxBytes)

		size := attrBlobSize(t, store, id)
		where := "LSM (inline)"
		if size >= threshold {
			where = "VALUE LOG"
		}
		t.Logf("%d values of %d bytes accepted, f: blob = %d bytes -> %s",
			set, metadata.XattrInlineMaxBytes, size, where)

		require.Less(t, size, threshold,
			"%d values of %d bytes put %d bytes in the f: value; at or above the %d-byte threshold it goes to the value log, which is the leak",
			set, metadata.XattrInlineMaxBytes, size, threshold)

		names, err := store.ListXattr(ctx, h)
		require.NoError(t, err)
		require.Len(t, names, set, "every accepted xattr must still be listed")
	})

	// Regime two: many tiny values. The SMB EA channel delivers a whole chain in
	// one call, so that is how it is driven here.
	t.Run("ManySmallValues", func(t *testing.T) {
		h := mkPayloadFile(t, store, "/s", root, "small.bin", "/small.bin", metadata.PayloadID("/small.bin"))
		_, id, err := metadata.DecodeFileHandle(h)
		require.NoError(t, err)

		file, err := store.GetFile(ctx, h)
		require.NoError(t, err)

		// A chain whose value bytes come nowhere near the bound, but whose names
		// and framing clear it several times over. A bound charged on value bytes
		// alone accepts this, which is why the bound is measured on the encoding.
		oversized, valueBytes := smallXattrChain(3000)
		require.Less(t, valueBytes, metadata.XattrTotalMaxBytes,
			"the point of this case is that its value bytes alone are legal")
		require.ErrorIs(t, file.ApplyEAMutations(oversized), metadata.ErrXattrTooLarge,
			"%d one-byte values carrying %d bytes of value must still be refused on their names and framing",
			len(oversized), valueBytes)
		require.Empty(t, file.EAs, "a refused chain must leave the set untouched")

		// A chain that fits must store, and the blob it produces must stay in the
		// LSM — the bound's whole purpose.
		fits, _ := smallXattrChain(1000)
		require.NoError(t, file.ApplyEAMutations(fits))
		require.NoError(t, store.UpdateAttrs(ctx, file))

		size := attrBlobSize(t, store, id)
		t.Logf("%d small values accepted, f: blob = %d bytes (threshold %d)", len(fits), size, threshold)
		require.Less(t, size, threshold,
			"a set at the bound put %d bytes in the f: value, at or above the %d-byte threshold",
			size, threshold)

		names, err := store.ListXattr(ctx, h)
		require.NoError(t, err)
		require.Len(t, names, len(fits))
	})
}

// TestXattrAttrOnlyWriteDoesNotGrowValueLog is the amplification guard: once a
// file carries as many xattrs as it is allowed, a chmod must cost what it changed
// rather than a fresh copy of the whole attr blob.
//
// A correctness assertion cannot stand in for this. The uncapped store served
// every xattr correctly while each chmod appended ~2 MiB to the value log against
// ~50 bytes of LSM pressure, so only counting bytes observes it.
func TestXattrAttrOnlyWriteDoesNotGrowValueLog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// A small value-log file size keeps any regression visible within the test's
	// budget rather than hidden inside one preallocated file.
	opts := badgerdb.DefaultOptions(dir).
		WithValueLogFileSize(16 << 20).
		WithLoggingLevel(badgerdb.WARNING)

	store, err := NewBadgerMetadataStore(ctx, BadgerMetadataStoreConfig{
		DBPath:        dir,
		BadgerOptions: &opts,
	})
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, store.Close()) })

	threshold := store.BadgerOptions().ValueThreshold
	root := mkPayloadShare(t, store, "/s")
	h := mkPayloadFile(t, store, "/s", root, "big.bin", "/big.bin", metadata.PayloadID("/big.bin"))
	_, id, err := metadata.DecodeFileHandle(h)
	require.NoError(t, err)

	// As many max-size xattrs as the file will take. Uncapped, 24 of these put
	// 2 MiB in the attr blob — that is the regime the defect lives in.
	set, setErr := fillMaxSizeXattrs(t, store, h, 24)

	// Logged, deliberately not asserted: uncapped this would fail here and the
	// value-log assertion below — the one this test exists for — would never be
	// reached. TestXattrTotalStaysUnderValueThreshold is what guards the threshold.
	t.Logf("%d of 24 xattrs accepted (refused with %v), f: blob = %d bytes (threshold %d)",
		set, setErr, attrBlobSize(t, store, id), threshold)

	before := dirBytes(t, dir)
	t.Logf("before chmods: %v", before)

	const chmods = 300
	for i := range chmods {
		file, err := store.GetFile(ctx, h)
		require.NoError(t, err)
		file.Mode = 0o600 | uint32(i%8)
		require.NoError(t, store.UpdateAttrs(ctx, file))
	}

	after := dirBytes(t, dir)
	t.Logf("after %d chmods: %v", chmods, after)

	vlogGrowth := after["vlog"] - before["vlog"]
	t.Logf("value log grew %d bytes over %d attr-only writes", vlogGrowth, chmods)

	// The attr blob never reaches the value log, so an attr-only write must not
	// push anything there. Uncapped, this grew by ~592 MiB.
	require.Zero(t, vlogGrowth,
		"an attr-only write must not write to the value log; it grew %d bytes", vlogGrowth)
}
